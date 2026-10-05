package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"runtime/debug"
	"sync"
	"time"
	"uuid"
)

// Handler does the actual work for one job. Requirements:
//   - Honour ctx: it is cancelled when the lease is lost or on forced shutdown.
//   - Be idempotent: delivery is at-least-once. Use job.ID as the idempotency
//     key for downstream calls, or pass job.FencingToken and reject stale ones.
//   - Return Permanent(err) for errors that retrying cannot fix, and
//     RetryAfter(err, d) to override the default backoff.
//
// The returned bytes are stored as the job's response (empty is stored as
// null). A response that cannot be stored as jsonb fails the job permanently.
type Handler func(ctx context.Context, job Job) (json.RawMessage, error)

// WorkerConfig tunes the polling loop. Zero values get sane defaults.
type WorkerConfig struct {
	WorkerID         string        // unique per instance (default host-pid-random)
	Concurrency      int           // max jobs in flight (default 4)
	BatchSize        int           // max jobs claimed per poll (default = Concurrency)
	PollInterval     time.Duration // sleep when the queue is empty (default 1s)
	LeaseDuration    time.Duration // lease length; heartbeat every third (default 30s)
	HeartbeatTimeout time.Duration // per-heartbeat DB timeout (default LeaseDuration/6)
	FinalizeTimeout  time.Duration // budget to record Complete/Fail/Retry (default 10s)
	ShutdownGrace    time.Duration // how long in-flight jobs may run after stop (default 30s)

	// Backoff maps the attempt number just made (1-based) to a retry delay.
	Backoff func(attempt int32) time.Duration
}

func (c *WorkerConfig) applyDefaults() {
	if c.WorkerID == "" {
		host, _ := os.Hostname()
		c.WorkerID = fmt.Sprintf("%s-%d-%s", host, os.Getpid(), uuid.New().String()[:8])
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.BatchSize <= 0 || c.BatchSize > c.Concurrency {
		c.BatchSize = c.Concurrency
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 30 * time.Second
	}
	if c.HeartbeatTimeout <= 0 {
		c.HeartbeatTimeout = c.LeaseDuration / 6
	}
	if c.FinalizeTimeout <= 0 {
		c.FinalizeTimeout = 10 * time.Second
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 30 * time.Second
	}
	if c.Backoff == nil {
		c.Backoff = DefaultBackoff
	}
}

// DefaultBackoff: 5s, 10s, 20s ... capped at 15m, with jitter in [d/2, d] so
// retries from many failed jobs do not synchronise.
func DefaultBackoff(attempt int32) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := 5 * time.Second << uint(min(attempt-1, 10))
	d = min(d, 15*time.Minute)
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// Worker is the consumer-side use case: claim, run, heartbeat, finalize.
type Worker struct {
	repo    Repository
	handler Handler
	cfg     WorkerConfig
	log     *slog.Logger
}

func NewWorker(repo Repository, handler Handler, cfg WorkerConfig, log *slog.Logger) *Worker {
	cfg.applyDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Worker{repo: repo, handler: handler, cfg: cfg, log: log.With("worker_id", cfg.WorkerID)}
}

// Run polls until ctx is cancelled, then stops claiming and lets in-flight
// jobs finish for up to ShutdownGrace. After the grace period their contexts
// are cancelled (cause ErrShutdown) and they are released for fast retry.
// Run returns once everything has drained.
func (w *Worker) Run(ctx context.Context) error {
	if w.repo.IsTx(ctx) {
		return ErrTxInContext
	}
	// In-flight jobs must NOT die the instant ctx is cancelled, so they run
	// on a context detached from it; we cancel it ourselves after the grace.
	workCtx, cancelWork := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancelWork(nil)

	sem := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	claimFailures := 0

	for ctx.Err() == nil {
		n := acquireSlots(ctx, sem, w.cfg.BatchSize)
		if n == 0 {
			break // ctx cancelled while waiting for a free slot
		}

		if ctx.Err() != nil {
			for range n {
				<-sem
			}
			break
		}

		claimedAt := time.Now() // taken BEFORE the call: a conservative lease start
		claimed, err := w.claim(ctx, n)
		for i := len(claimed); i < n; i++ {
			<-sem // give back slots we did not use
		}
		if err != nil {
			// The claim may have committed anyway, e.g. on a timeout; those
			// jobs wait out their lease and are picked up again.
			claimFailures++
			w.log.Error("claim failed", "err", err, "consecutive_failures", claimFailures)
			sleepCtx(ctx, DefaultBackoff(int32(min(claimFailures, 6))))
			continue
		}
		claimFailures = 0

		for _, job := range claimed {
			wg.Go(func() {
				defer func() { <-sem }()
				w.process(workCtx, job, claimedAt)
			})
		}

		if len(claimed) < n { // queue is (nearly) drained
			sleepCtx(ctx, jitter(w.cfg.PollInterval))
		}
	}

	// Drain.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.cfg.ShutdownGrace):
		w.log.Warn("shutdown grace elapsed; cancelling in-flight jobs")
		cancelWork(ErrShutdown)
		<-done
	}
	return nil
}

// claim leases up to n jobs. It does not use ctx for the call: if ctx were
// cancelled after the claim committed, the leased jobs would be lost, and each
// would use up an attempt without running. Jobs claimed during shutdown run
// like any other in-flight job, within ShutdownGrace.
func (w *Worker) claim(ctx context.Context, n int) ([]Job, error) {
	// A claim slower than this hands out jobs whose lease is mostly gone.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.LeaseDuration/3)
	defer cancel()
	return w.repo.FindAvailableJobs(ctx, FindAvailableJobsParams{
		WorkerID:     w.cfg.WorkerID,
		LeaseSeconds: w.cfg.LeaseDuration.Seconds(),
		Limit:        int32(n),
	})
}

// process runs one claimed job end to end.
func (w *Worker) process(workCtx context.Context, job Job, claimedAt time.Time) {
	log := w.log.With("job_id", job.ID, "attempt", job.Attempts, "fencing_token", job.FencingToken)

	jobCtx, cancel := context.WithCancelCause(workCtx)
	defer cancel(nil)

	// Keep the lease alive while the handler runs.
	hbCtx, stopHB := context.WithCancel(jobCtx)
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		w.heartbeatLoop(hbCtx, cancel, job, claimedAt.Add(w.cfg.LeaseDuration), log)
	}()

	resp, herr := w.safeHandle(jobCtx, job, log)

	stopHB()
	<-hbDone
	cause := context.Cause(jobCtx) // nil unless lease lost / forced shutdown

	if errors.Is(cause, ErrLeaseLost) {
		// Someone else owns the job; any write would be rejected. Drop it.
		log.Warn("lease lost; abandoning result", "handler_err", herr)
		return
	}

	// Finalize on a context that survives shutdown, with its own deadline:
	// recording the outcome matters more than anything else at this point.
	finCtx, cancelFin := context.WithTimeout(context.WithoutCancel(workCtx), w.cfg.FinalizeTimeout)
	defer cancelFin()

	if herr == nil {
		if len(resp) == 0 {
			resp = json.RawMessage(`null`)
		}
		// Retrying cannot fix a response that the database rejects.
		if err := validJSONB(resp); err != nil {
			herr = Permanent(fmt.Errorf("%w: %w", ErrInvalidResponse, err))
		}
	}

	switch {
	case herr == nil:
		w.finalize(finCtx, log, "complete", func(ctx context.Context) error {
			_, err := w.repo.Complete(ctx, CompleteParams{
				ID: job.ID, FencingToken: job.FencingToken, WorkerID: w.cfg.WorkerID, Response: resp,
			})
			if errors.Is(err, ErrInvalidData) {
				_, err = w.repo.Fail(ctx, FailParams{
					ID: job.ID, FencingToken: job.FencingToken, WorkerID: w.cfg.WorkerID,
					Error: errText(fmt.Errorf("%w: %w", ErrInvalidResponse, err)),
				})
			}
			return err
		})

	case errors.Is(cause, ErrShutdown):
		// Interrupted by forced shutdown: hand the job back immediately.
		// (The attempt already counted; if it was the last one the job fails.)
		w.finalize(finCtx, log, "release", func(ctx context.Context) error {
			_, err := w.repo.Retry(ctx, RetryParams{
				ID: job.ID, FencingToken: job.FencingToken, WorkerID: w.cfg.WorkerID,
				Error: "worker shutdown", BackoffSeconds: 0,
			})
			return err
		})

	case IsPermanent(herr):
		log.Error("job failed permanently", "err", herr)
		w.finalize(finCtx, log, "fail", func(ctx context.Context) error {
			_, err := w.repo.Fail(ctx, FailParams{
				ID: job.ID, FencingToken: job.FencingToken, WorkerID: w.cfg.WorkerID, Error: errText(herr),
			})
			return err
		})

	default:
		delay, ok := retryDelay(herr)
		if !ok {
			delay = w.cfg.Backoff(job.Attempts)
		}
		log.Warn("job failed; scheduling retry", "err", herr, "backoff", delay,
			"attempts_left", job.MaxAttempts-job.Attempts)
		// The query turns this into a terminal 'failed' when attempts are exhausted.
		w.finalize(finCtx, log, "retry", func(ctx context.Context) error {
			_, err := w.repo.Retry(ctx, RetryParams{
				ID: job.ID, FencingToken: job.FencingToken, WorkerID: w.cfg.WorkerID,
				Error: errText(herr), BackoffSeconds: delay.Seconds(),
			})
			return err
		})
	}
}

// heartbeatLoop renews the lease every LeaseDuration/3.
//   - ErrNoRows from the server => ownership is gone: cancel the handler with
//     ErrLeaseLost and let process() drop the result.
//   - Transient errors => keep trying, but stop the handler
//     (errLeaseExpiredLocally) a margin before our local lease clock runs out,
//     since another worker may claim the job once it does. We still try to
//     finalize afterwards: the fencing token makes that safe.
//
// The local deadline is enforced by its own timer, so a heartbeat that hangs
// cannot delay it, and each heartbeat's timeout ends at the deadline.
func (w *Worker) heartbeatLoop(ctx context.Context, cancel context.CancelCauseFunc, job Job, leaseDeadline time.Time, log *slog.Logger) {
	// The margin covers clock drift between us and the database, and the time
	// the handler takes to notice the cancellation.
	margin := w.cfg.LeaseDuration / 6
	stopAt := func() time.Duration { return time.Until(leaseDeadline) - margin }

	expire := time.NewTimer(stopAt())
	defer expire.Stop()
	t := time.NewTicker(w.cfg.LeaseDuration / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-expire.C:
			cancel(errLeaseExpiredLocally)
			return
		case <-t.C:
		}

		timeout := min(w.cfg.HeartbeatTimeout, stopAt())
		if timeout <= 0 {
			cancel(errLeaseExpiredLocally)
			return
		}
		sent := time.Now() // taken BEFORE the call: a conservative lease start
		hbCtx, c := context.WithTimeout(ctx, timeout)
		_, err := w.repo.Heartbeat(hbCtx, HeartbeatParams{
			ID: job.ID, FencingToken: job.FencingToken, WorkerID: w.cfg.WorkerID,
			LeaseSeconds: w.cfg.LeaseDuration.Seconds(),
		})
		c()

		switch {
		case err == nil:
			leaseDeadline = sent.Add(w.cfg.LeaseDuration)
			expire.Reset(stopAt())
		case errors.Is(err, ErrNoRows):
			cancel(ErrLeaseLost)
			return
		case ctx.Err() != nil:
			return // job finished while the heartbeat was in flight
		default:
			log.Warn("heartbeat failed", "err", err)
		}
	}
}

func (w *Worker) safeHandle(ctx context.Context, job Job, log *slog.Logger) (resp json.RawMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("handler panicked", "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return w.handler(ctx, job)
}

// finalize records an outcome, retrying transient DB errors a few times:
// losing a Complete means the job re-runs after the lease expires.
func (w *Worker) finalize(ctx context.Context, log *slog.Logger, op string, fn func(context.Context) error) {
	var err error
	for attempt := range 4 {
		if err = fn(ctx); err == nil || errors.Is(err, ErrNoRows) || errors.Is(err, ErrInvalidData) || ctx.Err() != nil {
			break
		}
		sleepCtx(ctx, time.Duration(attempt+1)*200*time.Millisecond)
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrNoRows):
		log.Warn("lease lost before finalize; result discarded", "op", op)
	default:
		log.Error("could not record job outcome; it will re-run after lease expiry", "op", op, "err", err)
	}
}

// --- helpers -----------------------------------------------------------------

// acquireSlots blocks for one free slot, then greedily takes up to max total.
func acquireSlots(ctx context.Context, sem chan struct{}, max int) int {
	select {
	case <-ctx.Done():
		return 0
	case sem <- struct{}{}:
	}
	n := 1
	for n < max {
		select {
		case sem <- struct{}{}:
			n++
		default:
			return n
		}
	}
	return n
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d)/2+1))
}
