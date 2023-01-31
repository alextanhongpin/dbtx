package lock

import (
	"context"
	"errors"
	"fmt"

	"github.com/alextanhongpin/uow"
)

var ErrLockOutsideTx = errors.New("lock: lock must be carried out in transaction")

// Lock locks the given key. If multiple operations lock the same key, it
// will wait for the previous operation to complete.
// Lock must be run within a transaction context, panics otherwise.
func Lock(ctx context.Context, key Key) error {
	uowCtx := uow.MustValue(ctx)
	if !uowCtx.IsTx() {
		return fmt.Errorf("%w: %s", ErrLockOutsideTx, key)
	}

	tx := uowCtx.DB(ctx)

	switch v := key.(type) {
	case *intKey:
		_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, v.m, v.n)
		return err
	case *bigIntKey:
		_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, v.b)
		return err
	default:
		panic("sql: invalid Key")
	}
}

// TryLock locks the given key. If multiple operations lock the same key, only
// the first will succeed. The rest will fail with the error ErrAlreadyLocked.
// TryLock must be run within a transaction context, panics otherwise.
func TryLock(ctx context.Context, key Key) (locked bool, err error) {
	uowCtx := uow.MustValue(ctx)
	if !uowCtx.IsTx() {
		return false, fmt.Errorf("%w: %s", ErrLockOutsideTx, key)
	}

	tx := uowCtx.DB(ctx)

	// locked will be true if the key is locked successfully.
	switch v := key.(type) {
	case *intKey:
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1, $2)`, v.m, v.n).Scan(&locked)
	case *bigIntKey:
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, v.b).Scan(&locked)
	default:
		panic("sql: invalid Key")
	}

	return
}
