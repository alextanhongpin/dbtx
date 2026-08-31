package cache

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"time"

	_ "embed"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/cache/internal/postgres"
	"github.com/alextanhongpin/dbtx/postgres/lock"
)

var ErrNotFound = errors.New("cache: not found")

type Cache struct {
	*dbtx.DB
}

// CompareAndDelete atomically deletes a key only if its current value matches the expected old value.
func (c *Cache) CompareAndDelete[T any](ctx context.Context, key string, old T) error {
	row, err := newRow(key, old, 0)
	if err != nil {
		return err
	}
	n, err := c.db(ctx).CompareAndDelete(ctx, postgres.CompareAndDeleteParams{
		Key:    key,
		Digest: row.Digest,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CompareAndSwap atomically updates a key only if its current value matches the expected old value.
func (c *Cache) CompareAndSwap[T any](ctx context.Context, key string, old, value T, ttl time.Duration) error {
	deleted, err := c.deleteExpired(ctx, key)
	if err != nil {
		return err
	}
	if deleted {
		return ErrNotFound
	}
	oldDto, err := newRow(key, old, 0)
	if err != nil {
		return err
	}
	newDto, err := newRow(key, value, ttl)
	if err != nil {
		return err
	}
	n, err := c.db(ctx).CompareAndSwap(ctx, postgres.CompareAndSwapParams{
		Value:  newDto.Value,
		Key:    key,
		Digest: oldDto.Digest,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Load retrieves the value for a key. Returns ErrNotFound if the key doesn't exist.
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
	var t T
	var d dto
	err = c.DBTx(ctx).QueryRowContext(ctx, loadAndDeleteStmt, key).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	return d.Load[T]()
}

// LoadOrStore atomically loads a key's value if it exists, or stores the provided value if it doesn't.
// Returns the current value and whether it was loaded (true) or stored (false).
func (c *Cache) LoadOrStore[T any](ctx context.Context, key string, value T, ttl time.Duration) (curr T, loaded bool, err error) {
	_, err = c.deleteExpired(ctx, key)
	if err != nil {
		return curr, false, err
	}

	var t T
	b, digest, err := hash(value)
	if err != nil {
		return t, false, err
	}

	expiresAt := sql.NullTime{
		Time:  time.Now().Add(ttl),
		Valid: ttl > 0,
	}

	var d dto
	err = c.DBTx(ctx).QueryRowContext(ctx, loadOrStoreStmt, key, b, digest, expiresAt).Scan(&d)
	if err != nil {
		return t, false, err
	}
	if err := json.Unmarshal(d.Value, &t); err != nil {
		return t, false, err
	}
	loaded = d.Digest != digest
	return t, loaded, nil
}

func (c *Cache) LoadOrCreate[T any](ctx context.Context, key string, fn func(ctx context.Context, key string) (T, time.Duration, error)) (curr T, loaded bool, err error) {
	err = c.RunInTx(ctx, func(txCtx context.Context) error {
		if err := lock.Lock(txCtx, lock.NewStrKey(key)); err != nil {
			return err
		}
		dto, err := c.load(txCtx, key)
		if err != nil {
			return err
		}
		if !dto.IsExpired() {
			v, err := dto.Load[T]()
			if err != nil {
				return err
			}
			loaded = true
			curr = v
			return nil
		}
		newVal, ttl, err := fn(txCtx, key)
		if err != nil {
			return err
		}
		if err := c.Store(txCtx, key, newVal, ttl); err != nil {
			return err
		}
		curr = newVal
		loaded = false
		return nil
	})
	return
}

// Store sets a key's value with the specified TTL.
func (c *Cache) Store[T any](ctx context.Context, key string, value T, ttl time.Duration) error {
	b, digest, err := hash(value)
	if err != nil {
		return err
	}
	expiresAt := sql.NullTime{
		Time:  time.Now().Add(ttl),
		Valid: ttl > 0,
	}
	_, err = c.DBTx(ctx).ExecContext(ctx, storeStmt, key, b, digest, expiresAt)
	return err
}

// StoreOnce stores a key's value only if the key doesn't already exist.
func (c *Cache) StoreOnce[T any](ctx context.Context, key string, value T, ttl time.Duration) error {
	_, err := c.deleteExpired(ctx, key)
	if err != nil {
		return err
	}

	b, digest, err := hash(value)
	if err != nil {
		return err
	}
	expiresAt := sql.NullTime{
		Time:  time.Now().Add(ttl),
		Valid: ttl > 0,
	}
	res, err := c.DBTx(ctx).ExecContext(ctx, storeOnceStmt, key, b, digest, expiresAt)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// Exists checks if a key exists in the cache.
func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	d, err := c.load(ctx, key)
	if err != nil {
		return false, err
	}
	if d.IsExpired() {
		_, err := c.Delete(ctx, key)
		if err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// TTL returns the remaining time to live for a key.
// Returns -1 if the key exists but has no expiration.
// Returns -2 if the key does not exist.
func (c *Cache) TTL(ctx context.Context, key string) (time.Duration, error) {
	var expiresAt sql.NullTime
	err := c.DBTx(ctx).QueryRowContext(ctx, ttlStmt, key).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return -2, nil
	}
	if err != nil {
		return 0, err
	}
	if !expiresAt.Valid {
		return -1, nil
	}
	return time.Until(expiresAt.Time), nil
}

// Expire sets a timeout on a key. After the timeout has expired, the key will automatically be deleted.
func (c *Cache) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return c.db(ctx).Expire(ctx, postgres.ExpireParams{
		ExpiresAt: sql.NullTime{
			Time:  time.Now().Add(ttl),
			Valid: ttl > 0,
		},
		Key: key,
	})
}

// Delete removes one or more keys from the cache.
func (c *Cache) Delete(ctx context.Context, key string) error {
	_, err := c.db(ctx).Delete(ctx, key)
	return err
}

func (c *Cache) delete(ctx context.Context, key string) (*dto, error) {
	row, err := c.db(ctx).Delete(ctx, key)
	if err != nil {
		return nil, err
	}
	dto := newDto(row)
	return dto, nil
}

func (c *Cache) load(ctx context.Context, key string) (*dto, error) {
	row, err := c.db(ctx).Load(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d := newDto(row)
	if d.Valid() {
		return d, nil
	}

	err = c.Delete(ctx, key)
	if err != nil {
		return nil, err
	}
	return nil, ErrNotFound
}

func (c *Cache) deleteExpired(ctx context.Context, key string) (bool, error) {
	n, err := c.db(ctx).DeleteExpired(ctx, key)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (c *Cache) db(ctx context.Context) postgres.Querier {
	return postgres.New(c.DBTx(ctx))
}
