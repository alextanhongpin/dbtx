package repository

import (
	_ "embed"

	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/cache"
	"github.com/alextanhongpin/dbtx/postgres/cache/repository/postgres"
)

//go:embed schema.sql
var Schema string

var _ cache.Repository = (*Repository)(nil)

// loadOrStoreAttempts bounds the retries when LoadOrStore races with a
// concurrent write that is not visible to the statement's snapshot.
const loadOrStoreAttempts = 3

type Repository struct {
	*dbtx.DB
}

func New(db *sql.DB) *Repository {
	return &Repository{
		DB: dbtx.New(db),
	}
}

// CompareAndDelete returns ErrNotExist if the key does not exist, or
// ErrConflict if it exists with a different value.
func (r *Repository) CompareAndDelete(ctx context.Context, key string, value jsontext.Value) error {
	row, err := r.db(ctx).CompareAndDelete(ctx, postgres.CompareAndDeleteParams{
		Key:   key,
		Value: value,
	})
	if err != nil {
		return err
	}
	return compareResult(row.Found, row.Deleted)
}

// CompareAndSwap returns ErrNotExist if the key does not exist, or
// ErrConflict if it exists with a different value.
func (r *Repository) CompareAndSwap(ctx context.Context, key string, oldValue, newValue jsontext.Value, ttl time.Duration) error {
	row, err := r.db(ctx).CompareAndSwap(ctx, postgres.CompareAndSwapParams{
		Key:      key,
		OldValue: oldValue,
		NewValue: newValue,
		Ttl:      ttlParam(ttl),
	})
	if err != nil {
		return err
	}
	return compareResult(row.Found, row.Swapped)
}

// Delete deletes the key, including a lease placeholder, and returns the
// deleted entry.
func (r *Repository) Delete(ctx context.Context, key string) (*cache.Entry, error) {
	row, err := r.db(ctx).Delete(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, cache.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return newEntry(row)
}

func (r *Repository) Exists(ctx context.Context, key string) (bool, error) {
	return r.db(ctx).Exists(ctx, key)
}

func (r *Repository) Expire(ctx context.Context, key string, ttl time.Duration) error {
	_, err := r.db(ctx).Expire(ctx, postgres.ExpireParams{
		Key: key,
		Ttl: ttlParam(ttl),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return cache.ErrNotExist
	}
	return err
}

func (r *Repository) Load(ctx context.Context, key string) (*cache.Entry, error) {
	row, err := r.db(ctx).Load(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, cache.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return newEntry(row)
}

// TTL returns the remaining time to live measured by the database clock, or 0
// if the key does not expire.
func (r *Repository) TTL(ctx context.Context, key string) (time.Duration, error) {
	us, err := r.db(ctx).TTL(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, cache.ErrNotExist
	}
	if err != nil {
		return 0, err
	}
	return time.Duration(us) * time.Microsecond, nil
}

func (r *Repository) Purge(ctx context.Context) (int64, error) {
	return r.db(ctx).Purge(ctx)
}

func (r *Repository) Store(ctx context.Context, key string, value jsontext.Value, ttl time.Duration) (*cache.Entry, error) {
	row, err := r.db(ctx).Store(ctx, postgres.StoreParams{
		Key:   key,
		Value: value,
		Ttl:   ttlParam(ttl),
	})
	if err != nil {
		return nil, err
	}
	return newEntry(row)
}

// StoreOnce returns ErrExists if a live entry, including a lease placeholder,
// already exists.
func (r *Repository) StoreOnce(ctx context.Context, key string, value jsontext.Value, ttl time.Duration) error {
	n, err := r.db(ctx).StoreOnce(ctx, postgres.StoreOnceParams{
		Key:   key,
		Value: value,
		Ttl:   ttlParam(ttl),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return cache.ErrExists
	}
	return nil
}

// LoadOrStore returns the existing live entry with loaded = true, or stores
// the value and returns it with loaded = false. The returned entry may be a
// lease placeholder; check cache.Entry.Lease.
func (r *Repository) LoadOrStore(ctx context.Context, key string, value jsontext.Value, ttl time.Duration) (*cache.Entry, bool, error) {
	return r.loadOrStore(ctx, postgres.LoadOrStoreParams{
		Key:   key,
		Value: value,
		Ttl:   ttlParam(ttl),
	})
}

// AcquireLease stores a lease placeholder for the key unless a live entry
// exists. It returns the existing entry with acquired = false otherwise.
func (r *Repository) AcquireLease(ctx context.Context, key string, lease uuid.UUID, ttl time.Duration) (*cache.Entry, bool, error) {
	entry, loaded, err := r.loadOrStore(ctx, postgres.LoadOrStoreParams{
		Key:   key,
		Value: jsontext.Value("null"),
		Lease: sql.NullString{String: lease.String(), Valid: true},
		Ttl:   ttlParam(ttl),
	})
	return entry, !loaded, err
}

// RenewLease extends the lease. It returns ErrNotExist if the lease is no
// longer held.
func (r *Repository) RenewLease(ctx context.Context, key string, lease uuid.UUID, ttl time.Duration) error {
	n, err := r.db(ctx).RenewLease(ctx, postgres.RenewLeaseParams{
		Key:   key,
		Lease: lease.String(),
		Ttl:   ttlParam(ttl),
	})
	return affected(n, err)
}

// ReleaseLease deletes the lease placeholder. It returns ErrNotExist if the
// lease is no longer held.
func (r *Repository) ReleaseLease(ctx context.Context, key string, lease uuid.UUID) error {
	n, err := r.db(ctx).ReleaseLease(ctx, postgres.ReleaseLeaseParams{
		Key:   key,
		Lease: lease.String(),
	})
	return affected(n, err)
}

// FulfillLease replaces the lease placeholder with the value. It returns
// ErrNotExist if the lease is no longer held.
func (r *Repository) FulfillLease(ctx context.Context, key string, lease uuid.UUID, value jsontext.Value, ttl time.Duration) error {
	n, err := r.db(ctx).FulfillLease(ctx, postgres.FulfillLeaseParams{
		Key:   key,
		Lease: lease.String(),
		Value: value,
		Ttl:   ttlParam(ttl),
	})
	return affected(n, err)
}

func (r *Repository) loadOrStore(ctx context.Context, arg postgres.LoadOrStoreParams) (*cache.Entry, bool, error) {
	for range loadOrStoreAttempts {
		row, err := r.db(ctx).LoadOrStore(ctx, arg)
		if errors.Is(err, sql.ErrNoRows) {
			// A concurrent insert, or a write replacing an expired row, committed
			// after the snapshot was taken. A new statement gets a new snapshot
			// (under READ COMMITTED), so retry.
			continue
		}
		if err != nil {
			return nil, false, err
		}

		entry, err := newEntry(&postgres.DbtxCache{
			Key:       row.Key,
			Value:     row.Value,
			Lease:     row.Lease,
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
			ExpiresAt: row.ExpiresAt,
		})
		if err != nil {
			return nil, false, err
		}
		return entry, row.Loaded, nil
	}

	return nil, false, fmt.Errorf("%w: concurrent write to key %q", cache.ErrConflict, arg.Key)
}

func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}

func newEntry(row *postgres.DbtxCache) (*cache.Entry, error) {
	var expiresAt *time.Time
	if row.ExpiresAt.Valid {
		expiresAt = new(row.ExpiresAt.Time)
	}

	var lease *uuid.UUID
	if row.Lease.Valid {
		id, err := uuid.Parse(row.Lease.String)
		if err != nil {
			return nil, fmt.Errorf("parsing lease: %w", err)
		}
		lease = &id
	}

	return &cache.Entry{
		Key:       row.Key,
		Value:     row.Value,
		Lease:     lease,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
		ExpiresAt: expiresAt,
	}, nil
}

// ttlParam converts a TTL to microseconds, the resolution of timestamptz.
// A zero TTL means no expiration.
func ttlParam(ttl time.Duration) sql.NullInt64 {
	// Round up, so that a TTL below 1µs does not expire as it is written.
	us := (ttl + time.Microsecond - 1) / time.Microsecond
	return sql.NullInt64{
		Int64: int64(us),
		Valid: ttl != 0,
	}
}

func compareResult(found, ok bool) error {
	switch {
	case ok:
		return nil
	case found:
		return cache.ErrConflict
	default:
		return cache.ErrNotExist
	}
}

func affected(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return cache.ErrNotExist
	}
	return nil
}
