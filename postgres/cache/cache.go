package cache

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
)

const (
	// DefaultLease is how long LoadOrCreate holds a key while computing its
	// value before the lease must be renewed.
	DefaultLease = 30 * time.Second

	// NoExpiration, passed as a TTL, stores a key that never expires.
	NoExpiration time.Duration = 0

	// minRenewInterval bounds how often a lease is renewed.
	minRenewInterval = 10 * time.Millisecond

	// minLease is the shortest lease that is renewed before it expires.
	minLease = 3 * minRenewInterval

	// releaseTimeout bounds releasing a lease after the caller's context is
	// done.
	releaseTimeout = 5 * time.Second
)

var (
	// Errors.
	ErrConflict        = errors.New("conflict")
	ErrExists          = errors.New("exists")
	ErrNegativeTTL     = errors.New("negative ttl")
	ErrNotExist        = errors.New("not exist")
	ErrRequestInFlight = errors.New("request in flight")
)

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

type Repository interface {
	AcquireLease(ctx context.Context, key string, lease uuid.UUID, ttl time.Duration) (*Entry, bool, error)
	CompareAndDelete(ctx context.Context, key string, value jsontext.Value) error
	CompareAndSwap(ctx context.Context, key string, oldValue, newValue jsontext.Value, ttl time.Duration) error
	Delete(ctx context.Context, key string) (*Entry, error)
	Exists(ctx context.Context, key string) (bool, error)
	Expire(ctx context.Context, key string, ttl time.Duration) error
	FulfillLease(ctx context.Context, key string, lease uuid.UUID, value jsontext.Value, ttl time.Duration) error
	Load(ctx context.Context, key string) (*Entry, error)
	LoadOrStore(ctx context.Context, key string, value jsontext.Value, ttl time.Duration) (*Entry, bool, error)
	Purge(ctx context.Context) (int64, error)
	ReleaseLease(ctx context.Context, key string, lease uuid.UUID) error
	RenewLease(ctx context.Context, key string, lease uuid.UUID, ttl time.Duration) error
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error
	Store(ctx context.Context, key string, value jsontext.Value, ttl time.Duration) (*Entry, error)
	StoreOnce(ctx context.Context, key string, value jsontext.Value, ttl time.Duration) error
	TTL(ctx context.Context, key string) (time.Duration, error)
}

type Option func(*Cache)

// WithPrefix prepends prefix to every key. No separator is added, so include
// one in the prefix, e.g. "books:".
func WithPrefix(prefix string) Option {
	return func(c *Cache) {
		c.prefix = prefix
	}
}

// WithLease sets how long LoadOrCreate holds a key while computing its value.
// The lease is renewed in the background until the value is stored.
// Non-positive values use DefaultLease.
// Leases shorter than 30ms are raised to 30ms, so that they can be renewed
// before they expire.
func WithLease(lease time.Duration) Option {
	return func(c *Cache) {
		c.lease = lease
	}
}

// Cache is safe for concurrent use.
type Cache struct {
	repo   Repository
	prefix string
	lease  time.Duration
}

func New(repo Repository, opts ...Option) *Cache {
	c := &Cache{
		repo: repo,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.lease <= 0 {
		c.lease = DefaultLease
	}
	c.lease = max(c.lease, minLease)
	return c
}

// Prefix returns the prefix prepended to every key.
func (c *Cache) Prefix() string {
	return c.prefix
}

// CompareAndDelete atomically deletes a key only if its current value matches the expected old value.
// Returns ErrNotExist if the key does not exist, or ErrConflict if the value does not match.
func (c *Cache) CompareAndDelete[T any](ctx context.Context, key string, old T) error {
	b, err := json.Marshal(old)
	if err != nil {
		return wrap("compare and delete", key, err)
	}
	return wrap("compare and delete", key, c.repo.CompareAndDelete(ctx, c.buildKey(key), b))
}

// CompareAndSwap atomically updates a key only if its current value matches the expected old value.
// Returns ErrNotExist if the key does not exist, or ErrConflict if the value does not match.
func (c *Cache) CompareAndSwap[T any](ctx context.Context, key string, oldValue, newValue T, ttl time.Duration) error {
	if ttl < 0 {
		return wrap("compare and swap", key, ErrNegativeTTL)
	}
	oldBytes, err := json.Marshal(oldValue)
	if err != nil {
		return wrap("compare and swap", key, err)
	}
	newBytes, err := json.Marshal(newValue)
	if err != nil {
		return wrap("compare and swap", key, err)
	}
	return wrap("compare and swap", key, c.repo.CompareAndSwap(ctx, c.buildKey(key), oldBytes, newBytes, ttl))
}

// Delete removes a key. Returns ErrNotExist if the key does not exist.
// Deleting a key that LoadOrCreate is computing discards the computed value.
func (c *Cache) Delete(ctx context.Context, key string) error {
	_, err := c.repo.Delete(ctx, c.buildKey(key))
	return wrap("delete", key, err)
}

// Expire sets a timeout on a key, after which the key is deleted.
// A ttl of NoExpiration removes the timeout. Returns ErrNotExist if the key does not exist.
func (c *Cache) Expire(ctx context.Context, key string, ttl time.Duration) error {
	if ttl < 0 {
		return wrap("expire", key, ErrNegativeTTL)
	}
	return wrap("expire", key, c.repo.Expire(ctx, c.buildKey(key), ttl))
}

// Load retrieves the value for a key. Returns ErrNotExist if the key doesn't exist.
func (c *Cache) Load[T any](ctx context.Context, key string) (T, error) {
	var zero T
	entry, err := c.repo.Load(ctx, c.buildKey(key))
	if err != nil {
		return zero, wrap("load", key, err)
	}
	v, err := decode[T](entry.Value)
	return v, wrap("load", key, err)
}

// LoadAndDelete atomically retrieves and deletes a key's value.
// Returns ErrNotExist if the key doesn't exist. If LoadOrCreate is computing
// the key, that computation is discarded and ErrNotExist is returned.
func (c *Cache) LoadAndDelete[T any](ctx context.Context, key string) (T, error) {
	var zero T
	entry, err := c.repo.Delete(ctx, c.buildKey(key))
	if err != nil {
		return zero, wrap("load and delete", key, err)
	}
	if entry.Lease != nil {
		return zero, wrap("load and delete", key, ErrNotExist)
	}
	v, err := decode[T](entry.Value)
	return v, wrap("load and delete", key, err)
}

// LoadOrCreate returns the cached value for a key with loaded = true, or
// computes it with fn, caches it for the TTL that fn returns and returns it
// with loaded = false.
//
// While fn runs, the key is leased so that concurrent callers get
// ErrRequestInFlight instead of computing the value again. If the lease is
// lost, for example because the key was deleted or overwritten, fn's value is
// still returned but not cached.
//
// The lease is written outside any transaction in ctx, so that other callers
// see it. fn still runs with ctx, so a value computed from the caller's
// uncommitted writes is cached even if that transaction rolls back.
func (c *Cache) LoadOrCreate[T any](ctx context.Context, key string, fn func(ctx context.Context, key string) (T, time.Duration, error)) (curr T, loaded bool, err error) {
	var zero T
	fullKey := c.buildKey(key)
	lease := uuid.NewV7()

	entry, acquired, err := c.repo.AcquireLease(withoutTx(ctx), fullKey, lease, c.lease)
	if err != nil {
		return zero, false, wrap("load or create", key, err)
	}
	if !acquired {
		if entry.Lease != nil {
			return zero, false, wrap("load or create", key, ErrRequestInFlight)
		}
		v, err := decode[T](entry.Value)
		if err != nil {
			return zero, false, wrap("load or create", key, err)
		}
		return v, true, nil
	}

	// Release the lease unless it was fulfilled or lost, so that other callers
	// do not wait for it to expire. This must outlive ctx, since ctx being done
	// is a common reason to get here.
	held := true
	defer func() {
		if !held {
			return
		}
		ctx, cancel := context.WithTimeout(withoutTx(context.WithoutCancel(ctx)), releaseTimeout)
		defer cancel()
		err := c.repo.ReleaseLease(ctx, fullKey, lease)
		if err != nil && !errors.Is(err, ErrNotExist) {
			slog.ErrorContext(ctx, "releasing cache lease", "err", err, "key", fullKey, "lease", lease)
		}
	}()

	// Stop fn when we return early.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		val T
		ttl time.Duration
		err error
		// panic holds what fn panicked with. fn runs in its own goroutine,
		// where a panic would crash the process, so it is re-raised in the
		// caller's goroutine instead.
		panic any
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- result{panic: p}
			}
		}()
		val, ttl, err := fn(ctx, key)
		ch <- result{val: val, ttl: ttl, err: err}
	}()

	t := time.NewTicker(max(c.lease/3, minRenewInterval))
	defer t.Stop()
	renew := t.C

	for {
		select {
		case <-ctx.Done():
			return zero, false, wrap("load or create", key, context.Cause(ctx))
		case <-renew:
			err := c.repo.RenewLease(withoutTx(ctx), fullKey, lease, c.lease)
			if errors.Is(err, ErrNotExist) {
				// Lease lost. Let fn finish, but do not cache its value.
				held = false
				renew = nil
				continue
			}
			if err != nil {
				return zero, false, wrap("load or create: renewing lease", key, err)
			}
		case res := <-ch:
			if res.panic != nil {
				panic(res.panic) // the deferred release still runs
			}
			if res.err != nil {
				return zero, false, wrap("load or create: executing", key, res.err)
			}
			if res.ttl < 0 {
				return zero, false, wrap("load or create", key, ErrNegativeTTL)
			}
			if !held {
				return res.val, false, nil
			}
			b, err := json.Marshal(res.val)
			if err != nil {
				return zero, false, wrap("load or create", key, err)
			}
			err = c.repo.FulfillLease(withoutTx(ctx), fullKey, lease, b, res.ttl)
			if errors.Is(err, ErrNotExist) {
				held = false
				return res.val, false, nil
			}
			if err != nil {
				return zero, false, wrap("load or create: storing value", key, err)
			}
			held = false
			return res.val, false, nil
		}
	}
}

// LoadOrStore atomically loads a key's value if it exists, or stores the provided value if it doesn't.
// Returns the current value and whether it was loaded (true) or stored (false).
// Returns ErrRequestInFlight if LoadOrCreate is computing the key.
func (c *Cache) LoadOrStore[T any](ctx context.Context, key string, value T, ttl time.Duration) (curr T, loaded bool, err error) {
	var zero T
	if ttl < 0 {
		return zero, false, wrap("load or store", key, ErrNegativeTTL)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return zero, false, wrap("load or store", key, err)
	}

	entry, loaded, err := c.repo.LoadOrStore(ctx, c.buildKey(key), b, ttl)
	if err != nil {
		return zero, false, wrap("load or store", key, err)
	}
	if !loaded {
		return value, false, nil
	}
	if entry.Lease != nil {
		return zero, false, wrap("load or store", key, ErrRequestInFlight)
	}

	v, err := decode[T](entry.Value)
	if err != nil {
		return zero, false, wrap("load or store", key, err)
	}
	return v, true, nil
}

// Store sets a key's value with the specified TTL, or NoExpiration.
// It overwrites any existing value, and discards the value LoadOrCreate is computing for the key.
func (c *Cache) Store[T any](ctx context.Context, key string, value T, ttl time.Duration) error {
	if ttl < 0 {
		return wrap("store", key, ErrNegativeTTL)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return wrap("store", key, err)
	}
	_, err = c.repo.Store(ctx, c.buildKey(key), b, ttl)
	return wrap("store", key, err)
}

// StoreOnce stores a key's value only if the key doesn't already exist.
// Returns ErrExists if it does, including while LoadOrCreate is computing it.
func (c *Cache) StoreOnce[T any](ctx context.Context, key string, value T, ttl time.Duration) error {
	if ttl < 0 {
		return wrap("store once", key, ErrNegativeTTL)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return wrap("store once", key, err)
	}
	return wrap("store once", key, c.repo.StoreOnce(ctx, c.buildKey(key), b, ttl))
}

// Exists checks if a key exists in the cache.
// A key that LoadOrCreate is still computing does not exist yet.
func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	ok, err := c.repo.Exists(ctx, c.buildKey(key))
	return ok, wrap("exists", key, err)
}

// TTL returns the remaining time to live for a key, or NoExpiration if the key does not expire.
// Returns ErrNotExist if the key does not exist.
func (c *Cache) TTL(ctx context.Context, key string) (time.Duration, error) {
	ttl, err := c.repo.TTL(ctx, c.buildKey(key))
	return ttl, wrap("ttl", key, err)
}

// Purge deletes all expired keys and returns how many were deleted.
// Expired keys are also deleted lazily on access.
// Purge applies to every key, regardless of the cache's prefix.
func (c *Cache) Purge(ctx context.Context) (int64, error) {
	n, err := c.repo.Purge(ctx)
	if err != nil {
		return 0, fmt.Errorf("cache: purge: %w", err)
	}
	return n, nil
}

// txless hides dbtx transactions from the repository. Without it, the lease
// would be invisible to other connections until the caller commits, and
// renewing it would use the caller's connection while fn may be using it too.
type txless struct {
	context.Context
}

func withoutTx(ctx context.Context) context.Context {
	return txless{ctx}
}

func (c txless) Value(key any) any {
	v := c.Context.Value(key)
	if _, ok := v.(*dbtx.Tx); ok {
		return nil
	}
	return v
}

func (c *Cache) buildKey(key string) string {
	return c.prefix + key
}

func decode[T any](b jsontext.Value) (T, error) {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}

func wrap(op, key string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("cache: %s %q: %w", op, key, err)
}
