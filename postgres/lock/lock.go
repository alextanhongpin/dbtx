package lock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/alextanhongpin/dbtx"
)

var (
	ErrAlreadyLocked = errors.New("lock: key already locked")
	ErrLockOutsideTx = errors.New("lock: lock must be carried out in transaction")
)

type Locker struct {
	db  *sql.DB
	err error
}

func New(db *sql.DB) *Locker {
	return &Locker{db: db}
}

func (l *Locker) Lock(ctx context.Context, key *Key) (func(), error) {
	return l.lock(ctx, key, Lock)
}

func (l *Locker) TryLock(ctx context.Context, key *Key) (func(), error) {
	return l.lock(ctx, key, TryLock)
}

func (l *Locker) lock(ctx context.Context, key *Key, lockFn func(ctx context.Context, key *Key) error) (func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan error)

	go func() {
		_ = dbtx.RunInTx(ctx, l.db, nil, func(tx *sql.Tx) error {
			txCtx := dbtx.WithValue(ctx, dbtx.NewTx(tx))
			err := lockFn(txCtx, key)
			ch <- err
			// If there is an error, just end early.
			if err != nil {
				return err
			}

			// Otherwise, wait until the context is cancelled.
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	return cancel, <-ch
}

// Lock locks the given key. If multiple operations lock the same key, it
// will wait for the previous operation to complete.
// Lock must be run within a transaction context, panics otherwise.
func Lock(ctx context.Context, key *Key) error {
	txCtx := dbtx.MustValue(ctx)
	if !txCtx.IsTx() {
		return fmt.Errorf("%w: %s", ErrLockOutsideTx, key)
	}

	tx := txCtx.Tx(ctx)
	if key.pair {
		_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, key.x, key.y)
		return err
	}

	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, key.z)
	return err
}

// TryLock locks the given key. If multiple operations lock the same key, only
// the first will succeed. The rest will fail with the error ErrAlreadyLocked.
// TryLock must be run within a transaction context, panics otherwise.
func TryLock(ctx context.Context, key *Key) error {
	txCtx := dbtx.MustValue(ctx)
	if !txCtx.IsTx() {
		return fmt.Errorf("%w: %s", ErrLockOutsideTx, key)
	}

	tx := txCtx.Tx(ctx)

	// locked will be true if the key is locked successfully.
	var locked bool
	var err error
	if key.pair {
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1, $2)`, key.x, key.y).Scan(&locked)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, key.z).Scan(&locked)
	}
	if err != nil {
		return err
	}

	if !locked {
		return fmt.Errorf("%w: %s", ErrAlreadyLocked, key)
	}

	return nil
}
