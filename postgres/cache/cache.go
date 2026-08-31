package cache

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "embed"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/cache/internal/postgres"
	"github.com/alextanhongpin/dbtx/postgres/lock"
)

var (
	ErrNotExist = errors.New("cache: not exist")
	ErrExists   = errors.New("cache: exists")
)

type Cache struct {
	*dbtx.DB
}

func New(db *sql.DB) *Cache {
	return &Cache{
		DB: dbtx.New(db),
	}
}

// CompareAndDelete atomically deletes a key only if its current value matches the expected old value.
func (c *Cache) CompareAndDelete[T any](ctx context.Context, key string, old T) error {
	deleted, err := c.deleteExpired(ctx, key)
	if err != nil {
		return err
	}
	if deleted {
		return ErrNotExist
	}
	row, err := newRow(key, old, 0)
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
	deleted, err := c.deleteExpired(ctx, key)
	if err != nil {
		return err
	}
	if deleted {
		return ErrNotExist
	}
	oldDto, err := newRow(key, old, 0)
	if err != nil {
		return err
	}
	newDto, err := newRow(key, value, ttl)
	if err != nil {
		return err
	}
	_, err = c.db(ctx).CompareAndSwap(ctx, postgres.CompareAndSwapParams{
		Value:  newDto.Value,
		Key:    key,
		Digest: oldDto.Digest,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotExist
	}
	return err
}

// Load retrieves the value for a key. Returns ErrNotExist if the key doesn't exist.
func (c *Cache) Load[T any](ctx context.Context, key string) (T, error) {
	var zero T
	dto, err := c.load(ctx, key)
	if err != nil {
		return zero, err
	}
	return dto.Load[T]()
}

// LoadAndDelete atomically retrieves and deletes a key's value.
func (c *Cache) LoadAndDelete[T any](ctx context.Context, key string) (value T, err error) {
	var zero T
	dto, err := c.delete(ctx, key)
	if err != nil {
		return zero, err
	}
	return dto.Load[T]()
}

// LoadOrStore atomically loads a key's value if it exists, or stores the provided value if it doesn't.
// Returns the current value and whether it was loaded (true) or stored (false).
func (c *Cache) LoadOrStore[T any](ctx context.Context, key string, value T, ttl time.Duration) (curr T, loaded bool, err error) {
	err = c.RunInTx(ctx, func(ctx context.Context) error {
		if err := lock.Lock(ctx, lock.NewStrKey(key)); err != nil {
			return err
		}

		dto, err := c.load(ctx, key)
		if err == nil {
			curr, err = dto.Load[T]()
			if err != nil {
				return err
			}
			loaded = true
			return nil
		}
		if !errors.Is(err, ErrNotExist) {
			return err
		}
		curr = value
		return c.StoreOnce(ctx, key, value, ttl)
	})
	return
}

func (c *Cache) LoadOrCreate[T any](ctx context.Context, key string, fn func(ctx context.Context, key string) (T, time.Duration, error)) (curr T, loaded bool, err error) {
	err = c.RunInTx(ctx, func(ctx context.Context) error {
		if err := lock.Lock(ctx, lock.NewStrKey(key)); err != nil {
			return err
		}
		dto, err := c.load(ctx, key)
		if err == nil {
			curr, err = dto.Load[T]()
			if err != nil {
				return err
			}
			loaded = true
			return nil
		}
		if !errors.Is(err, ErrNotExist) {
			return err
		}
		newVal, ttl, err := fn(ctx, key)
		if err != nil {
			return err
		}
		if err := c.StoreOnce(ctx, key, newVal, ttl); err != nil {
			return err
		}
		curr = newVal
		return nil
	})
	return
}

// Store sets a key's value with the specified TTL.
func (c *Cache) Store[T any](ctx context.Context, key string, value T, ttl time.Duration) error {
	row, err := newRow(key, value, ttl)
	if err != nil {
		return err
	}
	_, err = c.db(ctx).Store(ctx, *row)
	return err
}

// StoreOnce stores a key's value only if the key doesn't already exist.
func (c *Cache) StoreOnce[T any](ctx context.Context, key string, value T, ttl time.Duration) error {
	row, err := newRow(key, value, ttl)
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
	dto, err := c.load(ctx, key)
	if errors.Is(err, ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return dto.Valid(), nil
}

// TTL returns the remaining time to live for a key.
// Returns -1 if the key exists but has no expiration.
// Returns -2 if the key does not exist.
func (c *Cache) TTL(ctx context.Context, key string) (time.Duration, error) {
	row, err := c.db(ctx).TTL(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return -2, nil
	}
	if err != nil {
		return 0, err
	}
	dto := newDto(row)
	if dto.ExpiresAt == nil {
		return -1, nil
	}
	return time.Until(*dto.ExpiresAt), nil
}

// Expire sets a timeout on a key. After the timeout has expired, the key will automatically be deleted.
func (c *Cache) Expire(ctx context.Context, key string, ttl time.Duration) error {
	err := c.db(ctx).Expire(ctx, postgres.ExpireParams{
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

// Delete removes one or more keys from the cache.
func (c *Cache) Delete(ctx context.Context, key string) error {
	_, err := c.db(ctx).Delete(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotExist
	}
	return err
}

func (c *Cache) delete(ctx context.Context, key string) (*dto, error) {
	row, err := c.db(ctx).Delete(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	dto := newDto(row)
	return dto, nil
}

func (c *Cache) load(ctx context.Context, key string) (*dto, error) {
	row, err := c.db(ctx).Load(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	d := newDto(row)
	if !d.Valid() {
		err = c.Delete(ctx, key)
		if err != nil {
			return nil, err
		}
		return nil, ErrNotExist
	}

	return d, nil
}

func (c *Cache) deleteExpired(ctx context.Context, key string) (bool, error) {
	_, err := c.db(ctx).DeleteExpired(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *Cache) db(ctx context.Context) postgres.Querier {
	return postgres.New(c.DBTx(ctx))
}
