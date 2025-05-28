package lock

import (
	"context"
	"errors"
	"fmt"

	"github.com/alextanhongpin/dbtx"
)

var (
	ErrAlreadyLocked = errors.New("lock: key already locked")
	ErrLockOutsideTx = errors.New("lock: cannot lock outside transaction")
)

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
