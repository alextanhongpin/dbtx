package inbox_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/inbox"
	"github.com/alextanhongpin/dbtx/postgres/durable/jobs"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository"
)

type events struct {
	mu  sync.Mutex
	all []durable.Event
}

func (e *events) Observe(_ context.Context, ev durable.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.all = append(e.all, ev)
}

func (e *events) of(t durable.EventType) []durable.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []durable.Event
	for _, ev := range e.all {
		if ev.Type == t {
			out = append(out, ev)
		}
	}
	return out
}

func setupCfg(t *testing.T, cfg inbox.Config) (*inbox.Inbox, *dbtx.DB) {
	t.Helper()
	db := dbtx.New(durabletest.NewDB(t))
	if _, err := db.DB().ExecContext(t.Context(), `CREATE TABLE projection (seq bigint)`); err != nil {
		t.Fatal(err)
	}
	cfg.DB = db
	in, err := inbox.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return in, db
}

// receiveAll delivers seqs to a sequenced stream and fails on any error.
func receiveAll(t *testing.T, in *inbox.Inbox, apply inbox.ApplyFunc, seqs ...int64) {
	t.Helper()
	for _, seq := range seqs {
		if _, err := in.Receive(t.Context(), inbox.ModeSequenced, seqMsg(seq), apply); err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
	}
}

// A poison buffered message is counted, observed, parked after MaxAttempts
// and no longer applied; Unpark retries it once it is fixed.
func TestPoisonBufferedMessageParks(t *testing.T) {
	ctx := t.Context()
	ev := &events{}
	in, db := setupCfg(t, inbox.Config{MaxAttempts: 3, Observer: ev})
	a := &applied{fail: map[int64]int{2: 100}}
	var calls atomic.Int32
	apply := func(txCtx context.Context, m inbox.Message) error {
		calls.Add(1)
		return a.apply(db)(txCtx, m)
	}
	// Seq 1 is applied; its drain fails on 2 but Receive still succeeds.
	receiveAll(t, in, apply, 2, 3, 1)
	if p := projected(t, db); fmt.Sprint(p) != "[1]" {
		t.Fatalf("projection %v", p)
	}
	if b := ev.of(durable.EventStreamBlocked); len(b) != 1 || b[0].Seq != 2 || b[0].Attempt != 1 || b[0].Err == nil {
		t.Fatalf("blocked events %+v", b)
	}

	_, err := in.Drain(ctx, "orders", "o1", apply)
	var de *inbox.DrainError
	if !errors.As(err, &de) || de.Seq != 2 || de.Attempts != 2 || de.Parked || errors.Is(err, inbox.ErrParked) {
		t.Fatalf("drain 2: %v", err)
	}
	_, err = in.Drain(ctx, "orders", "o1", apply)
	if !errors.As(err, &de) || de.Attempts != 3 || !de.Parked || !errors.Is(err, inbox.ErrParked) || de.Err.Error() != "apply failed" {
		t.Fatalf("drain 3: %v", err)
	}
	if p := ev.of(durable.EventStreamParked); len(p) != 1 || p[0].Seq != 2 || p[0].Attempt != 3 {
		t.Fatalf("parked events %+v", p)
	}

	// Parked: further drains and receives do not call apply for it.
	before := calls.Load()
	if _, err := in.Drain(ctx, "orders", "o1", apply); !errors.Is(err, inbox.ErrParked) {
		t.Fatalf("drain parked: %v", err)
	}
	receiveAll(t, in, apply, 4)
	if calls.Load() != before {
		t.Fatalf("apply called for a parked message")
	}

	parked, err := in.Parked(ctx, "", 0)
	if err != nil || len(parked) != 1 {
		t.Fatalf("parked %+v %v", parked, err)
	}
	if p := parked[0]; p.Seq != 2 || p.MessageID != "m2" || p.Attempts != 3 || p.LastError != "apply failed" || p.ParkedAt.IsZero() {
		t.Fatalf("parked %+v", p)
	}
	if other, _ := in.Parked(ctx, "other", 0); len(other) != 0 {
		t.Fatalf("source filter: %+v", other)
	}
	if gaps, _ := in.Gaps(ctx, 0); len(gaps) != 1 || gaps[0].NextBuffered != 2 {
		t.Fatalf("gaps %+v", gaps)
	}
	if _, _, _, err := in.SkipGap(ctx, "orders", "o1", apply); !errors.Is(err, durable.ErrInvalidArgument) {
		t.Fatalf("skip gap without gap: %v", err)
	}

	// Fixed: Unpark retries and drains the rest.
	a.mu.Lock()
	a.fail[2] = 0
	a.mu.Unlock()
	n, err := in.Unpark(ctx, "orders", "o1", 2, apply)
	if err != nil || n != 3 {
		t.Fatalf("unpark: %d %v", n, err)
	}
	if p := projected(t, db); fmt.Sprint(p) != "[1 2 3 4]" {
		t.Fatalf("projection %v", p)
	}
	if parked, _ := in.Parked(ctx, "", 0); len(parked) != 0 {
		t.Fatalf("still parked %+v", parked)
	}
	if _, err := in.Unpark(ctx, "orders", "o1", 2, apply); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("unpark applied: %v", err)
	}
}

// Discard skips a poison message; its redelivery is a duplicate.
func TestDiscardParked(t *testing.T) {
	ctx := t.Context()
	ev := &events{}
	in, db := setupCfg(t, inbox.Config{MaxAttempts: 1, Observer: ev})
	a := &applied{fail: map[int64]int{2: 100}}
	receiveAll(t, in, a.apply(db), 3, 2, 1)
	if parked, _ := in.Parked(ctx, "orders", 10); len(parked) != 1 || parked[0].Seq != 2 {
		t.Fatalf("parked %+v", parked)
	}
	if _, err := in.Discard(ctx, "orders", "o1", 3, a.apply(db)); !errors.Is(err, durable.ErrInvalidArgument) {
		t.Fatalf("discard not next: %v", err)
	}
	n, err := in.Discard(ctx, "orders", "o1", 2, a.apply(db))
	if err != nil || n != 1 {
		t.Fatalf("discard: %d %v", n, err)
	}
	if p := projected(t, db); fmt.Sprint(p) != "[1 3]" {
		t.Fatalf("projection %v", p)
	}
	if res, err := in.Receive(ctx, inbox.ModeSequenced, seqMsg(2), a.apply(db)); err != nil || res != inbox.Duplicate {
		t.Fatalf("redelivery: %v %v", res, err)
	}
	if g := ev.of(durable.EventInboxGap); len(g) == 0 || g[len(g)-1].Seq != 2 {
		t.Fatalf("gap events %+v", g)
	}
	if _, err := in.Discard(ctx, "orders", "o1", 4, a.apply(db)); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("discard missing: %v", err)
	}
}

// Messages applied before a drain failure stay committed, as does the
// failure record; the error reaches the caller.
func TestDrainPartialProgressCommits(t *testing.T) {
	ctx := t.Context()
	in, db := setupCfg(t, inbox.Config{})
	a := &applied{fail: map[int64]int{4: 2}}
	receiveAll(t, in, a.apply(db), 2, 3, 4, 5)
	// Gap at 1: SkipGap applies 2 and 3, then fails on 4 but commits.
	from, to, n, err := in.SkipGap(ctx, "orders", "o1", a.apply(db))
	var de *inbox.DrainError
	if from != 1 || to != 1 || n != 2 || !errors.As(err, &de) || de.Seq != 4 || de.Attempts != 1 {
		t.Fatalf("skip %d..%d applied %d: %v", from, to, n, err)
	}
	if p := projected(t, db); fmt.Sprint(p) != "[2 3]" {
		t.Fatalf("projection %v", p)
	}
	_, err = in.Drain(ctx, "orders", "o1", a.apply(db))
	if !errors.As(err, &de) || de.Attempts != 2 {
		t.Fatalf("drain: %v", err)
	}
	if n, err := in.Drain(ctx, "orders", "o1", a.apply(db)); err != nil || n != 2 {
		t.Fatalf("drain: %d %v", n, err)
	}
	if p := projected(t, db); fmt.Sprint(p) != "[2 3 4 5]" {
		t.Fatalf("projection %v", p)
	}
}

// A cancelled context is not counted as an attempt, and nothing commits.
func TestDrainCancelledNotCounted(t *testing.T) {
	in, db := setupCfg(t, inbox.Config{MaxAttempts: 1})
	a := &applied{}
	receiveAll(t, in, a.apply(db), 3)
	ctx, cancel := context.WithCancel(t.Context())
	cancelling := func(context.Context, inbox.Message) error { cancel(); return context.Canceled }
	if _, _, _, err := in.SkipGap(ctx, "orders", "o1", cancelling); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	if parked, _ := in.Parked(t.Context(), "", 0); len(parked) != 0 {
		t.Fatalf("cancellation parked a message: %+v", parked)
	}
	from, to, n, err := in.SkipGap(t.Context(), "orders", "o1", a.apply(db))
	if err != nil || from != 1 || to != 2 || n != 1 {
		t.Fatalf("skip %d..%d applied %d: %v", from, to, n, err)
	}
}

// Dedup failures belong to the broker (Receive) or to jobs (Enqueue): a
// failed job is listed, found from the message and requeued.
func TestEnqueueFailedJobRequeue(t *testing.T) {
	ctx := t.Context()
	db := dbtx.New(durabletest.NewDB(t))
	jc, err := jobs.NewClient(jobs.Config{DB: db, Kinds: []string{"catalog.reindex"}})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := inbox.New(inbox.Config{DB: db, Jobs: jc})
	m := inbox.Message{Source: "catalog", MessageID: "e1", Payload: []byte(`{"track":9}`)}
	if _, _, err := in.Enqueue(ctx, m, jobs.SubmitRequest{Kind: "catalog.reindex", MaxAttempts: 2}); err != nil {
		t.Fatal(err)
	}
	var broken atomic.Bool
	broken.Store(true)
	var attempts atomic.Int32
	w, err := jobs.NewWorker(jobs.WorkerConfig{DB: db, PollInterval: 10 * time.Millisecond,
		Backoff: func(int) time.Duration { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Register("catalog.reindex", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		attempts.Add(1)
		if broken.Load() {
			return jobs.Result{}, errors.New("search index down")
		}
		return jobs.Done(nil), nil
	}, jobs.KindOptions{})
	run := func() {
		for range 20 {
			if n, err := w.RunOnce(ctx); err != nil {
				t.Fatal(err)
			} else if n == 0 {
				j, _ := in.Job(ctx, "catalog", "e1")
				if j.Status.Terminal() {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	run()
	j, err := in.Job(ctx, "catalog", "e1")
	if err != nil || j.Status != jobs.StatusFailed || j.Attempts != 2 || attempts.Load() != 2 {
		t.Fatalf("%+v %v (handler ran %d)", j, err, attempts.Load())
	}
	failed, err := jc.ListFailed(ctx, jobs.Filter{Kind: "catalog.reindex", ErrorContains: "index down"})
	if err != nil || len(failed) != 1 || failed[0].ID != j.ID {
		t.Fatalf("failed %+v %v", failed, err)
	}
	// Redelivery stays a duplicate even though the job failed.
	if res, _, err := in.Enqueue(ctx, m, jobs.SubmitRequest{Kind: "catalog.reindex"}); err != nil || res != inbox.Duplicate {
		t.Fatalf("redelivery %v %v", res, err)
	}
	broken.Store(false)
	if err := jc.Requeue(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	run()
	if j, _ := in.Job(ctx, "catalog", "e1"); j.Status != jobs.StatusSucceeded {
		t.Fatalf("after requeue %+v", j)
	}
	if _, err := in.Job(ctx, "catalog", "nope"); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
	noJobs, _ := inbox.New(inbox.Config{DB: db})
	if _, err := noJobs.Job(ctx, "catalog", "e1"); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("no jobs: %v", err)
	}
}

// Ported: concurrent deliveries of one message apply it once.
func TestDedupConcurrent(t *testing.T) {
	ctx := t.Context()
	in, db := setup(t)
	a := &applied{}
	m := inbox.Message{Source: "psp", MessageID: "evt_1", Seq: 1}
	var wg sync.WaitGroup
	results := make([]inbox.Result, 8)
	for n := range results {
		wg.Go(func() {
			res, err := in.Receive(ctx, inbox.ModeDedup, m, a.apply(db))
			if err != nil {
				t.Error(err)
			}
			results[n] = res
		})
	}
	wg.Wait()
	if c := slices.Index(results, inbox.Applied); c < 0 || len(projected(t, db)) != 1 {
		t.Fatalf("results %v projection %v", results, projected(t, db))
	}
}

// Ported: Receive joins the caller's transaction; a rollback forgets it.
func TestReceiveJoinsCallerTx(t *testing.T) {
	ctx := t.Context()
	in, db := setup(t)
	a := &applied{}
	m := inbox.Message{Source: "psp", MessageID: "evt_1", Seq: 1}
	boom := errors.New("caller failed")
	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		if res, err := in.Receive(txCtx, inbox.ModeDedup, m, a.apply(db)); err != nil || res != inbox.Applied {
			t.Fatalf("%v %v", res, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if res, err := in.Receive(ctx, inbox.ModeDedup, m, a.apply(db)); err != nil || res != inbox.Applied {
		t.Fatalf("after rollback: %v %v", res, err)
	}
}

// Ported: purge removes old dedup records only.
func TestPurgeMessages(t *testing.T) {
	ctx := t.Context()
	in, db := setup(t)
	a := &applied{}
	for n := range 3 {
		m := inbox.Message{Source: "psp", MessageID: fmt.Sprint("evt", n), Seq: int64(n)}
		if _, err := in.Receive(ctx, inbox.ModeDedup, m, a.apply(db)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := in.PurgeMessages(ctx, "psp", time.Now().Add(-time.Hour), 0); err != nil || n != 0 {
		t.Fatalf("purged recent: %d %v", n, err)
	}
	if n, err := in.PurgeMessages(ctx, "psp", time.Now().Add(time.Hour), 2); err != nil || n != 2 {
		t.Fatalf("purged: %d %v", n, err)
	}
	if n, _ := in.PurgeMessages(ctx, "psp", time.Now().Add(time.Hour), 0); n != 1 {
		t.Fatalf("purged rest: %d", n)
	}
}

// Ported: invalid configuration and arguments.
func TestValidation(t *testing.T) {
	ctx := t.Context()
	if _, err := inbox.New(inbox.Config{}); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatal(err)
	}
	in, db := setup(t)
	if _, err := inbox.New(inbox.Config{DB: db, MaxAttempts: -1}); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatal(err)
	}
	a := &applied{}
	for _, c := range []struct {
		mode inbox.Mode
		m    inbox.Message
	}{
		{inbox.ModeDedup, inbox.Message{Source: "s"}},
		{inbox.ModeSequenced, inbox.Message{Source: "s", MessageID: "1", AggregateKey: "k"}},
		{inbox.ModeVersioned, inbox.Message{Source: "s", MessageID: "1"}},
		{"bogus", inbox.Message{Source: "s", MessageID: "1"}},
	} {
		if _, err := in.Receive(ctx, c.mode, c.m, a.apply(db)); !errors.Is(err, durable.ErrInvalidArgument) {
			t.Fatalf("%v %+v: %v", c.mode, c.m, err)
		}
	}
	if _, err := in.Drain(ctx, "", "k", a.apply(db)); !errors.Is(err, durable.ErrInvalidArgument) {
		t.Fatal(err)
	}
	if _, err := in.Drain(ctx, "s", "k", nil); !errors.Is(err, durable.ErrInvalidArgument) {
		t.Fatal(err)
	}
	if _, _, err := in.Enqueue(ctx, inbox.Message{Source: "s", MessageID: "1"}, jobs.SubmitRequest{}); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatal(err)
	}
}

// The numbered migrations apply cleanly on top of the schema (and to each
// other), so existing installs pick up the inbox failure columns.
func TestMigrationsApply(t *testing.T) {
	db := durabletest.NewDB(t)
	names, err := fs.Glob(repository.Migrations, "migrations/*.sql")
	if err != nil || len(names) < 2 {
		t.Fatalf("%v %v", names, err)
	}
	for _, n := range names {
		b, _ := fs.ReadFile(repository.Migrations, n)
		if _, err := db.ExecContext(t.Context(), string(b)); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `SELECT attempts, last_error, failed_at, parked_at FROM durable.inbox_pending`); err != nil {
		t.Fatal(err)
	}
}
