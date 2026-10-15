package lease

import (
	"context"
	"errors"
	"time"
)

// Activity acquires, renews while fn runs, cancels fn's context on loss, and
// releases afterward. It waits for fn to return even if fn ignores cancellation.
// External effects must enforce their own idempotency/fencing.
func (l *Locker) Activity(ctx context.Context, key string, fn func(context.Context, Lease) error, opts ...LockOption) error {
	return l.activity(ctx, key, fn, false, opts...)
}

// TryActivity returns ErrContended without invoking fn if ownership is held.
func (l *Locker) TryActivity(ctx context.Context, key string, fn func(context.Context, Lease) error, opts ...LockOption) error {
	return l.activity(ctx, key, fn, true, opts...)
}

func (l *Locker) activity(ctx context.Context, key string, fn func(context.Context, Lease) error, try bool, opts ...LockOption) (err error) {
	if fn == nil {
		return ErrInvalidOption
	}
	acquired, err := l.acquire(ctx, key, try, opts...)
	if err != nil {
		return err
	}
	h := acquired.(*heldLease)
	workCtx, cancel := context.WithCancelCause(ctx)
	hbCtx, stopHB := context.WithCancel(workCtx)
	watchCtx, stopWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	// Loss detection must not wait for an in-flight heartbeat request to return.
	go func() {
		defer close(watchDone)
		select {
		case <-watchCtx.Done():
		case <-h.done:
			cancel(h.Err())
		}
	}()
	hbDone := make(chan struct{})
	go func() { defer close(hbDone); h.heartbeat(hbCtx, cancel) }()
	defer func() {
		stopHB()
		stopWatch()
		<-watchDone
		<-hbDone
		// Inspect loss before release changes the state to ErrReleased.
		terminal := h.Err()
		cause := context.Cause(workCtx)
		cancel(nil)
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.CleanupTimeout)
		defer stop()
		cleanupErr := h.Release(cleanupCtx)
		err = errors.Join(err, terminal, cause, cleanupErr)
	}()
	return fn(workCtx, h)
}

func (h *heldLease) heartbeat(ctx context.Context, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(h.ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.done:
			cancel(h.Err())
			return
		case <-ticker.C:
		case <-h.renewNow:
		}
		err := h.renew(ctx, true)
		if terminal := h.Err(); terminal != nil {
			cancel(terminal)
			return
		}
		// Busy means a short fenced transaction/lifecycle call is in progress.
		// Transport failures leave the existing deadline unchanged; retry next tick.
		if err != nil && ctx.Err() != nil {
			return
		}
	}
}

// RunInFencedTx acquires a new lease, runs a short guarded transaction, and
// releases it. Repositories join the callback context through Config.DB.
func (l *Locker) RunInFencedTx(ctx context.Context, key string, fn func(context.Context, Lease) error, opts ...LockOption) error {
	return l.runInFencedTx(ctx, key, fn, false, opts...)
}

// TryRunInFencedTx attempts acquisition once; on contention fn is not invoked.
func (l *Locker) TryRunInFencedTx(ctx context.Context, key string, fn func(context.Context, Lease) error, opts ...LockOption) error {
	return l.runInFencedTx(ctx, key, fn, true, opts...)
}

func (l *Locker) runInFencedTx(ctx context.Context, key string, fn func(context.Context, Lease) error, try bool, opts ...LockOption) (err error) {
	if fn == nil {
		return ErrInvalidOption
	}
	held, err := l.acquire(ctx, key, try, opts...)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.CleanupTimeout)
		defer cancel()
		cleanupErr := held.Release(cleanupCtx)
		if cleanupErr != nil {
			if err == nil {
				err = &TxOutcomeError{TxCommitted, cleanupErr}
			} else {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	return held.RunInFencedTx(ctx, func(txCtx context.Context) error { return fn(txCtx, held) })
}
