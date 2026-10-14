// Package cache is a typed JSON cache on an UNLOGGED PostgreSQL table with
// stampede protection: when a value is missing or stale, one caller computes
// it under a compute lease while others serve the stale value or wait.
// Writes are versioned, so a slow computation can never overwrite a newer
// value. Contents are disposable: a database crash empties the table.
package cache

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// Config is copied at construction.
type Config struct {
	DB     *dbtx.DB
	Prefix string // prepended to every key
}

type Cache struct{ cfg Config }

func New(cfg Config) (*Cache, error) {
	if cfg.DB == nil || cfg.DB.Unwrap() == nil {
		return nil, fmt.Errorf("%w: DB is required", durable.ErrInvalidConfig)
	}
	return &Cache{cfg: cfg}, nil
}

// Contention decides what a caller does when another caller is computing and
// no stale value is available.
type Contention string

const (
	// Wait polls for the other computation for up to ComputeTTL, then
	// computes itself (default, Q6).
	Wait Contention = "wait"
	// ComputeAnyway computes immediately; versioning keeps the newest write.
	ComputeAnyway Contention = "compute"
)

// Options control GetOrCompute.
type Options struct {
	TTL          time.Duration // how long a value is fresh; required
	StaleFor     time.Duration // after TTL, how long it may be served while one caller recomputes
	ComputeTTL   time.Duration // compute lease; default 30s
	OnContention Contention    // default Wait
	PollInterval time.Duration // while waiting; default 50ms
}

func (c *Cache) key(k string) string { return c.cfg.Prefix + k }

func (c *Cache) q(ctx context.Context) *postgres.Queries { return postgres.New(c.cfg.DB.DBTx(ctx)) }

func (c *Cache) load(ctx context.Context, key string) (postgres.LoadCacheEntryRow, error) {
	r, err := c.q(ctx).LoadCacheEntry(ctx, c.key(key))
	if errors.Is(err, sql.ErrNoRows) {
		return postgres.LoadCacheEntryRow{}, nil
	}
	return r, err
}

// Get returns a fresh value, or found=false. T cannot be inferred, so name it:
//
//	v, ok, err := c.Get[Chart](ctx, "chart:sg")
func (c *Cache) Get[T any](ctx context.Context, key string) (v T, found bool, err error) {
	r, err := c.load(ctx, key)
	if err != nil || !r.Fresh {
		return v, false, err
	}
	return v, true, json.Unmarshal(r.Value, &v)
}

// Set stores v unconditionally with a new version. T is inferred from v.
func (c *Cache) Set[T any](ctx context.Context, key string, v T, ttl, staleFor time.Duration) error {
	if ttl <= 0 || staleFor < 0 {
		return fmt.Errorf("%w: positive TTL and non-negative StaleFor", durable.ErrInvalidArgument)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.q(ctx).SetCacheEntry(ctx, postgres.SetCacheEntryParams{
		Key: c.key(key), Value: b, TtlUs: pg.Micros(ttl), StaleUs: pg.Micros(ttl + staleFor),
	})
}

// claim takes the compute lease and returns its version token. It returns 0
// if another caller holds a live lease, or if a fresh value appeared since
// the caller looked (it lost the race to a computation that already finished).
func (c *Cache) claim(ctx context.Context, key, owner string, ttl time.Duration) (int64, error) {
	token, err := postgres.New(c.cfg.DB.DB()).ClaimCompute(ctx, postgres.ClaimComputeParams{
		Key: c.key(key), Owner: owner, TtlUs: pg.Micros(ttl),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return token, err
}

// store writes a computed value only if no newer version exists, and ends
// this caller's compute lease. It reports whether the value was stored.
func (c *Cache) store(ctx context.Context, key, owner string, token int64, value []byte, o Options) (bool, error) {
	n, err := postgres.New(c.cfg.DB.DB()).StoreComputed(ctx, postgres.StoreComputedParams{
		Value: value, Token: token, TtlUs: pg.Micros(o.TTL), StaleUs: pg.Micros(o.TTL + o.StaleFor),
		Owner: owner, Key: c.key(key),
	})
	if err != nil {
		return false, err
	}
	if n == 0 {
		_ = c.releaseCompute(ctx, key, owner)
	}
	return n == 1, nil
}

func (c *Cache) releaseCompute(ctx context.Context, key, owner string) error {
	return postgres.New(c.cfg.DB.DB()).ReleaseCompute(context.WithoutCancel(ctx), postgres.ReleaseComputeParams{
		Key: c.key(key), Owner: owner,
	})
}

// GetOrCompute returns a fresh value, or computes it with fn. Only one caller
// computes at a time; others get the stale value (within StaleFor) or follow
// OnContention. A computation that finishes after a newer value was written
// returns its own result but does not overwrite the newer one. T is inferred
// from fn.
func (c *Cache) GetOrCompute[T any](ctx context.Context, key string, o Options, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if o.TTL <= 0 || o.StaleFor < 0 || fn == nil {
		return zero, fmt.Errorf("%w: positive TTL, non-negative StaleFor and fn are required", durable.ErrInvalidArgument)
	}
	if o.ComputeTTL <= 0 {
		o.ComputeTTL = 30 * time.Second
	}
	if o.OnContention == "" {
		o.OnContention = Wait
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 50 * time.Millisecond
	}
	compute := func(token int64, owner string) (T, error) {
		v, err := fn(ctx)
		if err != nil {
			if owner != "" {
				_ = c.releaseCompute(ctx, key, owner)
			}
			return zero, err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return zero, err
		}
		if token == 0 {
			// Computing without the lease: take a version so the write is
			// still ordered against other writers.
			if token, err = postgres.New(c.cfg.DB.DB()).NextToken(ctx); err != nil {
				return v, err
			}
		}
		_, err = c.store(ctx, key, owner, token, b, o)
		return v, err
	}
	deadline := time.Now().Add(o.ComputeTTL)
	for {
		r, err := c.load(ctx, key)
		if err != nil {
			return zero, err
		}
		if r.Fresh {
			return decode[T](r.Value)
		}
		owner := rand.Text()
		token, err := c.claim(ctx, key, owner, o.ComputeTTL)
		if err != nil {
			return zero, err
		}
		if token > 0 {
			return compute(token, owner)
		}
		if r.StaleOk {
			return decode[T](r.Value)
		}
		if o.OnContention == ComputeAnyway || !time.Now().Before(deadline) {
			return compute(0, "")
		}
		t := time.NewTimer(o.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return zero, context.Cause(ctx)
		case <-t.C:
		}
	}
}

// Delete invalidates key. It joins a transaction in ctx, so invalidation
// commits (or rolls back) together with the change that caused it. It leaves
// a tombstone with a new version instead of removing the row, so a
// computation that started before the invalidation cannot store its stale
// result afterwards. Purge removes tombstones.
func (c *Cache) Delete(ctx context.Context, key string) error {
	return c.q(ctx).InvalidateCacheEntry(ctx, c.key(key))
}

// InvalidateTx is Delete that requires a transaction in ctx (S32).
func (c *Cache) InvalidateTx(txCtx context.Context, key string) error {
	if !c.cfg.DB.IsTx(txCtx) {
		return durable.ErrNotInTx
	}
	return c.Delete(txCtx, key)
}

// Purge deletes up to limit entries past their stale window with no live
// compute lease.
func (c *Cache) Purge(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	return postgres.New(c.cfg.DB.DB()).PurgeCache(ctx, int32(limit))
}

func decode[T any](b []byte) (T, error) {
	var v T
	return v, json.Unmarshal(b, &v)
}
