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
	ErrLockOutsideTx = errors.New("lock: cannot lock outside transaction")
)

type Locker struct {
	db *sql.DB
}

func New(db *sql.DB) *Locker {
	return &Locker{db: db}
}

func (l *Locker) Lock(ctx context.Context, key *Key, fn func(context.Context) error) error {
	return dbtx.New(l.db).RunInTx(ctx, func(txCtx context.Context) error {
		if err := Lock(txCtx, key); err != nil {
			return err
		}

		return fn(txCtx)
	})
}

func (l *Locker) TryLock(ctx context.Context, key *Key, fn func(context.Context) error) error {
	return dbtx.New(l.db).RunInTx(ctx, func(txCtx context.Context) error {
		if err := TryLock(txCtx, key); err != nil {
			return err
		}

		return fn(txCtx)
	})
}

// Lock locks the given key. If multiple operations lock the same key, it
// will wait for the previous operation to complete.
// Lock must be run within a transaction context, panics otherwise.
func Lock(ctx context.Context, key *Key) error {
	tx, ok := dbtx.Value(ctx)
	if !ok {
		return fmt.Errorf("%w: %s", ErrLockOutsideTx, key)
	}

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
	tx, ok := dbtx.Value(ctx)
	if !ok {
		return fmt.Errorf("%w: %s", ErrLockOutsideTx, key)
	}

	// locked will be true if the key is locked successfully.
	var isLockAcquired bool
	var err error
	if key.pair {
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1, $2)`, key.x, key.y).Scan(&isLockAcquired)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, key.z).Scan(&isLockAcquired)
	}
	if err != nil {
		return err
	}

	if !isLockAcquired {
		return fmt.Errorf("%w: %s", ErrAlreadyLocked, key)
	}

	return nil
}
