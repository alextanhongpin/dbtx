package jobs_test

// Tests ported from the removed standalone postgres/jobs module (shutdown,
// reaping, archive, input limits, repository edge cases), adapted to the
// durable API.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/jobs"
)

func (e *env) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.DB().QueryRowContext(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A claim that commits while shutdown begins must still run the execution,
// not leave it leased and unrun so its only attempt is spent.
func TestShutdownDuringClaim(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "once")
	w := e.worker(t, jobs.WorkerConfig{})
	var calls atomic.Int32
	_ = w.Register("once", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		calls.Add(1)
		return jobs.Done("ran"), nil
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "once", MaxAttempts: 1})
	runCtx, stop := context.WithCancel(ctx)
	jobs.SetAfterClaimHook(w, stop) // shut down after the claim committed
	if err := w.Run(runCtx); err != nil {
		t.Fatal(err)
	}
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusSucceeded || j.Attempts != 1 || calls.Load() != 1 {
		t.Fatalf("%+v calls=%d", j, calls.Load())
	}
}

// slowFinal starts a final-attempt execution on a worker whose completion is
// paused past its lease, returning the execution and a func that releases it.
func slowFinal(t *testing.T, e *env) (uuid.UUID, func()) {
	t.Helper()
	gate := durabletest.NewGate()
	t.Cleanup(gate.Open)
	a := e.worker(t, jobs.WorkerConfig{LeaseDuration: 100 * time.Millisecond,
		Hooks: &durable.Hooks{BeforeComplete: func(context.Context, string) { gate.Wait() }}})
	_ = a.Register("slow", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		return jobs.Done("late"), nil
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(t.Context(), jobs.SubmitRequest{Kind: "slow", MaxAttempts: 1})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = a.RunOnce(t.Context()) }()
	<-gate.Reached()
	time.Sleep(250 * time.Millisecond) // the lease has expired
	return s.ID, func() { gate.Open(); <-done }
}

func reaper(t *testing.T, e *env, grace time.Duration) *jobs.Worker {
	w := e.worker(t, jobs.WorkerConfig{ReapGrace: grace})
	_ = w.Register("slow", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		t.Error("must not run")
		return jobs.Result{}, nil
	}, jobs.KindOptions{})
	return w
}

// Within ReapGrace an expired final attempt is left alone, and its owner's
// late success is recorded because nobody reclaimed it.
func TestReapGraceKeepsSlowSuccess(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "slow")
	id, release := slowFinal(t, e)
	if _, err := reaper(t, e, 10*time.Second).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if j, _ := e.client.Get(ctx, id); j.Status != jobs.StatusRunning {
		t.Fatalf("reaped within grace: %+v", j)
	}
	release()
	j, _ := e.client.Get(ctx, id)
	if j.Status != jobs.StatusSucceeded || string(j.Output) != `"late"` {
		t.Fatalf("%+v output=%s", j, j.Output)
	}
}

// After ReapGrace the execution is failed and the late completion rejected.
func TestReapAfterGrace(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "slow")
	id, release := slowFinal(t, e)
	if _, err := reaper(t, e, time.Millisecond).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	release()
	j, _ := e.client.Get(ctx, id)
	if j.Status != jobs.StatusFailed || j.Output != nil || !strings.Contains(j.LastError, "lease expired") {
		t.Fatalf("%+v output=%s", j, j.Output)
	}
}

// A handler that outlives a lease its heartbeats could not renew, ignores
// cancellation and succeeds still has its result recorded when nobody
// reclaimed the execution.
func TestSuccessAfterLocalLeaseLoss(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "x")
	w := e.worker(t, jobs.WorkerConfig{LeaseDuration: 90 * time.Millisecond,
		Hooks: &durable.Hooks{BeforeHeartbeat: func(ctx context.Context, _ string) { <-ctx.Done() }}})
	var cause atomic.Value
	_ = w.Register("x", func(ctx context.Context, _ *jobs.Execution) (jobs.Result, error) {
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		time.Sleep(100 * time.Millisecond) // past the lease
		return jobs.Done("kept"), nil
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "x", MaxAttempts: 1})
	drain(t, w)
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusSucceeded || string(j.Output) != `"kept"` ||
		!errors.Is(cause.Load().(error), durable.ErrLeaseLost) {
		t.Fatalf("%+v cause=%v", j, cause.Load())
	}
}

// A reclaimed execution cancels the stale handler, which writes nothing.
func TestLeaseLostCancelsHandler(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "x")
	w := e.worker(t, jobs.WorkerConfig{LeaseDuration: 150 * time.Millisecond})
	started := make(chan uuid.UUID, 1)
	var cause atomic.Value
	_ = w.Register("x", func(ctx context.Context, x *jobs.Execution) (jobs.Result, error) {
		started <- x.ID
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return jobs.Done("stale"), nil
	}, jobs.KindOptions{})
	_, _ = e.client.Submit(ctx, jobs.SubmitRequest{Kind: "x"})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = w.RunOnce(ctx) }()
	id := <-started
	e.exec(t, `UPDATE durable.executions SET token = token + 1000 WHERE id = $1`, id)
	<-done
	j, _ := e.client.Get(ctx, id)
	if j.Status != jobs.StatusRunning || j.Output != nil || !errors.Is(cause.Load().(error), durable.ErrLeaseLost) {
		t.Fatalf("%+v cause=%v", j, cause.Load())
	}
}

// Heartbeats keep a handler running for several lease lengths.
func TestHeartbeatsKeepHandlerRunning(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "long")
	w := e.worker(t, jobs.WorkerConfig{LeaseDuration: 120 * time.Millisecond})
	_ = w.Register("long", func(ctx context.Context, _ *jobs.Execution) (jobs.Result, error) {
		select {
		case <-ctx.Done():
			return jobs.Result{}, context.Cause(ctx)
		case <-time.After(500 * time.Millisecond):
			return jobs.Done(nil), nil
		}
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "long", MaxAttempts: 1})
	drain(t, w)
	if j, _ := e.client.Get(ctx, s.ID); j.Status != jobs.StatusSucceeded {
		t.Fatalf("%+v", j)
	}
}

// A handler that ignores cancellation makes Run return ErrShutdownTimeout.
func TestShutdownTimeout(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "stuck")
	w := e.worker(t, jobs.WorkerConfig{DrainTimeout: 50 * time.Millisecond, FinalizeTimeout: 100 * time.Millisecond})
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	_ = w.Register("stuck", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		close(started)
		<-release
		return jobs.Done(nil), nil
	}, jobs.KindOptions{})
	_, _ = e.client.Submit(ctx, jobs.SubmitRequest{Kind: "stuck"})
	runCtx, stop := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- w.Run(runCtx) }()
	<-started
	stop()
	select {
	case err := <-errc:
		if !errors.Is(err, jobs.ErrShutdownTimeout) {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestSubmitLimits(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "k")
	long := strings.Repeat("x", 256)
	for _, req := range []jobs.SubmitRequest{{Kind: "k", Key: long}, {Kind: "k", Key: "a", Scope: long}} {
		if _, err := e.client.Submit(ctx, req); !errors.Is(err, durable.ErrInvalidArgument) {
			t.Fatalf("long key/scope: %v", err)
		}
	}
	if _, err := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "k", Key: long[:255]}); err != nil {
		t.Fatalf("255-byte key: %v", err)
	}
	small, _ := jobs.NewClient(jobs.Config{DB: e.db, Kinds: []string{"k"}, MaxPayloadBytes: 16})
	if _, err := small.Submit(ctx, jobs.SubmitRequest{Kind: "k", Payload: map[string]string{"data": long}}); !errors.Is(err, durable.ErrInvalidArgument) {
		t.Fatalf("large payload: %v", err)
	}
	if _, err := small.Submit(ctx, jobs.SubmitRequest{Kind: "k", Payload: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatalf("small payload: %v", err)
	}
	if _, err := small.Submit(ctx, jobs.SubmitRequest{Kind: "k", Payload: func() {}}); !errors.Is(err, durable.ErrInvalidArgument) {
		t.Fatalf("unencodable payload: %v", err)
	}
	unlimited, _ := jobs.NewClient(jobs.Config{DB: e.db, Kinds: []string{"k"}, MaxPayloadBytes: -1, MaxKeyLength: -1})
	if _, err := unlimited.Submit(ctx, jobs.SubmitRequest{Kind: "k", Key: long + long, Payload: map[string]string{"data": long}}); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	if _, err := jobs.NewClient(jobs.Config{DB: e.db, Kinds: []string{"k"}, KeyRetention: -time.Second}); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("negative KeyRetention: %v", err)
	}
}

// Archive keeps the hot table small while keys keep deduplicating.
func TestArchive(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "work", "bad")
	w := e.worker(t, jobs.WorkerConfig{})
	_ = w.Register("work", func(context.Context, *jobs.Execution) (jobs.Result, error) { return jobs.Done("out"), nil }, jobs.KindOptions{})
	var failed atomic.Bool
	_ = w.Register("bad", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		if failed.CompareAndSwap(false, true) {
			return jobs.Result{}, errors.New("flaky")
		}
		return jobs.Result{}, durable.Permanent(errors.New("broken"))
	}, jobs.KindOptions{})
	keyed, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "work", Key: "a1", Payload: map[string]int{"n": 1}})
	plain, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "bad"})
	pending, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "work", RunAt: time.Now().Add(time.Hour)})
	drain(t, w)
	if n, err := e.client.Archive(ctx, time.Now().Add(-time.Hour), 10); err != nil || n != 0 {
		t.Fatalf("archived recent executions: %d %v", n, err)
	}
	if n, err := e.client.Archive(ctx, time.Now().Add(time.Hour), 10); err != nil || n != 2 {
		t.Fatalf("archived %d: %v", n, err)
	}
	if n := e.count(t, `SELECT count(*) FROM durable.executions`); n != 1 {
		t.Fatalf("hot table has %d rows, want only the pending one", n)
	}
	if j, err := e.client.Get(ctx, pending.ID); err != nil || j.ArchivedAt != nil {
		t.Fatalf("pending: %+v %v", j, err)
	}
	j, err := e.client.Get(ctx, keyed.ID)
	if err != nil || j.ArchivedAt == nil || j.Status != jobs.StatusSucceeded || string(j.Output) != `"out"` || j.FinishedAt == nil {
		t.Fatalf("archived: %+v %v", j, err)
	}
	if j, err := e.client.GetByKey(ctx, "", "a1"); err != nil || j.ID != keyed.ID {
		t.Fatalf("by key: %+v %v", j, err)
	}
	// The key still deduplicates and answers from the archive.
	r, err := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "work", Key: "a1", Payload: map[string]int{"n": 1}})
	if err != nil || r.Created || r.ID != keyed.ID || r.Status != jobs.StatusSucceeded || string(r.Output) != `"out"` {
		t.Fatalf("replay: %+v %v", r, err)
	}
	if _, err := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "work", Key: "a1", Payload: map[string]int{"n": 2}}); !errors.Is(err, durable.ErrRequestMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	f, _ := e.client.Get(ctx, plain.ID)
	if f.Status != jobs.StatusFailed || f.LastError != "broken" || f.Attempts != 2 {
		t.Fatalf("failed: %+v", f)
	}
	var log []map[string]any
	var raw []byte
	if err := e.db.DB().QueryRowContext(ctx, `SELECT attempt_log FROM durable.executions_archive WHERE id = $1`, plain.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &log); err != nil || len(log) != 2 || log[0]["outcome"] != "retry" || log[1]["outcome"] != "failed" {
		t.Fatalf("attempt log %s: %v", raw, err)
	}
	if err := e.client.Requeue(ctx, plain.ID); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("requeue archived: %v", err)
	}
	// Archived rows go only once their key has expired.
	if n, err := e.client.PurgeArchive(ctx, time.Now().Add(time.Hour), 10); err != nil || n != 1 {
		t.Fatalf("purged %d: %v", n, err)
	}
	if _, err := e.client.Get(ctx, plain.ID); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("purged execution: %v", err)
	}
	e.exec(t, `UPDATE durable.idempotency_keys SET expires_at = now() - interval '1 second' WHERE key = 'a1'`)
	if n, err := e.client.PurgeArchive(ctx, time.Now().Add(time.Hour), 10); err != nil || n != 1 {
		t.Fatalf("purged %d after key expiry: %v", n, err)
	}
}

func TestJanitorArchives(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "work")
	w := e.worker(t, jobs.WorkerConfig{})
	_ = w.Register("work", func(context.Context, *jobs.Execution) (jobs.Result, error) { return jobs.Done(nil), nil }, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "work", Key: "j1"})
	drain(t, w)
	e.exec(t, `UPDATE durable.executions SET finished_at = now() - interval '2 hours'`)
	jctx, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stop()
	j := &jobs.Janitor{Client: e.client, Interval: 20 * time.Millisecond, Retention: time.Hour, Archive: true}
	if err := j.Run(jctx); err != nil {
		t.Fatal(err)
	}
	if got, err := e.client.Get(ctx, s.ID); err != nil || got.ArchivedAt == nil {
		t.Fatalf("%+v %v", got, err)
	}
}

// Concurrent submissions of one key create exactly one execution.
func TestConcurrentSubmitSameKey(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "k")
	const n = 10
	var (
		wg      sync.WaitGroup
		created atomic.Int32
		mu      sync.Mutex
		ids     = map[uuid.UUID]bool{}
	)
	for range n {
		wg.Go(func() {
			r, err := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "k", Key: "same", Payload: map[string]int{"a": 1}})
			if err != nil {
				t.Error(err)
				return
			}
			if r.Created {
				created.Add(1)
			}
			mu.Lock()
			ids[r.ID] = true
			mu.Unlock()
		})
	}
	wg.Wait()
	if created.Load() != 1 || len(ids) != 1 || e.count(t, `SELECT count(*) FROM durable.executions`) != 1 {
		t.Fatalf("created=%d ids=%d", created.Load(), len(ids))
	}
}

// Payloads are compared as JSON values: key order and whitespace do not matter.
func TestSubmitReplayIgnoresJSONFormatting(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "k")
	a, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "k", Key: "f", Payload: json.RawMessage(`{"a":1,"b":[1,2]}`)})
	b, err := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "k", Key: "f", Payload: json.RawMessage(" {\"b\": [1, 2],\n \"a\": 1} ")})
	if err != nil || b.Created || b.ID != a.ID {
		t.Fatalf("%+v %v", b, err)
	}
}

// Many workers over many executions run each exactly once.
func TestConcurrentWorkersRunEachOnce(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "c")
	const jobsN, workersN = 40, 4
	var mu sync.Mutex
	runs := map[uuid.UUID]int{}
	h := func(_ context.Context, x *jobs.Execution) (jobs.Result, error) {
		mu.Lock()
		runs[x.ID]++
		mu.Unlock()
		return jobs.Done(nil), nil
	}
	for range jobsN {
		if _, err := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "c"}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range workersN {
		w := e.worker(t, jobs.WorkerConfig{Concurrency: 3})
		_ = w.Register("c", h, jobs.KindOptions{})
		wg.Go(func() {
			for {
				n, err := w.RunOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	if len(runs) != jobsN {
		t.Fatalf("ran %d of %d", len(runs), jobsN)
	}
	for id, n := range runs {
		if n != 1 {
			t.Fatalf("%s ran %d times", id, n)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM durable.executions WHERE status = 'succeeded' AND attempts = 1`); n != jobsN {
		t.Fatalf("succeeded=%d", n)
	}
}

// Claims respect run_at and leave leased or exhausted executions alone.
func TestClaimEdgeCases(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "c")
	w := e.worker(t, jobs.WorkerConfig{ReapGrace: time.Hour})
	var ran atomic.Int32
	_ = w.Register("c", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		ran.Add(1)
		return jobs.Done(nil), nil
	}, jobs.KindOptions{})
	_, _ = e.client.Submit(ctx, jobs.SubmitRequest{Kind: "c", RunAt: time.Now().Add(time.Hour)})
	leased, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "c"})
	e.exec(t, `UPDATE durable.executions SET status = 'running', attempts = 1, owner_id = 'other', token = 999999,
		lease_expires_at = now() + interval '1 hour' WHERE id = $1`, leased.ID)
	exhausted, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "c", MaxAttempts: 2})
	e.exec(t, `UPDATE durable.executions SET status = 'running', attempts = 2, owner_id = 'dead', token = 1,
		lease_expires_at = now() - interval '1 minute' WHERE id = $1`, exhausted.ID)
	if n, err := w.RunOnce(ctx); err != nil || n != 0 || ran.Load() != 0 {
		t.Fatalf("claimed %d: %v", n, err)
	}
	// An expired lease with attempts left is reclaimed with a new token.
	e.exec(t, `UPDATE durable.executions SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, leased.ID)
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("reclaimed %d: %v", n, err)
	}
	j, _ := e.client.Get(ctx, leased.ID)
	if j.Status != jobs.StatusSucceeded || j.Attempts != 2 || j.Token == 999999 {
		t.Fatalf("%+v", j)
	}
	if j, _ := e.client.Get(ctx, exhausted.ID); j.Status != jobs.StatusRunning {
		t.Fatalf("exhausted execution changed within the reap grace: %+v", j)
	}
}
