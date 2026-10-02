package lock

import (
	"context"
	"errors"
	"fmt"

	"github.com/alextanhongpin/dbtx"
)

var (
	ErrLockOutsideTx = errors.New("lock: cannot lock outside transaction")
)

const (
	lock        = `SELECT pg_advisory_xact_lock($1)`
	lockPair    = `SELECT pg_advisory_xact_lock($1, $2)`
	tryLock     = `SELECT pg_try_advisory_xact_lock($1)`
	tryLockPair = `SELECT pg_try_advisory_xact_lock($1, $2)`
)

// Lock locks the given key. If multiple operations lock the same key, it
// will wait for the previous operation to complete.
// Lock must be run within a transaction context, panics otherwise.
func Lock[K key](ctx context.Context, key K) error {
	return NamedLock(ctx, dbtx.ID, key)
}

// TryLock locks the given key. If multiple operations lock the same key, only
// the first will succeed. The rest will fail with the error ErrAlreadyLocked.
// TryLock must be run within a transaction context, panics otherwise.
func TryLock[K key](ctx context.Context, key K) (bool, error) {
	return NamedTryLock(ctx, dbtx.ID, key)
}

type Pair[T any] struct {
	Key1, Key2 T
}

func (p Pair[T]) String() string {
	return fmt.Sprintf("%v, %v", p.Key1, p.Key2)
}

type key interface {
	~int64 | ~string | Pair[string] | Pair[int32]
}

func NamedTryLock[K key](ctx context.Context, id string, key K) (bool, error) {
	tx, ok := dbtx.NamedValue(ctx, id)
	if !ok {
		return false, fmt.Errorf("%w: Key(%v)", ErrLockOutsideTx, key)
	}

	// locked will be true if the key is locked successfully.
	var locked bool
	var err error
	switch k := any(key).(type) {
	case string:
		err = tx.QueryRowContext(ctx, tryLock, Hash64(k)).Scan(&locked)
	case int64:
		err = tx.QueryRowContext(ctx, tryLock, k).Scan(&locked)
	case Pair[string]:
		err = tx.QueryRowContext(ctx, tryLockPair, Hash32(k.Key1), Hash32(k.Key2)).Scan(&locked)
	case Pair[int32]:
		err = tx.QueryRowContext(ctx, tryLockPair, k.Key1, k.Key2).Scan(&locked)
	}
	return locked, err
}

func NamedLock[K key](ctx context.Context, id string, key K) error {
	tx, ok := dbtx.NamedValue(ctx, id)
	if !ok {
		return fmt.Errorf("%w: Key(%v)", ErrLockOutsideTx, key)
	}

	// locked will be true if the key is locked successfully.
	var err error
	switch k := any(key).(type) {
	case string:
		_, err = tx.ExecContext(ctx, lock, Hash64(k))
	case int64:
		_, err = tx.ExecContext(ctx, lock, k)
	case Pair[string]:
		_, err = tx.ExecContext(ctx, lockPair, Hash32(k.Key1), Hash32(k.Key2))
	case Pair[int32]:
		_, err = tx.ExecContext(ctx, lockPair, k.Key1, k.Key2)
	}
	return err
}
