// Package txlock serializes work with transaction-scoped PostgreSQL advisory
// locks. Locks are released when the transaction commits or rolls back.
package txlock

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// ErrContended is returned when a lock could not be taken in time.
var ErrContended = durable.ErrContended

// Locker takes advisory locks inside dbtx transactions.
type Locker struct{ db *dbtx.DB }

func New(db *dbtx.DB) *Locker { return &Locker{db: db} }

// Options control waiting. Zero Wait means wait until ctx ends.
type Options struct {
	Wait time.Duration
}

// WithLock runs fn in a transaction holding the lock for key. If ctx already
// carries a transaction for this DB, the lock joins it and is held until that
// transaction ends.
func (l *Locker) WithLock(ctx context.Context, key string, opts Options, fn func(txCtx context.Context) error) error {
	return l.WithLocks(ctx, []string{key}, opts, fn)
}

// WithLocks locks every key, sorted and de-duplicated so that callers locking
// overlapping sets in any order cannot deadlock each other.
func (l *Locker) WithLocks(ctx context.Context, keys []string, opts Options, fn func(txCtx context.Context) error) error {
	return l.run(ctx, keys, opts, false, fn)
}

// TryWithLock returns ErrContended without calling fn if key is held.
func (l *Locker) TryWithLock(ctx context.Context, key string, fn func(txCtx context.Context) error) error {
	return l.run(ctx, []string{key}, Options{}, true, fn)
}

// TryWithLocks returns ErrContended without calling fn if any key is held.
// Inside an existing transaction, locks taken before the contended key stay
// held until that transaction ends.
func (l *Locker) TryWithLocks(ctx context.Context, keys []string, fn func(txCtx context.Context) error) error {
	return l.run(ctx, keys, Options{}, true, fn)
}

func (l *Locker) run(ctx context.Context, keys []string, opts Options, try bool, fn func(context.Context) error) error {
	if fn == nil || len(keys) == 0 || slices.Contains(keys, "") {
		return fmt.Errorf("%w: fn and non-empty keys are required", durable.ErrInvalidArgument)
	}
	keys = slices.Clone(keys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	return l.db.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(l.db.DBTx(txCtx))
		if try {
			for _, k := range keys {
				ok, err := q.TryAdvisoryXactLock(txCtx, k)
				if err != nil {
					return err
				}
				if !ok {
					return ErrContended
				}
			}
			return fn(txCtx)
		}
		var previous string
		if opts.Wait > 0 {
			var err error
			if previous, err = q.CurrentLockTimeout(txCtx); err != nil {
				return err
			}
			if err := q.SetLocalLockTimeout(txCtx, pg.Timeout(opts.Wait)); err != nil {
				return err
			}
		}
		for _, k := range keys {
			if err := q.AdvisoryXactLock(txCtx, k); err != nil {
				if pg.SQLState(err) == pg.CodeLockNotAvailable {
					return errors.Join(ErrContended, err)
				}
				return err
			}
		}
		if opts.Wait > 0 {
			if err := q.SetLocalLockTimeout(txCtx, previous); err != nil {
				return err
			}
		}
		return fn(txCtx)
	})
}
