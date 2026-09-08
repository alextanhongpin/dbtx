package cache

import (
	_ "embed"

	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/cache/internal/postgres"
	"github.com/alextanhongpin/dbtx/postgres/lock"
)

var (
	//go:embed internal/schema.sql
	schema string

	// Errors.
	ErrNotExist = errors.New("cache: not exist")
	ErrConflict = errors.New("cache: conflict")
	ErrExists   = errors.New("cache: exists")
)

type Cache struct {
	*dbtx.DB
	prefix string
}

func New(db *sql.DB) *Cache {
	return &Cache{
		DB: dbtx.New(db),
	}
}

func (c *Cache) Prefix() string {
	return c.prefix
}

func (c *Cache) SetPrefix(prefix string) {
	c.prefix = prefix
}

// CompareAndDelete atomically deletes a key only if its current value matches the expected old value.
func (c *Cache) CompareAndDelete[T any](ctx context.Context, key string, old T) error {
	key = c.buildKey(key)
	row, err := newDto(key, old, 0)
	if err != nil {
		return err
	}
	_, err = c.db(ctx).CompareAndDelete(ctx, postgres.CompareAndDeleteParams{
		Key:    key,
		Digest: row.Digest,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotExist
	}
	return err
}

// CompareAndSwap atomically updates a key only if its current value matches the expected old value.
func (c *Cache) CompareAndSwap[T any](ctx context.Context, key string, old, value T, ttl time.Duration) error {
	key = c.buildKey(key)
	oldVal, err := newDto(key, old, 0)
	if err != nil {
		return err
	}
	newVal, err := newDto(key, value, ttl)
	if err != nil {
		return err
	}
	_, err = c.db(ctx).CompareAndSwap(ctx, postgres.CompareAndSwapParams{
		Value:  newVal.Value,
		Key:    key,
		Digest: oldVal.Digest,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotExist
	}
	return err
}

// Delete removes one or more keys from the cache.
func (c *Cache) Delete(ctx context.Context, key string) error {
	key = c.buildKey(key)
	_, err := c.db(ctx).Delete(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotExist
	}

	return err
}

// Expire sets a timeout on a key. After the timeout has expired, the key will automatically be ok.
func (c *Cache) Expire(ctx context.Context, key string, ttl time.Duration) error {
	key = c.buildKey(key)
	_, err := c.db(ctx).Expire(ctx, postgres.ExpireParams{
		ExpiresAt: sql.NullTime{
			Time:  time.Now().Add(ttl),
			Valid: ttl > 0,
		},
		Key: key,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotExist
	}
	return err
}

// Load retrieves the value for a key. Returns ErrNotExist if the key doesn't exist.
func (c *Cache) Load[T any](ctx context.Context, key string) (T, error) {
	key = c.buildKey(key)
	return c.load[T](ctx, key)
}

func (c *Cache) load[T any](ctx context.Context, key string) (T, error) {
	var zero T
	dto, err := c.db(ctx).Load(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, ErrNotExist
	}
	if err != nil {
		return zero, err
	}

	var v T
	err = json.Unmarshal(dto.Value, &v)
	if err != nil {
		return zero, err
	}
	return v, nil
}

// LoadAndDelete atomically retrieves and deletes a key's value.
func (c *Cache) LoadAndDelete[T any](ctx context.Context, key string) (value T, err error) {
	key = c.buildKey(key)

	var zero T
	dto, err := c.db(ctx).Delete(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, ErrNotExist
	}
	if err != nil {
		return zero, err
	}

	var v T
	err = json.Unmarshal(dto.Value, &v)
	if err != nil {
		return zero, err
	}
	return v, nil
}

// LoadOrStore atomically loads a key's value if it exists, or stores the provided value if it doesn't.
// Returns the current value and whether it was loaded (true) or stored (false).
func (c *Cache) LoadOrStore[T any](ctx context.Context, key string, value T, ttl time.Duration) (curr T, loaded bool, err error) {
	key = c.buildKey(key)
	err = c.RunInTx(ctx, func(ctx context.Context) error {
		if err := lock.NamedLock(ctx, c.ID(), lock.Pair[string]{Key1: c.prefix, Key2: key}); err != nil {
			return err
		}

		v, err := c.load[T](ctx, key)
		if errors.Is(err, ErrNotExist) {
			err = c.store(ctx, key, value, ttl)
			if err != nil {
				return err
			}

			curr = value
			return nil
		}
		if err != nil {
			return err
		}

		curr = v
		loaded = true
		return nil
	})

	return
}

func (c *Cache) LoadOrCreate[T any](ctx context.Context, key string, fn func(ctx context.Context, key string) (T, time.Duration, error)) (curr T, loaded bool, err error) {
	key = c.buildKey(key)
	v, err := c.load[T](ctx, key)
	if err == nil {
		return v, true, nil
	}
	if !errors.Is(err, ErrNotExist) {
		return curr, false, err
	}

	err = c.RunInTx(ctx, func(ctx context.Context) error {
		if err := lock.NamedLock(ctx, c.ID(), lock.Pair[string]{Key1: c.prefix, Key2: key}); err != nil {
			return err
		}
		v, err := c.load[T](ctx, key)
		if errors.Is(err, ErrNotExist) {
			val, ttl, err := fn(ctx, key)
			if err != nil {
				return err
			}

			err = c.store(ctx, key, val, ttl)
			if err != nil {
				return err
			}

			curr = val
			return nil
		}
		if err != nil {
			return err
		}

		curr = v
		loaded = true
		return nil
	})
	if err != nil {
		var zero T
		return zero, false, err
	}

	return curr, loaded, nil
}

// Store sets a key's value with the specified TTL.
func (c *Cache) Store[T any](ctx context.Context, key string, value T, ttl time.Duration) error {
	key = c.buildKey(key)
	return c.store(ctx, key, value, ttl)
}

func (c *Cache) store[T any](ctx context.Context, key string, value T, ttl time.Duration) error {
	row, err := newDto(key, value, ttl)
	if err != nil {
		return err
	}
	_, err = c.db(ctx).Store(ctx, postgres.StoreParams{
		Key:       row.Key,
		Value:     row.Value,
		Digest:    row.Digest,
		ExpiresAt: row.ExpiresAt,
	})
	return err
}

// StoreOnce stores a key's value only if the key doesn't already exist.
func (c *Cache) StoreOnce[T any](ctx context.Context, key string, value T, ttl time.Duration) error {
	key = c.buildKey(key)
	return c.storeOnce(ctx, key, value, ttl)
}

func (c *Cache) storeOnce[T any](ctx context.Context, key string, value T, ttl time.Duration) error {
	row, err := newDto(key, value, ttl)
	if err != nil {
		return err
	}
	_, err = c.db(ctx).StoreOnce(ctx, postgres.StoreOnceParams{
		Key:       row.Key,
		Value:     row.Value,
		Digest:    row.Digest,
		ExpiresAt: row.ExpiresAt,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrExists
	}
	return err
}

// Exists checks if a key exists in the cache.
func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	key = c.buildKey(key)
	_, err := c.db(ctx).Load(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// TTL returns the remaining time to live for a key.
// Returns -1 if the key exists but has no expiration.
// Returns -2 if the key does not exist.
func (c *Cache) TTL(ctx context.Context, key string) (time.Duration, error) {
	key = c.buildKey(key)
	row, err := c.db(ctx).Load(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return -2, nil
	}
	if err != nil {
		return 0, err
	}
	if !row.ExpiresAt.Valid {
		return -1, nil
	}

	return time.Until(row.ExpiresAt.Time), nil
}

func (c *Cache) Purge(ctx context.Context) (int64, error) {
	return c.db(ctx).Purge(ctx)
}

func (c *Cache) Migrate(ctx context.Context) error {
	_, err := c.DBTx(ctx).ExecContext(ctx, schema)
	return err
}

func (c *Cache) buildKey(key string) string {
	return fmt.Sprintf("%s:%s", c.prefix, key)
}

func (c *Cache) db(ctx context.Context) postgres.Querier {
	return postgres.New(c.DBTx(ctx))
}
