package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/jobs"
)

var noBackoff = durable.Backoff(func(int) time.Duration { return 0 })

type env struct {
	db     *dbtx.DB
	client *jobs.Client
}

func newEnv(t *testing.T, kinds ...string) *env {
	t.Helper()
	db := dbtx.New(durabletest.NewDB(t))
	c, err := jobs.NewClient(jobs.Config{DB: db, Kinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	return &env{db: db, client: c}
}

func (e *env) worker(t *testing.T, cfg jobs.WorkerConfig) *jobs.Worker {
	t.Helper()
	cfg.DB = e.db
	if cfg.Backoff == nil {
		cfg.Backoff = noBackoff
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 20 * time.Millisecond
	}
	w, err := jobs.NewWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (e *env) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.db.DB().ExecContext(t.Context(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func drain(t *testing.T, w *jobs.Worker) {
	t.Helper()
	for range 50 {
		n, err := w.RunOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("work did not drain")
}

// S10, S12, Q4: idempotent submission, transactions, kind allowlist.
func TestSubmit(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "email")
	a, err := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "email", Key: "k1", Payload: map[string]int{"user": 1}})
	if err != nil || !a.Created {
		t.Fatalf("first: %+v %v", a, err)
	}
	b, err := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "email", Key: "k1", Payload: json.RawMessage(`{"user": 1}`)})
	if err != nil || b.Created || b.ID != a.ID {
		t.Fatalf("replay: %+v %v", b, err)
	}
	if _, err := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "email", Key: "k1", Payload: map[string]int{"user": 2}}); !errors.Is(err, durable.ErrRequestMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
	if _, err := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "emial"}); !errors.Is(err, durable.ErrUnknownKind) {
		t.Fatalf("want unknown kind, got %v", err)
	}
	boom := errors.New("rollback")
	err = e.db.RunInTx(ctx, func(txCtx context.Context) error {
		if _, err := e.client.Submit(txCtx, jobs.SubmitRequest{Kind: "email", Key: "k2"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := e.client.GetByKey(ctx, "", "k2"); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("rolled-back submission is visible: %v", err)
	}
	strict, _ := jobs.NewClient(jobs.Config{DB: e.db, Kinds: []string{"email"}, RequireTx: true})
	if _, err := strict.Submit(ctx, jobs.SubmitRequest{Kind: "email"}); !errors.Is(err, durable.ErrNotInTx) {
		t.Fatalf("want ErrNotInTx, got %v", err)
	}
}

// Retries with backoff, then success; output stored; attempts audited.
func TestActivityRetryThenSucceed(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "flaky")
	w := e.worker(t, jobs.WorkerConfig{})
	var calls atomic.Int32
	_ = w.Register("flaky", func(ctx context.Context, x *jobs.Execution) (jobs.Result, error) {
		if calls.Add(1) < 3 {
			return jobs.Result{}, errors.New("transient")
		}
		return jobs.Done(map[string]int{"attempt": x.Attempt}), nil
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "flaky"})
	drain(t, w)
	j, err := e.client.Await(ctx, s.ID, 0)
	if err != nil || j.Status != jobs.StatusSucceeded || j.Attempts != 3 || string(j.Output) != `{"attempt": 3}` {
		t.Fatalf("%+v %v output=%s", j, err, j.Output)
	}
	var n int
	_ = e.db.DB().QueryRowContext(ctx, `SELECT count(*) FROM durable.execution_attempts WHERE execution_id = $1 AND outcome IS NOT NULL`, s.ID.String()).Scan(&n)
	if n != 3 {
		t.Fatalf("attempt rows = %d", n)
	}
}

// FM-6 + Q9: a panicking handler exhausts its attempts; compensation is queued.
func TestPoisonJobFailsAndCompensates(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "ship", "ship.compensate")
	w := e.worker(t, jobs.WorkerConfig{})
	_ = w.Register("ship", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		panic("boom")
	}, jobs.KindOptions{Compensate: true})
	var compensated atomic.Value
	_ = w.Register("ship.compensate", func(_ context.Context, x *jobs.Execution) (jobs.Result, error) {
		var in struct {
			ExecutionID string `json:"execution_id"`
			Error       string `json:"error"`
		}
		_ = x.Decode(&in)
		compensated.Store(in.ExecutionID)
		return jobs.Done(nil), nil
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "ship", MaxAttempts: 3})
	drain(t, w)
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusFailed || j.Attempts != 3 {
		t.Fatalf("%+v", j)
	}
	if compensated.Load() != s.ID.String() {
		t.Fatalf("compensation ran for %v", compensated.Load())
	}
}

func TestPermanentErrorFailsImmediately(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "bad")
	w := e.worker(t, jobs.WorkerConfig{})
	_ = w.Register("bad", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		return jobs.Result{}, durable.Permanent(errors.New("invalid payload"))
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "bad"})
	drain(t, w)
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusFailed || j.Attempts != 1 || j.LastError != "invalid payload" {
		t.Fatalf("%+v", j)
	}
}

// S13 (tx mode): checkpoints commit with the step's writes; a failing step
// rolls back its writes but records the failure.
func TestTxModeCheckpoints(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "order")
	e.exec(t, `CREATE TABLE ledger (step text)`)
	w := e.worker(t, jobs.WorkerConfig{})
	var failOnce atomic.Bool
	failOnce.Store(true)
	_ = w.Register("order", func(ctx context.Context, x *jobs.Execution) (jobs.Result, error) {
		q := e.db.DBTx(ctx)
		name, _ := x.Checkpoint()
		switch name {
		case "":
			_, err := q.ExecContext(ctx, `INSERT INTO ledger VALUES ('reserve')`)
			return jobs.Next("reserved", map[string]int{"reservation": 42}), err
		case "reserved":
			if _, err := q.ExecContext(ctx, `INSERT INTO ledger VALUES ('charge')`); err != nil {
				return jobs.Result{}, err
			}
			if failOnce.Swap(false) {
				return jobs.Result{}, errors.New("charge failed")
			}
			var cp struct{ Reservation int }
			_ = x.DecodeCheckpoint(&cp)
			return jobs.Done(map[string]int{"reservation": cp.Reservation}), nil
		}
		return jobs.Result{}, errors.New("unknown checkpoint")
	}, jobs.KindOptions{Mode: jobs.ModeTx})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "order"})
	drain(t, w)
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusSucceeded || string(j.Output) != `{"reservation": 42}` {
		t.Fatalf("%+v %s", j, j.Output)
	}
	var steps []string
	rows, _ := e.db.DB().QueryContext(ctx, `SELECT step FROM ledger ORDER BY step`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		steps = append(steps, s)
	}
	rows.Close()
	if len(steps) != 2 || steps[0] != "charge" || steps[1] != "reserve" {
		t.Fatalf("ledger = %v (failed step must have rolled back)", steps)
	}
}

func TestRunStepRejectedInTxMode(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "x")
	w := e.worker(t, jobs.WorkerConfig{})
	_ = w.Register("x", func(ctx context.Context, x *jobs.Execution) (jobs.Result, error) {
		_, err := x.RunStep(ctx, "call",
			func(context.Context) (int, error) { return 1, nil },
			func(context.Context, string, int) (int, error) { return 1, nil })
		return jobs.Result{}, err
	}, jobs.KindOptions{Mode: jobs.ModeTx})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "x"})
	drain(t, w)
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusFailed || j.Attempts != 1 {
		t.Fatalf("%+v", j)
	}
}

// fakeRemote is an idempotent API: one effect per idempotency key.
type fakeRemote struct {
	mu      sync.Mutex
	effects map[string]int
	calls   int
}

func (f *fakeRemote) charge(_ context.Context, key string, amount int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.effects == nil {
		f.effects = map[string]int{}
	}
	if _, ok := f.effects[key]; !ok {
		f.effects[key] = amount
	}
	return "ch_" + key, nil
}

// S14 + FM-2: the attempt loses its lease after the remote call but before
// saving the response; the retry resends the saved request with the same key.
func TestRunStepSurvivesLostLeaseAfterRemoteCall(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "charge")
	remote := &fakeRemote{}
	var once sync.Once
	w := e.worker(t, jobs.WorkerConfig{Hooks: &durable.Hooks{
		AfterRemoteCall: func(ctx context.Context, id, step string) {
			once.Do(func() {
				// Simulate a pause long enough for the lease to expire.
				e.exec(t, `UPDATE durable.executions SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id)
			})
		},
	}})
	var built atomic.Int32
	_ = w.Register("charge", func(ctx context.Context, x *jobs.Execution) (jobs.Result, error) {
		id, err := x.RunStep(ctx, "charge",
			func(context.Context) (int, error) { built.Add(1); return 100, nil },
			remote.charge)
		if err != nil {
			return jobs.Result{}, err
		}
		return jobs.Done(map[string]string{"charge": id}), nil
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "charge"})
	drain(t, w)
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusSucceeded || j.Attempts != 2 {
		t.Fatalf("%+v", j)
	}
	if built.Load() != 1 || remote.calls != 2 || len(remote.effects) != 1 {
		t.Fatalf("built=%d calls=%d effects=%v", built.Load(), remote.calls, remote.effects)
	}
	for k := range remote.effects {
		if k != s.ID.String()+":charge" {
			t.Fatalf("remote key %q", k)
		}
	}
}

// FM-1: worker A pauses before completing; its lease expires; worker B
// claims and completes; A's late completion is rejected.
func TestPausedWorkerCannotComplete(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "work")
	gate := durabletest.NewGate()
	defer gate.Open()
	a := e.worker(t, jobs.WorkerConfig{WorkerID: "A", LeaseDuration: 200 * time.Millisecond,
		Hooks: &durable.Hooks{BeforeComplete: func(context.Context, string) { gate.Wait() }}})
	b := e.worker(t, jobs.WorkerConfig{WorkerID: "B", LeaseDuration: 200 * time.Millisecond})
	h := func(name string) jobs.Handler {
		return func(context.Context, *jobs.Execution) (jobs.Result, error) { return jobs.Done(name), nil }
	}
	_ = a.Register("work", h("A"), jobs.KindOptions{})
	_ = b.Register("work", h("B"), jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "work"})
	aDone := make(chan struct{})
	go func() { defer close(aDone); _, _ = a.RunOnce(ctx) }()
	<-gate.Reached()
	time.Sleep(300 * time.Millisecond) // A is paused past its lease
	if n, err := b.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("B claimed %d: %v", n, err)
	}
	gate.Open()
	<-aDone
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusSucceeded || string(j.Output) != `"B"` || j.Attempts != 2 {
		t.Fatalf("%+v output=%s", j, j.Output)
	}
}

// S16: cancelling a running activity cancels the handler's context.
func TestCancelRunning(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "long")
	w := e.worker(t, jobs.WorkerConfig{LeaseDuration: 150 * time.Millisecond})
	started := make(chan struct{})
	var cause atomic.Value
	_ = w.Register("long", func(ctx context.Context, _ *jobs.Execution) (jobs.Result, error) {
		close(started)
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return jobs.Result{}, ctx.Err()
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "long"})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = w.RunOnce(ctx) }()
	<-started
	if st, err := e.client.Cancel(ctx, s.ID); err != nil || st != jobs.StatusRunning {
		t.Fatalf("cancel: %v %v", st, err)
	}
	<-done
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusCancelled || !errors.Is(cause.Load().(error), durable.ErrCancelled) {
		t.Fatalf("%+v cause=%v", j, cause.Load())
	}
	p, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "long", RunAt: time.Now().Add(time.Hour)})
	if st, _ := e.client.Cancel(ctx, p.ID); st != jobs.StatusCancelled {
		t.Fatalf("pending cancel: %v", st)
	}
}

// FM-31: shutdown hands unfinished work back without consuming an attempt.
func TestDrainHandsBack(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "slow")
	w := e.worker(t, jobs.WorkerConfig{DrainTimeout: 100 * time.Millisecond})
	started := make(chan struct{}, 1)
	_ = w.Register("slow", func(ctx context.Context, _ *jobs.Execution) (jobs.Result, error) {
		started <- struct{}{}
		<-ctx.Done()
		return jobs.Result{}, ctx.Err()
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "slow"})
	runCtx, stop := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- w.Run(runCtx) }()
	<-started
	if !w.Ready() {
		t.Fatal("worker not ready while running")
	}
	stop()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusPending || j.Attempts != 0 {
		t.Fatalf("%+v", j)
	}
}

// The janitor-side reap: a lease that expired on the final attempt fails.
func TestReapExpiredFinalAttempt(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "crash")
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "crash", MaxAttempts: 1})
	e.exec(t, `UPDATE durable.executions SET status = 'running', attempts = 1, owner_id = 'dead',
		lease_expires_at = now() - interval '1 second' WHERE id = $1`, s.ID.String())
	w := e.worker(t, jobs.WorkerConfig{})
	_ = w.Register("crash", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		t.Fatal("must not run")
		return jobs.Result{}, nil
	}, jobs.KindOptions{})
	drain(t, w)
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusFailed {
		t.Fatalf("%+v", j)
	}
}

// S13 (activity mode): Finalize commits business writes with completion.
func TestFinalize(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "pay")
	e.exec(t, `CREATE TABLE orders (id int, paid bool)`)
	e.exec(t, `INSERT INTO orders VALUES (1, false)`)
	w := e.worker(t, jobs.WorkerConfig{})
	_ = w.Register("pay", func(ctx context.Context, x *jobs.Execution) (jobs.Result, error) {
		err := x.Finalize(ctx, func(txCtx context.Context) (any, error) {
			_, err := e.db.DBTx(txCtx).ExecContext(txCtx, `UPDATE orders SET paid = true WHERE id = 1`)
			return "paid", err
		})
		return jobs.Result{}, err
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "pay"})
	drain(t, w)
	j, _ := e.client.Get(ctx, s.ID)
	var paid bool
	_ = e.db.DB().QueryRowContext(ctx, `SELECT paid FROM orders WHERE id = 1`).Scan(&paid)
	if j.Status != jobs.StatusSucceeded || string(j.Output) != `"paid"` || !paid {
		t.Fatalf("%+v paid=%v", j, paid)
	}
}

// S34 + Q2: bulk requeue grants extra attempts; purge respects live keys.
func TestAdmin(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "bad", "good")
	w := e.worker(t, jobs.WorkerConfig{})
	var fixed atomic.Bool
	_ = w.Register("bad", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		if fixed.Load() {
			return jobs.Done(nil), nil
		}
		return jobs.Result{}, durable.Permanent(errors.New("upstream 400"))
	}, jobs.KindOptions{})
	_ = w.Register("good", func(context.Context, *jobs.Execution) (jobs.Result, error) { return jobs.Done(nil), nil }, jobs.KindOptions{})
	for range 3 {
		_, _ = e.client.Submit(ctx, jobs.SubmitRequest{Kind: "bad", MaxAttempts: 2, PayloadVersion: 2})
	}
	keyed, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "good", Key: "g1"})
	drain(t, w)
	failed, err := e.client.ListFailed(ctx, jobs.Filter{Kind: "bad", ErrorContains: "400"})
	if err != nil || len(failed) != 3 {
		t.Fatalf("failed=%d %v", len(failed), err)
	}
	fixed.Store(true)
	if n, err := e.client.RequeueFailed(ctx, jobs.Filter{Kind: "bad"}); err != nil || n != 3 {
		t.Fatalf("requeued %d %v", n, err)
	}
	if v, _ := e.client.OldestPayloadVersions(ctx); v["bad"] != 2 {
		t.Fatalf("versions %v", v)
	}
	drain(t, w)
	j, _ := e.client.Get(ctx, failed[0].ID)
	if j.Status != jobs.StatusSucceeded || j.MaxAttempts != 3 {
		t.Fatalf("%+v", j)
	}
	n, err := e.client.Purge(ctx, time.Now().Add(time.Hour), 100)
	if err != nil || n != 3 {
		t.Fatalf("purged %d (keyed execution must stay while its key lives): %v", n, err)
	}
	if _, err := e.client.Get(ctx, keyed.ID); err != nil {
		t.Fatal(err)
	}
}

// Regression: a cancel request survives a retryable failure.
func TestCancelDuringRetryableFailure(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "x")
	w := e.worker(t, jobs.WorkerConfig{})
	var calls atomic.Int32
	_ = w.Register("x", func(_ context.Context, x *jobs.Execution) (jobs.Result, error) {
		calls.Add(1)
		if _, err := e.client.Cancel(context.Background(), x.ID); err != nil {
			t.Error(err)
		}
		return jobs.Result{}, errors.New("transient")
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "x"})
	drain(t, w)
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusCancelled || calls.Load() != 1 {
		t.Fatalf("%+v calls=%d", j, calls.Load())
	}
}

// Regression: a handler that finishes after the drain timeout is completed,
// not handed back.
func TestShutdownKeepsSuccessfulResult(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "x")
	w := e.worker(t, jobs.WorkerConfig{DrainTimeout: 50 * time.Millisecond})
	running := make(chan struct{})
	_ = w.Register("x", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		close(running)
		time.Sleep(200 * time.Millisecond) // ignores ctx and finishes
		return jobs.Done("ok"), nil
	}, jobs.KindOptions{})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "x"})
	rctx, cancel := context.WithCancel(ctx)
	go func() { <-running; cancel() }()
	if err := w.Run(rctx); err != nil {
		t.Fatal(err)
	}
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusSucceeded || string(j.Output) != `"ok"` {
		t.Fatalf("%+v", j)
	}
}

// Regression: workers only reap kinds they registered, so compensation is
// decided by a worker that knows the kind.
func TestReapOnlyOwnKinds(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "pay", "pay.compensate", "other")
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "pay", MaxAttempts: 1})
	e.exec(t, `UPDATE durable.executions SET status = 'running', attempts = 1, owner_id = 'dead',
		token = 1, lease_expires_at = now() - interval '1 second' WHERE id = $1`, s.ID.String())
	other := e.worker(t, jobs.WorkerConfig{})
	_ = other.Register("other", func(context.Context, *jobs.Execution) (jobs.Result, error) { return jobs.Done(nil), nil }, jobs.KindOptions{})
	drain(t, other)
	if j, _ := e.client.Get(ctx, s.ID); j.Status != jobs.StatusRunning {
		t.Fatalf("reaped by a worker without the kind: %+v", j)
	}
	payer := e.worker(t, jobs.WorkerConfig{})
	_ = payer.Register("pay", func(context.Context, *jobs.Execution) (jobs.Result, error) { return jobs.Done(nil), nil }, jobs.KindOptions{Compensate: true})
	if _, err := payer.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var comp int
	_ = e.db.DB().QueryRowContext(ctx, `SELECT count(*) FROM durable.executions WHERE kind = 'pay.compensate'`).Scan(&comp)
	if j, _ := e.client.Get(ctx, s.ID); j.Status != jobs.StatusFailed || comp != 1 {
		t.Fatalf("%+v compensations=%d", j, comp)
	}
}

// Regression (D8): retries keep the mode the execution started with.
func TestStoredModeGovernsRetries(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "x")
	first := e.worker(t, jobs.WorkerConfig{})
	_ = first.Register("x", func(context.Context, *jobs.Execution) (jobs.Result, error) {
		return jobs.Result{}, errors.New("transient")
	}, jobs.KindOptions{Mode: jobs.ModeActivity})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "x"})
	if _, err := first.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	second := e.worker(t, jobs.WorkerConfig{})
	var mode jobs.Mode
	_ = second.Register("x", func(_ context.Context, x *jobs.Execution) (jobs.Result, error) {
		mode = x.Mode
		return jobs.Done(nil), nil
	}, jobs.KindOptions{Mode: jobs.ModeTx})
	drain(t, second)
	if j, _ := e.client.Get(ctx, s.ID); j.Status != jobs.StatusSucceeded || mode != jobs.ModeActivity {
		t.Fatalf("%+v ran in mode %q", j, mode)
	}
}

// Regression: a tx-mode attempt timeout is recorded, not left to lease expiry.
func TestTxModeAttemptTimeoutRecorded(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, "x")
	w := e.worker(t, jobs.WorkerConfig{})
	_ = w.Register("x", func(ctx context.Context, _ *jobs.Execution) (jobs.Result, error) {
		<-ctx.Done()
		return jobs.Result{}, ctx.Err()
	}, jobs.KindOptions{Mode: jobs.ModeTx, Timeout: 100 * time.Millisecond})
	s, _ := e.client.Submit(ctx, jobs.SubmitRequest{Kind: "x", MaxAttempts: 1})
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	j, _ := e.client.Get(ctx, s.ID)
	if j.Status != jobs.StatusFailed || !strings.Contains(j.LastError, "attempt timed out") {
		t.Fatalf("%+v", j)
	}
}
