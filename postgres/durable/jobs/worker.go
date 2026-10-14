package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/engine"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

var (
	// ErrShutdownTimeout is returned by Run when handlers did not return
	// within DrainTimeout + FinalizeTimeout after shutdown began.
	ErrShutdownTimeout = errors.New("jobs: handlers still running after shutdown timeout")
	errShutdown        = errors.New("jobs: worker shutting down")
	errAttemptTimeout  = errors.New("jobs: attempt timed out")
)

// WorkerConfig is copied at construction. Zero values select defaults.
type WorkerConfig struct {
	DB              *dbtx.DB
	Queue           string        // default "default"
	WorkerID        string        // default host-pid-random
	Concurrency     int           // executions in flight; default 4
	PollInterval    time.Duration // idle polling; default 1s
	LeaseDuration   time.Duration // default 30s; heartbeat every third in activity mode
	RequestTimeout  time.Duration // per heartbeat/database call; default 5s
	StepTimeout     time.Duration // bounds a tx-mode step or Finalize transaction; default 30s
	DrainTimeout    time.Duration // how long in-flight work may finish after shutdown; default 25s
	FinalizeTimeout time.Duration // budget to record an outcome; default 10s
	KeyRetention    time.Duration // for compensation submissions; default 7 days
	Backoff         durable.Backoff
	Observer        durable.Observer
	Hooks           *durable.Hooks
}

// KindOptions configure one registered kind.
type KindOptions struct {
	Mode          Mode          // default ModeActivity
	Timeout       time.Duration // per attempt; 0 means none
	Concurrency   int           // per worker; 0 means limited only by the worker
	LeaseDuration time.Duration // default WorkerConfig.LeaseDuration
	Backoff       durable.Backoff
	// Compensate submits an execution of kind Kind+".compensate" in the same
	// transaction that records a final failure (Q9). Its payload is
	// {"execution_id", "kind", "payload", "error"}. Register that kind too.
	Compensate bool
}

type registration struct {
	kind    string
	handler Handler
	opts    KindOptions
	running atomic.Int32
}

// Worker claims and runs executions for registered kinds.
type Worker struct {
	cfg   WorkerConfig
	mu    sync.Mutex
	kinds []*registration
	next  int
	ready atomic.Bool
}

func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.DB == nil || cfg.DB.Unwrap() == nil {
		return nil, fmt.Errorf("%w: DB is required", durable.ErrInvalidConfig)
	}
	if cfg.Queue == "" {
		cfg.Queue = "default"
	}
	if cfg.WorkerID == "" {
		host, _ := os.Hostname()
		cfg.WorkerID = fmt.Sprintf("%s-%d-%s", host, os.Getpid(), rand.Text()[:8])
	}
	setDefault(&cfg.Concurrency, 4)
	setDefault(&cfg.PollInterval, time.Second)
	setDefault(&cfg.LeaseDuration, 30*time.Second)
	setDefault(&cfg.RequestTimeout, 5*time.Second)
	setDefault(&cfg.StepTimeout, 30*time.Second)
	setDefault(&cfg.DrainTimeout, 25*time.Second)
	setDefault(&cfg.FinalizeTimeout, 10*time.Second)
	setDefault(&cfg.KeyRetention, 7*24*time.Hour)
	if cfg.Backoff == nil {
		cfg.Backoff = durable.DefaultBackoff
	}
	if cfg.Concurrency < 0 || cfg.LeaseDuration < 6*time.Millisecond {
		return nil, fmt.Errorf("%w: Concurrency >= 1 and LeaseDuration >= 6ms", durable.ErrInvalidConfig)
	}
	return &Worker{cfg: cfg}, nil
}

func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

// Register adds a handler for kind. Register every kind before Run.
func (w *Worker) Register(kind string, h Handler, opts KindOptions) error {
	if kind == "" || h == nil {
		return fmt.Errorf("%w: kind and handler are required", durable.ErrInvalidArgument)
	}
	if opts.Mode == "" {
		opts.Mode = ModeActivity
	}
	if opts.Mode != ModeActivity && opts.Mode != ModeTx {
		return fmt.Errorf("%w: mode %q", durable.ErrInvalidArgument, opts.Mode)
	}
	setDefault(&opts.LeaseDuration, w.cfg.LeaseDuration)
	if opts.Backoff == nil {
		opts.Backoff = w.cfg.Backoff
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if slices.ContainsFunc(w.kinds, func(r *registration) bool { return r.kind == kind }) {
		return fmt.Errorf("%w: kind %q already registered", durable.ErrInvalidArgument, kind)
	}
	w.kinds = append(w.kinds, &registration{kind: kind, handler: h, opts: opts})
	return nil
}

// Ready reports whether the worker is polling (for readiness probes).
func (w *Worker) Ready() bool { return w.ready.Load() }

// Run polls and runs executions until ctx is cancelled, then drains: it stops
// claiming, lets in-flight work finish for DrainTimeout, then cancels it and
// hands unfinished activity-mode executions back for immediate retry.
func (w *Worker) Run(ctx context.Context) error {
	if len(w.kinds) == 0 {
		return fmt.Errorf("%w: no kinds registered", durable.ErrInvalidConfig)
	}
	runCtx, cancelRun := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancelRun(nil)
	slots := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	for ctx.Err() == nil {
		_ = w.reap(ctx)
		free := cap(slots) - len(slots)
		claimed := 0
		if free > 0 {
			jobs, err := w.claim(ctx, free)
			if err == nil {
				w.ready.Store(true)
			}
			for _, e := range jobs {
				slots <- struct{}{}
				wg.Go(func() {
					defer func() { <-slots }()
					w.process(runCtx, ctx, e)
				})
			}
			claimed = len(jobs)
		}
		if free > 0 && claimed == free {
			continue
		}
		t := time.NewTimer(w.cfg.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	w.ready.Store(false)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(w.cfg.DrainTimeout):
	}
	cancelRun(errShutdown)
	select {
	case <-done:
		return nil
	case <-time.After(w.cfg.FinalizeTimeout):
		return ErrShutdownTimeout
	}
}

// RunOnce reaps, claims up to Concurrency executions, runs them, and returns
// how many ran. Intended for tests and cron-style draining.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	if err := w.reap(ctx); err != nil {
		return 0, err
	}
	jobs, err := w.claim(ctx, w.cfg.Concurrency)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, e := range jobs {
		wg.Go(func() { w.process(ctx, ctx, e) })
	}
	wg.Wait()
	return len(jobs), nil
}

type claimed struct {
	exec *Execution
	reg  *registration
	sent time.Time
}

// claim takes up to limit executions, respecting per-kind concurrency, and
// rotating the starting kind for fairness.
func (w *Worker) claim(ctx context.Context, limit int) ([]claimed, error) {
	w.mu.Lock()
	regs := slices.Clone(w.kinds)
	start := w.next
	w.next++
	w.mu.Unlock()
	var out []claimed
	for i := range regs {
		if limit <= 0 {
			break
		}
		reg := regs[(start+i)%len(regs)]
		n := limit
		if reg.opts.Concurrency > 0 {
			n = min(n, reg.opts.Concurrency-int(reg.running.Load()))
		}
		if n <= 0 {
			continue
		}
		got, err := w.claimKind(ctx, reg, n)
		if err != nil {
			return out, err
		}
		out = append(out, got...)
		limit -= len(got)
	}
	return out, nil
}

func (w *Worker) claimKind(ctx context.Context, reg *registration, limit int) ([]claimed, error) {
	owner := w.cfg.WorkerID + ":" + rand.Text()[:10]
	sent := time.Now()
	ctx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
	defer cancel()
	q := postgres.New(w.cfg.DB.DB())
	rows, err := q.ClaimExecutions(ctx, postgres.ClaimExecutionsParams{
		Queue: w.cfg.Queue, Kind: reg.kind, Lim: int32(limit),
		OwnerID: owner, TtlUs: pg.Micros(reg.opts.LeaseDuration),
	})
	if err != nil {
		return nil, err
	}
	var out []claimed
	for _, g := range rows {
		a, err := q.StartAttempt(ctx, postgres.StartAttemptParams{
			Mode: string(reg.opts.Mode), ID: g.ID,
			OwnerID: sql.NullString{String: owner, Valid: true}, Token: g.Token,
		})
		if err != nil {
			// The lease is granted; it will expire and the execution is retried.
			return out, err
		}
		reg.running.Add(1)
		out = append(out, claimed{reg: reg, sent: sent, exec: &Execution{
			ID: a.ID, Queue: a.Queue, Kind: a.Kind, Scope: a.Scope.String, Key: a.Key.String,
			Payload: a.Payload.Raw(), PayloadVersion: int(a.PayloadVersion), Mode: Mode(a.Mode.String),
			Attempt: int(a.Attempts), MaxAttempts: int(a.MaxAttempts), Token: a.Token,
			checkpointName: a.CheckpointName, checkpointData: a.CheckpointData.Raw(),
			w: w, owner: owner,
		}})
	}
	return out, nil
}

// reap finishes running executions of registered kinds whose lease expired
// on their final attempt (or that were cancelled), in one transaction with
// compensation.
func (w *Worker) reap(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
	defer cancel()
	return w.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(w.cfg.DB.DBTx(txCtx))
		rows, err := q.ExpiredExecutions(txCtx, postgres.ExpiredExecutionsParams{
			Queue: w.cfg.Queue, Kinds: pg.JSON(w.kindsJSON()),
		})
		if err != nil {
			return err
		}
		for _, r := range rows {
			row := engine.Row{Table: executionsTable, Running: string(StatusRunning),
				ID: r.ID, Owner: r.OwnerID.String, Token: r.Token}
			status, reason, typ := StatusFailed, "lease expired on final attempt", durable.EventFailed
			if r.CancelRequested {
				status, reason, typ = StatusCancelled, "", durable.EventCancelled
			}
			ok, err := row.Expire(txCtx, q, string(status), reason)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := q.SetExecutionFinished(txCtx, postgres.SetExecutionFinishedParams{ID: r.ID}); err != nil {
				return err
			}
			durable.Observe(ctx, w.cfg.Observer, durable.Event{Type: typ, Component: "jobs",
				Kind: r.Kind, Key: r.ID.String(), Attempt: int(r.Attempts), Err: errors.New(reason)})
			if reg := w.registration(r.Kind); reg != nil && reg.opts.Compensate && status == StatusFailed {
				if err := w.compensate(txCtx, r.ID.String(), r.Kind, r.Payload.Raw(), r.LastError); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// kindsJSON lists registered kinds: workers only reap executions they could
// have run, so compensation is decided by a worker that knows the kind.
func (w *Worker) kindsJSON() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make([]string, len(w.kinds))
	for i, r := range w.kinds {
		names[i] = r.kind
	}
	b, _ := json.Marshal(names)
	return string(b)
}

func (w *Worker) registration(kind string) *registration {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.kinds {
		if r.kind == kind {
			return r
		}
	}
	return nil
}

func (w *Worker) compensate(txCtx context.Context, id, kind string, payload json.RawMessage, lastErr string) error {
	_, err := submit(txCtx, w.cfg.DB, w.cfg.Queue, w.cfg.KeyRetention, SubmitRequest{
		Kind:  kind + ".compensate",
		Scope: "jobs.compensate",
		Key:   id,
		Payload: map[string]any{
			"execution_id": id, "kind": kind, "payload": payload, "error": lastErr,
		},
	})
	return err
}

func safeCall(h Handler, ctx context.Context, e *Execution) (r Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("jobs: handler panic: %v", p)
		}
	}()
	return h(ctx, e)
}

func (w *Worker) process(runCtx, ctx context.Context, c claimed) {
	defer c.reg.running.Add(-1)
	attemptCtx := runCtx
	if c.reg.opts.Timeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeoutCause(runCtx, c.reg.opts.Timeout, errAttemptTimeout)
		defer cancel()
	}
	// The stored mode governs (D8): a deploy that changes a kind's mode
	// only affects executions that have not started yet.
	if c.exec.Mode == ModeTx {
		w.processTx(attemptCtx, ctx, c)
		return
	}
	w.processActivity(attemptCtx, ctx, c)
}

func (w *Worker) outcomeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), w.cfg.FinalizeTimeout)
}

func (w *Worker) processActivity(attemptCtx, ctx context.Context, c claimed) {
	e, reg := c.exec, c.reg
	ttl := reg.opts.LeaseDuration
	e.keeper = engine.NewKeeper(ttl, w.cfg.RequestTimeout, c.sent, func(rctx context.Context) (bool, error) {
		q := postgres.New(w.cfg.DB.DB())
		ok, err := e.row().Renew(rctx, q, ttl)
		if err != nil || !ok {
			return ok, err
		}
		if cancelReq, err := q.ExecutionCancelRequested(rctx, e.ID); err == nil && cancelReq {
			e.keeper.CancelWork(durable.ErrCancelled)
		}
		return true, nil
	}, func(hctx context.Context) { w.cfg.Hooks.CallBeforeHeartbeat(hctx, e.ID.String()) })
	defer e.keeper.Close()

	var result Result
	fnErr, cause := e.keeper.Supervise(attemptCtx, e.ID.String(), func(workCtx context.Context) error {
		for {
			r, err := safeCall(reg.handler, workCtx, e)
			if err != nil || e.isFinalized() || !r.checkpoint {
				result = r
				return err
			}
			if err := e.locked(workCtx, func(txCtx context.Context, q *postgres.Queries) error {
				return w.saveCheckpoint(txCtx, q, e, r)
			}); err != nil {
				return err
			}
			w.cfg.Hooks.CallBetweenSteps(workCtx, e.ID.String(), r.name)
		}
	})
	if e.isFinalized() {
		w.observe(ctx, durable.EventSucceeded, e, nil)
		return
	}
	if terminal := e.keeper.Err(); terminal != nil {
		w.observe(ctx, durable.EventLeaseLost, e, terminal)
		return
	}
	octx, cancel := w.outcomeCtx(ctx)
	defer cancel()
	switch {
	case fnErr == nil:
		output, err := pg.Marshal(result.output)
		if err != nil {
			w.finishTx(octx, e, reg, func(txCtx context.Context, q *postgres.Queries) error {
				return w.recordFailure(txCtx, q, e, reg, durable.Permanent(fmt.Errorf("jobs: output: %w", err)))
			})
			return
		}
		w.cfg.Hooks.CallBeforeComplete(octx, e.ID.String())
		w.finishTx(octx, e, reg, func(txCtx context.Context, q *postgres.Queries) error { return w.complete(txCtx, q, e, output) })
	case durable.IsPermanent(fnErr):
		w.finishTx(octx, e, reg, func(txCtx context.Context, q *postgres.Queries) error {
			return w.recordFailure(txCtx, q, e, reg, fnErr)
		})
	case errors.Is(cause, durable.ErrCancelled):
		w.finishTx(octx, e, reg, func(txCtx context.Context, q *postgres.Queries) error { return w.recordCancelled(txCtx, q, e) })
	case errors.Is(cause, errShutdown):
		w.finishTx(octx, e, reg, func(txCtx context.Context, q *postgres.Queries) error { return w.handBack(txCtx, q, e) })
	default:
		if errors.Is(cause, errAttemptTimeout) {
			fnErr = errors.Join(errAttemptTimeout, fnErr)
		}
		w.finishTx(octx, e, reg, func(txCtx context.Context, q *postgres.Queries) error {
			return w.recordFailure(txCtx, q, e, reg, fnErr)
		})
	}
}

// finishTx records an outcome in its own transaction.
func (w *Worker) finishTx(ctx context.Context, e *Execution, reg *registration, fn func(context.Context, *postgres.Queries) error) {
	err := w.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		return fn(txCtx, postgres.New(w.cfg.DB.DBTx(txCtx)))
	})
	if errors.Is(err, durable.ErrLeaseLost) {
		w.observe(ctx, durable.EventLeaseLost, e, err)
	}
}

func (w *Worker) processTx(attemptCtx, ctx context.Context, c claimed) {
	e, reg := c.exec, c.reg
	db := w.cfg.DB
	for {
		var (
			r        Result
			finished bool
		)
		stepCtx, cancel := context.WithTimeout(attemptCtx, w.cfg.StepTimeout)
		err := db.RunInTx(stepCtx, func(txCtx context.Context) error {
			q := postgres.New(db.DBTx(txCtx))
			if err := q.SetLocalTimeouts(txCtx, pg.Timeout(w.cfg.StepTimeout)); err != nil {
				return err
			}
			// Renewing inside the step transaction also takes the row lock,
			// which keeps successors out until the step commits.
			renewed, err := e.row().Renew(txCtx, q, reg.opts.LeaseDuration)
			if err != nil {
				return err
			}
			if !renewed {
				return durable.ErrLeaseLost
			}
			cancelReq, err := q.ExecutionCancelRequested(txCtx, e.ID)
			if err != nil {
				return err
			}
			e.inLockedTx = true
			defer func() { e.inLockedTx = false }()
			if cancelReq {
				finished = true
				return w.recordCancelled(txCtx, q, e)
			}
			herr := db.RunInSubTx(txCtx, func(subCtx context.Context) error {
				var err error
				r, err = safeCall(reg.handler, subCtx, e)
				return err
			})
			if herr != nil {
				finished = true
				return w.recordFailure(txCtx, q, e, reg, herr)
			}
			if r.checkpoint {
				return w.saveCheckpoint(txCtx, q, e, r)
			}
			output, err := pg.Marshal(r.output)
			if err != nil {
				finished = true
				return w.recordFailure(txCtx, q, e, reg, durable.Permanent(fmt.Errorf("jobs: output: %w", err)))
			}
			w.cfg.Hooks.CallBeforeComplete(txCtx, e.ID.String())
			finished = true
			return w.complete(txCtx, q, e, output)
		})
		cancel()
		if err != nil {
			// The step rolled back. If our own context ended it (attempt
			// timeout or shutdown), record that in a fresh fenced transaction;
			// otherwise the lease was lost or the commit failed, and the
			// execution is retried once its lease expires.
			octx, ocancel := w.outcomeCtx(ctx)
			switch cause := context.Cause(attemptCtx); {
			case errors.Is(cause, errShutdown):
				w.finishTx(octx, e, reg, func(txCtx context.Context, q *postgres.Queries) error { return w.handBack(txCtx, q, e) })
			case errors.Is(cause, errAttemptTimeout):
				w.finishTx(octx, e, reg, func(txCtx context.Context, q *postgres.Queries) error {
					return w.recordFailure(txCtx, q, e, reg, errors.Join(errAttemptTimeout, err))
				})
			default:
				w.observe(ctx, durable.EventLeaseLost, e, err)
			}
			ocancel()
			return
		}
		if finished {
			return
		}
		w.cfg.Hooks.CallBetweenSteps(ctx, e.ID.String(), r.name)
	}
}

// saveCheckpoint writes a checkpoint; the caller holds the row lock.
func (w *Worker) saveCheckpoint(ctx context.Context, q *postgres.Queries, e *Execution, r Result) error {
	data, err := pg.Marshal(r.data)
	if err != nil {
		return durable.Permanent(fmt.Errorf("jobs: checkpoint: %w", err))
	}
	if err := q.SaveCheckpoint(ctx, postgres.SaveCheckpointParams{Name: r.name, Data: data, ID: e.ID}); err != nil {
		return err
	}
	// In tx mode this becomes visible only if the step commits; a failed
	// commit ends processing, so updating early is safe.
	e.checkpointName, e.checkpointData = r.name, data
	return nil
}

func (w *Worker) finishAttempt(ctx context.Context, q *postgres.Queries, e *Execution, outcome, errText string) error {
	return q.FinishAttempt(ctx, postgres.FinishAttemptParams{
		Outcome:     sql.NullString{String: outcome, Valid: true},
		Error:       sql.NullString{String: errText, Valid: errText != ""},
		ExecutionID: e.ID, Attempt: int32(e.Attempt),
	})
}

// lockOutcome takes the row lock before an outcome is written, so a cancel
// request cannot slip in between reading it and writing the status. Inside a
// tx-mode step or Finalize the lock is already held and expiry is not
// re-checked (D7).
func (w *Worker) lockOutcome(ctx context.Context, q *postgres.Queries, e *Execution) (cancelRequested bool, err error) {
	held, err := e.row().Lock(ctx, q, !e.inLockedTx)
	if err != nil {
		return false, err
	}
	if !held {
		return false, durable.ErrLeaseLost
	}
	return q.ExecutionCancelRequested(ctx, e.ID)
}

// end finishes the lease under the lock taken by lockOutcome.
func (w *Worker) end(ctx context.Context, q *postgres.Queries, e *Execution, f engine.Finish) error {
	ok, err := e.row().Finish(ctx, q, f)
	if err != nil {
		return err
	}
	if !ok {
		return durable.ErrLeaseLost
	}
	return nil
}

// complete marks success. It is idempotent: if an earlier COMMIT of this same
// attempt already succeeded, it returns nil (FM-7).
func (w *Worker) complete(ctx context.Context, q *postgres.Queries, e *Execution, output []byte) error {
	if _, err := w.lockOutcome(ctx, q, e); err != nil {
		if errors.Is(err, durable.ErrLeaseLost) {
			if st, err2 := q.ExecutionStatusToken(ctx, e.ID); err2 == nil &&
				st.Status == string(StatusSucceeded) && st.Token == e.Token {
				return nil
			}
		}
		return err
	}
	if err := w.end(ctx, q, e, engine.Finish{Status: string(StatusSucceeded), LastError: engine.Ptr("")}); err != nil {
		return err
	}
	if err := q.SetExecutionFinished(ctx, postgres.SetExecutionFinishedParams{Output: output, ID: e.ID}); err != nil {
		return err
	}
	w.observe(ctx, durable.EventSucceeded, e, nil)
	return w.finishAttempt(ctx, q, e, "succeeded", "")
}

func (w *Worker) recordFailure(ctx context.Context, q *postgres.Queries, e *Execution, reg *registration, herr error) error {
	cancelReq, err := w.lockOutcome(ctx, q, e)
	if err != nil {
		return err
	}
	msg := herr.Error()
	if cancelReq {
		return w.cancelled(ctx, q, e, msg)
	}
	retry := !durable.IsPermanent(herr) && !errors.Is(herr, durable.ErrRemoteStepInTx) && e.Attempt < e.MaxAttempts
	if retry {
		delay := durable.NextDelay(herr, e.Attempt, reg.opts.Backoff)
		if err := w.end(ctx, q, e, engine.Finish{Status: string(StatusPending), Delay: &delay, LastError: &msg}); err != nil {
			return err
		}
		durable.Observe(ctx, w.cfg.Observer, durable.Event{Type: durable.EventRetryScheduled, Component: "jobs",
			Kind: e.Kind, Key: e.ID.String(), Token: e.Token, Attempt: e.Attempt, RetryAt: time.Now().Add(delay), Err: herr})
		return w.finishAttempt(ctx, q, e, "retry", msg)
	}
	if err := w.end(ctx, q, e, engine.Finish{Status: string(StatusFailed), LastError: &msg}); err != nil {
		return err
	}
	if err := q.SetExecutionFinished(ctx, postgres.SetExecutionFinishedParams{ID: e.ID}); err != nil {
		return err
	}
	w.observe(ctx, durable.EventFailed, e, herr)
	if reg.opts.Compensate {
		if err := w.compensate(ctx, e.ID.String(), e.Kind, e.Payload, msg); err != nil {
			return err
		}
	}
	return w.finishAttempt(ctx, q, e, "failed", msg)
}

func (w *Worker) recordCancelled(ctx context.Context, q *postgres.Queries, e *Execution) error {
	if _, err := w.lockOutcome(ctx, q, e); err != nil {
		return err
	}
	return w.cancelled(ctx, q, e, "")
}

func (w *Worker) cancelled(ctx context.Context, q *postgres.Queries, e *Execution, msg string) error {
	f := engine.Finish{Status: string(StatusCancelled)}
	if msg != "" {
		f.LastError = &msg
	}
	if err := w.end(ctx, q, e, f); err != nil {
		return err
	}
	if err := q.SetExecutionFinished(ctx, postgres.SetExecutionFinishedParams{ID: e.ID}); err != nil {
		return err
	}
	w.observe(ctx, durable.EventCancelled, e, nil)
	return w.finishAttempt(ctx, q, e, "cancelled", msg)
}

// handBack returns an interrupted execution for immediate retry without
// consuming an attempt (or cancels it if a cancel was requested meanwhile).
func (w *Worker) handBack(ctx context.Context, q *postgres.Queries, e *Execution) error {
	cancelReq, err := w.lockOutcome(ctx, q, e)
	if err != nil {
		return err
	}
	if cancelReq {
		return w.cancelled(ctx, q, e, "")
	}
	if err := w.end(ctx, q, e, engine.Finish{Status: string(StatusPending), Delay: engine.Ptr(time.Duration(0)), AttemptDelta: -1}); err != nil {
		return err
	}
	w.observe(ctx, durable.EventHandedBack, e, nil)
	return w.finishAttempt(ctx, q, e, "handed_back", "")
}

func (w *Worker) observe(ctx context.Context, t durable.EventType, e *Execution, err error) {
	durable.Observe(ctx, w.cfg.Observer, durable.Event{Type: t, Component: "jobs", Kind: e.Kind,
		Key: e.ID.String(), Token: e.Token, Attempt: e.Attempt, Err: err})
}
