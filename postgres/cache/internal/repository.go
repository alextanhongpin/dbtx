package internal

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/cache/internal/postgres"
)

// loadOrStoreAttempts bounds the retries when LoadOrStore races with a
// concurrent insert that is not visible to the statement's snapshot.
const loadOrStoreAttempts = 3

var (
	ErrConflict = errors.New("conflict")
	ErrExists   = errors.New("exists")
	ErrNotExist = errors.New("not exist")
)

type Repository struct {
	*dbtx.DB
}

type Entry struct {
	Key   string
	Value jsontext.Value
	// Lease is non-nil when the entry is a placeholder held by a caller that
	// is still computing the value. Value is meaningless in that case.
	Lease     *uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt *time.Time
}

func NewRepository(db *sql.DB) *Repository {
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
func (r *Repository) Delete(ctx context.Context, key string) (*Entry, error) {
	row, err := r.db(ctx).Delete(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotExist
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
		return ErrNotExist
	}
	return err
}

func (r *Repository) Load(ctx context.Context, key string) (*Entry, error) {
	row, err := r.db(ctx).Load(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotExist
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
		return 0, ErrNotExist
	}
	if err != nil {
		return 0, err
	}
	return time.Duration(us) * time.Microsecond, nil
}

func (r *Repository) Purge(ctx context.Context) (int64, error) {
	return r.db(ctx).Purge(ctx)
}

func (r *Repository) Store(ctx context.Context, key string, value jsontext.Value, ttl time.Duration) (*Entry, error) {
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
		return ErrExists
	}
	return nil
}

// LoadOrStore returns the existing live entry with loaded = true, or stores
// the value and returns it with loaded = false. The returned entry may be a
// lease placeholder; check Entry.Lease.
func (r *Repository) LoadOrStore(ctx context.Context, key string, value jsontext.Value, ttl time.Duration) (*Entry, bool, error) {
	return r.loadOrStore(ctx, postgres.LoadOrStoreParams{
		Key:   key,
		Value: value,
		Ttl:   ttlParam(ttl),
	})
}

// AcquireLease stores a lease placeholder for the key unless a live entry
// exists. It returns the existing entry with acquired = false otherwise.
func (r *Repository) AcquireLease(ctx context.Context, key string, lease uuid.UUID, ttl time.Duration) (*Entry, bool, error) {
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

func (r *Repository) loadOrStore(ctx context.Context, arg postgres.LoadOrStoreParams) (*Entry, bool, error) {
	for range loadOrStoreAttempts {
		row, err := r.db(ctx).LoadOrStore(ctx, arg)
		if errors.Is(err, sql.ErrNoRows) {
			// A concurrent insert committed after the snapshot was taken. A new
			// statement gets a new snapshot (under READ COMMITTED), so retry.
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

	return nil, false, fmt.Errorf("%w: concurrent write to key %q", ErrConflict, arg.Key)
}

func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}

func newEntry(row *postgres.DbtxCache) (*Entry, error) {
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

	return &Entry{
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
	return sql.NullInt64{
		Int64: ttl.Microseconds(),
		Valid: ttl != 0,
	}
}

func compareResult(found, ok bool) error {
	switch {
	case ok:
		return nil
	case found:
		return ErrConflict
	default:
		return ErrNotExist
	}
}

func affected(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotExist
	}
	return nil
}
