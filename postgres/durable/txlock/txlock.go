// Package txlock serializes work with transaction-scoped PostgreSQL advisory
// locks. Locks are released when the transaction commits or rolls back.
//
// Keys are strings (hashed with hashtextextended into the bigint lock space),
// int64 values (bigint space, so they can collide with string hashes; do not
// mix both kinds for unrelated resources), or pairs (PostgreSQL's two-int4
// space, which never conflicts with the bigint space).
//
// When ctx already carries a transaction for the Locker's DB, the locks join
// it. Lock acquisition then runs inside a savepoint, so a lock_timeout,
// contention or other failure while locking leaves the caller's transaction
// usable and releases any locks taken by that call. Bound waits there with
// Options.Wait (a server-side lock_timeout), not by cancelling ctx: how a
// cancelled statement affects the connection is up to the driver (lib/pq may
// discard it, which ends the caller's transaction).
package txlock

import (
	"cmp"
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

// ErrNotInTx is returned by Lock and TryLock without a transaction in ctx.
var ErrNotInTx = durable.ErrNotInTx

// maxWait is PostgreSQL's largest lock_timeout (INT_MAX milliseconds).
const maxWait = 2147483647 * time.Millisecond

// Key identifies one advisory lock. Build it with StringKey, Int64Key,
// PairKey or StringPairKey; the zero Key is invalid.
type Key struct {
	kind   keyKind
	s1, s2 string
	i1     int64
	p1, p2 int32
}

// StringKey hashes s with hashtextextended(s, 0) into the bigint lock space.
// The string methods (WithLock, WithLocks, ...) use the same hash.
func StringKey(s string) Key { return Key{kind: kindString, s1: s} }

// Int64Key locks n directly in the bigint lock space.
func Int64Key(n int64) Key { return Key{kind: kindInt64, i1: n} }

// PairKey locks (k1, k2) in PostgreSQL's two-int4 lock space.
func PairKey(k1, k2 int32) Key { return Key{kind: kindPairInt, p1: k1, p2: k2} }

// StringPairKey locks (hashtext(k1), hashtext(k2)) in the two-int4 space.
func StringPairKey(k1, k2 string) Key { return Key{kind: kindPairString, s1: k1, s2: k2} }

func (k Key) String() string {
	switch k.kind {
	case kindString:
		return k.s1
	case kindInt64:
		return fmt.Sprint(k.i1)
	case kindPairInt:
		return fmt.Sprintf("%d, %d", k.p1, k.p2)
	case kindPairString:
		return k.s1 + ", " + k.s2
	}
	return "<invalid>"
}

func (k Key) valid() bool {
	switch k.kind {
	case kindString:
		return k.s1 != ""
	case kindPairString:
		return k.s1 != "" && k.s2 != ""
	case kindInt64, kindPairInt:
		return true
	}
	return false
}

// Locker takes advisory locks inside dbtx transactions.
type Locker struct{ db *dbtx.DB }

func New(db *dbtx.DB) *Locker { return &Locker{db: db} }

// Options control waiting. Zero Wait means wait until ctx ends. Wait is
// capped at PostgreSQL's maximum lock_timeout (about 24.8 days).
type Options struct {
	Wait time.Duration
}

// WithLock runs fn in a transaction holding the lock for key. If ctx already
// carries a transaction for this DB, the lock joins it and is held until that
// transaction ends.
func (l *Locker) WithLock(ctx context.Context, key string, opts Options, fn func(txCtx context.Context) error) error {
	return l.WithKeys(ctx, []Key{StringKey(key)}, opts, fn)
}

// WithLocks locks every key, sorted and de-duplicated so that callers locking
// overlapping sets in any order cannot deadlock each other.
func (l *Locker) WithLocks(ctx context.Context, keys []string, opts Options, fn func(txCtx context.Context) error) error {
	return l.WithKeys(ctx, stringKeys(keys), opts, fn)
}

// TryWithLock returns ErrContended without calling fn if key is held.
func (l *Locker) TryWithLock(ctx context.Context, key string, fn func(txCtx context.Context) error) error {
	return l.TryWithKeys(ctx, []Key{StringKey(key)}, fn)
}

// TryWithLocks returns ErrContended without calling fn if any key is held.
// No lock taken by the call is kept on contention.
func (l *Locker) TryWithLocks(ctx context.Context, keys []string, fn func(txCtx context.Context) error) error {
	return l.TryWithKeys(ctx, stringKeys(keys), fn)
}

// WithKeys is WithLocks for any key kind.
func (l *Locker) WithKeys(ctx context.Context, keys []Key, opts Options, fn func(txCtx context.Context) error) error {
	return l.run(ctx, keys, opts, false, fn)
}

// TryWithKeys is TryWithLocks for any key kind.
func (l *Locker) TryWithKeys(ctx context.Context, keys []Key, fn func(txCtx context.Context) error) error {
	return l.run(ctx, keys, Options{}, true, fn)
}

// Lock takes the locks for keys in the transaction carried by ctx and holds
// them until that transaction ends; without one it returns ErrNotInTx.
// Re-locking a key the same transaction holds succeeds immediately. It takes
// all keys or none: on failure (including ErrContended after opts.Wait) the
// caller's transaction stays usable.
func (l *Locker) Lock(ctx context.Context, opts Options, keys ...Key) error {
	if !l.db.IsTx(ctx) {
		return ErrNotInTx
	}
	return l.run(ctx, keys, opts, false, func(context.Context) error { return nil })
}

// TryLock is Lock without waiting: it returns ErrContended, holding none of
// keys, if any is held by another transaction.
func (l *Locker) TryLock(ctx context.Context, keys ...Key) error {
	if !l.db.IsTx(ctx) {
		return ErrNotInTx
	}
	return l.run(ctx, keys, Options{}, true, func(context.Context) error { return nil })
}

func stringKeys(keys []string) []Key {
	ks := make([]Key, len(keys))
	for i, k := range keys {
		ks[i] = StringKey(k)
	}
	return ks
}

func (l *Locker) run(ctx context.Context, keys []Key, opts Options, try bool, fn func(context.Context) error) error {
	if fn == nil || len(keys) == 0 || opts.Wait < 0 || opts.Wait > maxWait {
		return fmt.Errorf("%w: fn, keys and 0 <= Wait <= %v are required", durable.ErrInvalidArgument, maxWait)
	}
	if slices.ContainsFunc(keys, func(k Key) bool { return !k.valid() }) {
		return fmt.Errorf("%w: empty or zero key", durable.ErrInvalidArgument)
	}
	ks := slices.Clone(keys)
	slices.SortFunc(ks, Key.compare)
	ks = slices.CompactFunc(ks, func(a, b Key) bool { return a.compare(b) == 0 })

	if l.db.IsTx(ctx) {
		if err := l.inSavepoint(ctx, func() error { return l.lockAll(ctx, ks, opts, try) }); err != nil {
			return err
		}
		return fn(ctx)
	}
	return l.db.RunInTx(ctx, func(txCtx context.Context) error {
		if err := l.lockAll(txCtx, ks, opts, try); err != nil {
			return err
		}
		return fn(txCtx)
	})
}

func (l *Locker) lockAll(ctx context.Context, ks []Key, opts Options, try bool) error {
	tx := l.db.DBTx(ctx)
	if try {
		for _, k := range ks {
			var ok bool
			if err := tx.QueryRowContext(ctx, k.tryQuery(), k.args()...).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return ErrContended
			}
		}
		return nil
	}
	q := postgres.New(tx)
	var previous string
	if opts.Wait > 0 {
		var err error
		if previous, err = q.CurrentLockTimeout(ctx); err != nil {
			return err
		}
		if err := q.SetLocalLockTimeout(ctx, pg.Timeout(opts.Wait)); err != nil {
			return err
		}
	}
	for _, k := range ks {
		if _, err := tx.ExecContext(ctx, k.lockQuery(), k.args()...); err != nil {
			if pg.SQLState(err) == pg.CodeLockNotAvailable {
				return errors.Join(ErrContended, err)
			}
			if cause := context.Cause(ctx); cause != nil {
				return errors.Join(cause, err)
			}
			return err
		}
	}
	if opts.Wait > 0 {
		return q.SetLocalLockTimeout(ctx, previous)
	}
	return nil
}

// inSavepoint runs fn inside a savepoint of the caller's transaction. On
// error it rolls back to the savepoint (with a context that ignores ctx
// cancellation, so a cancelled lock wait still leaves the transaction usable),
// which also releases advisory locks fn took.
func (l *Locker) inSavepoint(ctx context.Context, fn func() error) error {
	tx := l.db.DBTx(ctx)
	if _, err := tx.ExecContext(ctx, "SAVEPOINT durable_txlock"); err != nil {
		return err
	}
	if err := fn(); err != nil {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, rbErr := tx.ExecContext(rbCtx, "ROLLBACK TO SAVEPOINT durable_txlock"); rbErr != nil {
			return errors.Join(err, fmt.Errorf("txlock: rollback to savepoint: %w", rbErr))
		}
		_, relErr := tx.ExecContext(rbCtx, "RELEASE SAVEPOINT durable_txlock")
		return errors.Join(err, relErr)
	}
	_, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT durable_txlock")
	return err
}

type keyKind uint8

const (
	kindInvalid keyKind = iota
	kindString
	kindInt64
	kindPairInt
	kindPairString
)

func (a Key) compare(b Key) int {
	return cmp.Or(cmp.Compare(a.kind, b.kind),
		cmp.Compare(a.s1, b.s1), cmp.Compare(a.s2, b.s2),
		cmp.Compare(a.i1, b.i1), cmp.Compare(a.p1, b.p1), cmp.Compare(a.p2, b.p2))
}

func (k Key) args() []any {
	switch k.kind {
	case kindString:
		return []any{k.s1}
	case kindInt64:
		return []any{k.i1}
	case kindPairInt:
		return []any{k.p1, k.p2}
	default:
		return []any{k.s1, k.s2}
	}
}

func (k Key) lockQuery() string {
	switch k.kind {
	case kindString:
		return `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`
	case kindInt64:
		return `SELECT pg_advisory_xact_lock($1::bigint)`
	case kindPairInt:
		return `SELECT pg_advisory_xact_lock($1::int4, $2::int4)`
	default:
		return `SELECT pg_advisory_xact_lock(hashtext($1::text), hashtext($2::text))`
	}
}

func (k Key) tryQuery() string {
	switch k.kind {
	case kindString:
		return `SELECT pg_try_advisory_xact_lock(hashtextextended($1::text, 0))`
	case kindInt64:
		return `SELECT pg_try_advisory_xact_lock($1::bigint)`
	case kindPairInt:
		return `SELECT pg_try_advisory_xact_lock($1::int4, $2::int4)`
	default:
		return `SELECT pg_try_advisory_xact_lock(hashtext($1::text), hashtext($2::text))`
	}
}
