package lease

import (
	"context"
	"errors"

	"github.com/alextanhongpin/dbtx/postgres/durable"
)

// Activity acquires (waiting through contention), heartbeats while fn runs,
// cancels fn's context with the cause on loss, and releases afterward. It
// waits for fn to return even if fn ignores cancellation. No transaction or
// connection is held while fn runs.
func (l *Locker) Activity(ctx context.Context, key string, fn func(context.Context, Lease) error, opts ...Option) error {
	return l.activity(ctx, key, fn, false, opts...)
}

// TryActivity returns ErrContended without calling fn when the key is held.
func (l *Locker) TryActivity(ctx context.Context, key string, fn func(context.Context, Lease) error, opts ...Option) error {
	return l.activity(ctx, key, fn, true, opts...)
}

func (l *Locker) activity(ctx context.Context, key string, fn func(context.Context, Lease) error, try bool, opts ...Option) (err error) {
	if fn == nil {
		return durable.ErrInvalidArgument
	}
	h, err := l.acquire(ctx, key, try, opts...)
	if err != nil {
		return err
	}
	fnErr, cause := h.keeper.Supervise(ctx, key, func(workCtx context.Context) error {
		return fn(workCtx, h)
	})
	terminal := h.keeper.Err()
	if terminal != nil && !errors.Is(terminal, durable.ErrReleased) {
		durable.Observe(ctx, l.cfg.Observer, durable.Event{
			Type: durable.EventLeaseLost, Component: "lease", Key: key, Token: h.token, Err: terminal,
		})
	}
	cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.CleanupTimeout)
	defer stop()
	cleanupErr := h.Release(cleanupCtx)
	if terminal != nil && cause != nil && errors.Is(cause, terminal) {
		cause = nil
	}
	return errors.Join(fnErr, terminal, cause, cleanupErr)
}

// RunInFencedTx acquires, runs one short guarded transaction, and releases.
func (l *Locker) RunInFencedTx(ctx context.Context, key string, fn func(context.Context, Lease) error, opts ...Option) error {
	return l.runInFencedTx(ctx, key, fn, false, opts...)
}

// TryRunInFencedTx attempts acquisition once; fn is not called on contention.
func (l *Locker) TryRunInFencedTx(ctx context.Context, key string, fn func(context.Context, Lease) error, opts ...Option) error {
	return l.runInFencedTx(ctx, key, fn, true, opts...)
}

func (l *Locker) runInFencedTx(ctx context.Context, key string, fn func(context.Context, Lease) error, try bool, opts ...Option) (err error) {
	if fn == nil {
		return durable.ErrInvalidArgument
	}
	h, err := l.acquire(ctx, key, try, opts...)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.CleanupTimeout)
		defer cancel()
		if cleanupErr := h.Release(cleanupCtx); cleanupErr != nil {
			if err == nil {
				err = &durable.TxOutcomeError{Outcome: durable.TxCommitted, Err: cleanupErr}
			} else {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	return h.RunInFencedTx(ctx, func(txCtx context.Context) error { return fn(txCtx, h) })
}
