package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// Errors returned by the key-value operations. A key "exists" while it holds
// a fresh value: stale values, tombstones and compute leases do not count.
// Missing keys are reported with durable.ErrNotFound.
var (
	ErrConflict = errors.New("cache: value does not match")
	ErrExists   = errors.New("cache: key exists")
)

// Every write below joins a transaction in ctx and takes a new version, so
// it wins over a computation in flight: that computation returns its value
// to its caller but cannot store it.

func checkTTL(ttl, staleFor time.Duration) error {
	if ttl <= 0 || staleFor < 0 {
		return fmt.Errorf("%w: positive TTL and non-negative StaleFor", durable.ErrInvalidArgument)
	}
	return nil
}

// Exists reports whether key holds a fresh value.
func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	r, err := c.load(ctx, key)
	return r.Fresh, err
}

// TTL returns how long key's value stays fresh, or durable.ErrNotFound.
func (c *Cache) TTL(ctx context.Context, key string) (time.Duration, error) {
	us, err := c.q(ctx).CacheEntryTTL(ctx, c.key(key))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, durable.ErrNotFound
	}
	return time.Duration(us) * time.Microsecond, err
}

// Expire gives key's fresh value a new TTL and stale window, counted from
// now, without changing its version. It returns durable.ErrNotFound if key
// holds no fresh value.
func (c *Cache) Expire(ctx context.Context, key string, ttl, staleFor time.Duration) error {
	if err := checkTTL(ttl, staleFor); err != nil {
		return err
	}
	n, err := c.q(ctx).ExpireCacheEntry(ctx, postgres.ExpireCacheEntryParams{
		Key: c.key(key), TtlUs: pg.Micros(ttl), StaleUs: pg.Micros(ttl + staleFor),
	})
	if err == nil && n == 0 {
		err = durable.ErrNotFound
	}
	return err
}

// StoreOnce stores v only if key holds no fresh value (a stale value or a
// tombstone is replaced). It returns ErrExists otherwise.
func (c *Cache) StoreOnce[T any](ctx context.Context, key string, v T, ttl, staleFor time.Duration) error {
	stored, err := c.storeIfAbsent(ctx, key, v, ttl, staleFor)
	if err == nil && !stored {
		err = ErrExists
	}
	return err
}

func (c *Cache) storeIfAbsent(ctx context.Context, key string, v any, ttl, staleFor time.Duration) (bool, error) {
	if err := checkTTL(ttl, staleFor); err != nil {
		return false, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	n, err := c.q(ctx).StoreCacheEntryIfAbsent(ctx, postgres.StoreCacheEntryIfAbsentParams{
		Key: c.key(key), Value: b, TtlUs: pg.Micros(ttl), StaleUs: pg.Micros(ttl + staleFor),
	})
	return n == 1, err
}

// LoadOrStore returns key's fresh value with loaded=true, or stores v and
// returns it with loaded=false. T is inferred from v.
func (c *Cache) LoadOrStore[T any](ctx context.Context, key string, v T, ttl, staleFor time.Duration) (cur T, loaded bool, err error) {
	for {
		stored, err := c.storeIfAbsent(ctx, key, v, ttl, staleFor)
		if err != nil {
			return cur, false, err
		}
		if stored {
			return v, false, nil
		}
		r, err := c.load(ctx, key)
		if err != nil {
			return cur, false, err
		}
		if r.Fresh {
			cur, err = decode[T](r.Value)
			return cur, err == nil, err
		}
		// The value expired or was deleted in between: try again.
		if err := context.Cause(ctx); err != nil {
			return cur, false, err
		}
	}
}

// CompareAndSwap replaces key's fresh value with newV if it equals oldV, as
// JSON (jsonb equality: key order and whitespace do not matter). It returns
// durable.ErrNotFound if key holds no fresh value and ErrConflict if it holds
// a different one.
func (c *Cache) CompareAndSwap[T any](ctx context.Context, key string, oldV, newV T, ttl, staleFor time.Duration) error {
	if err := checkTTL(ttl, staleFor); err != nil {
		return err
	}
	ob, err := json.Marshal(oldV)
	if err != nil {
		return err
	}
	nb, err := json.Marshal(newV)
	if err != nil {
		return err
	}
	n, err := c.q(ctx).CompareAndSwapCacheEntry(ctx, postgres.CompareAndSwapCacheEntryParams{
		Key: c.key(key), OldValue: ob, NewValue: nb, TtlUs: pg.Micros(ttl), StaleUs: pg.Micros(ttl + staleFor),
	})
	if err != nil || n == 1 {
		return err
	}
	return c.mismatch(ctx, key)
}

// CompareAndDelete invalidates key, leaving a versioned tombstone like
// Delete, if its fresh value equals oldV as JSON. Errors are as for
// CompareAndSwap.
func (c *Cache) CompareAndDelete[T any](ctx context.Context, key string, oldV T) error {
	ob, err := json.Marshal(oldV)
	if err != nil {
		return err
	}
	n, err := c.q(ctx).CompareAndInvalidateCacheEntry(ctx, postgres.CompareAndInvalidateCacheEntryParams{
		Key: c.key(key), OldValue: ob,
	})
	if err != nil || n == 1 {
		return err
	}
	return c.mismatch(ctx, key)
}

// mismatch classifies a failed compare: no fresh value, or a different one.
func (c *Cache) mismatch(ctx context.Context, key string) error {
	r, err := c.load(ctx, key)
	switch {
	case err != nil:
		return err
	case r.Fresh:
		return ErrConflict
	default:
		return durable.ErrNotFound
	}
}

// LoadAndDelete invalidates key like Delete and returns the fresh value it
// held. It returns durable.ErrNotFound if there was none; a stale value is
// still invalidated. T cannot be inferred, so name it.
func (c *Cache) LoadAndDelete[T any](ctx context.Context, key string) (v T, err error) {
	r, err := c.q(ctx).LoadAndInvalidateCacheEntry(ctx, c.key(key))
	if errors.Is(err, sql.ErrNoRows) {
		return v, durable.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if !r.Fresh {
		return v, durable.ErrNotFound
	}
	return decode[T](r.OldValue)
}
