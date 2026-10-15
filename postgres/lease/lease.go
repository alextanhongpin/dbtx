// Package lease coordinates PostgreSQL ownership and fences short database
// transactions. Activities can overlap after expiry; guarded commits reject
// expired owners. External effects require their own fencing or idempotency.
package lease

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	randv2 "math/rand/v2"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/internal/renew"
	"github.com/alextanhongpin/dbtx/postgres/lease/repository/postgres"
	"github.com/lib/pq"
)

var (
	ErrContended      = errors.New("lease: resource is held")
	ErrLeaseLost      = errors.New("lease: ownership lost or expired")
	ErrReleased       = errors.New("lease: released")
	ErrInvalidConfig  = errors.New("lease: invalid configuration")
	ErrInvalidOption  = errors.New("lease: invalid acquisition option")
	ErrTxInContext    = errors.New("lease: context carries a transaction")
	ErrLeaseBusy      = errors.New("lease: lifecycle operation already in progress")
	ErrTokenExhausted = errors.New("lease: fencing token exhausted")
)

// Config is copied at construction. Zero durations select defaults. DB and its
// pool are caller-owned and must outlive operations; do not change DB's ID.
// Requires PostgreSQL 17+ for server-side transaction_timeout.
type Config struct {
	DB                 *dbtx.DB
	Namespace          string
	LeaseDuration      time.Duration // default 30s, minimum 6ms
	TransactionTimeout time.Duration // default 2s
	RequestTimeout     time.Duration // default 5s; lease requests also limited by TTL
	CleanupTimeout     time.Duration // default 2s
	RetryInterval      time.Duration // default 50ms, jittered during contention
}

func (c Config) WithDefaults() Config {
	if c.LeaseDuration == 0 {
		c.LeaseDuration = 30 * time.Second
	}
	if c.TransactionTimeout == 0 {
		c.TransactionTimeout = 2 * time.Second
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 5 * time.Second
	}
	if c.CleanupTimeout == 0 {
		c.CleanupTimeout = 2 * time.Second
	}
	if c.RetryInterval == 0 {
		c.RetryInterval = 50 * time.Millisecond
	}
	return c
}

func (c Config) Validate() error {
	if c.DB == nil || c.DB.Unwrap() == nil || c.DB.ID() == "" || c.Namespace == "" {
		return fmt.Errorf("%w: DB, its ID and namespace are required", ErrInvalidConfig)
	}
	if c.LeaseDuration < 6*time.Millisecond || c.TransactionTimeout < time.Millisecond ||
		c.RequestTimeout < time.Millisecond || c.CleanupTimeout < time.Millisecond || c.RetryInterval < time.Millisecond {
		return fmt.Errorf("%w: lease duration must be >=6ms, other durations >=1ms", ErrInvalidConfig)
	}
	if c.TransactionTimeout > 2147483647*time.Millisecond {
		return fmt.Errorf("%w: transaction timeout exceeds PostgreSQL timer range", ErrInvalidConfig)
	}
	return nil
}

// Locker can be reused concurrently. Construction neither installs schema nor
// starts background work. Every successful acquisition owns an expiry monitor.
type Locker struct{ cfg Config }

func New(cfg Config) (*Locker, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Locker{cfg: cfg}, nil
}

// Lease exposes the generation of this acquisition, not a current-token lookup.
// Renew/Release/RunInFencedTx reject concurrent lifecycle mutations with
// ErrLeaseBusy (including reentrant calls from a fenced callback). User calls
// wait for an in-flight automatic heartbeat, bounded by their own context.
// Done/Err/Token may be inspected concurrently and from callbacks.
type Lease interface {
	Token() uint64
	Done() <-chan struct{}
	Err() error
	Renew(context.Context) error
	Release(context.Context) error
	RunInFencedTx(context.Context, func(context.Context) error) error
}

type acquireOptions struct{ ttl time.Duration }

// LockOption configures acquisition. Nil options are invalid.
type LockOption func(*acquireOptions) error

func WithLeaseDuration(ttl time.Duration) LockOption {
	return func(o *acquireOptions) error {
		if ttl < 6*time.Millisecond {
			return fmt.Errorf("%w: lease duration must be >=6ms", ErrInvalidOption)
		}
		o.ttl = ttl
		return nil
	}
}

// Acquire waits on contention until ctx ends. It does not automatically renew.
func (l *Locker) Acquire(ctx context.Context, key string, opts ...LockOption) (Lease, error) {
	return l.acquire(ctx, key, false, opts...)
}

// TryAcquire attempts once; contention returns ErrContended. Pool and network
// latency are still bounded by ctx and RequestTimeout.
func (l *Locker) TryAcquire(ctx context.Context, key string, opts ...LockOption) (Lease, error) {
	return l.acquire(ctx, key, true, opts...)
}

func (l *Locker) checkContext(ctx context.Context) error {
	if l.cfg.DB.IsTx(ctx) || dbtx.IsTx(ctx) {
		return ErrTxInContext
	}
	return context.Cause(ctx)
}

func (l *Locker) acquire(ctx context.Context, key string, try bool, opts ...LockOption) (Lease, error) {
	if err := l.checkContext(ctx); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("%w: key is required", ErrInvalidOption)
	}
	o := acquireOptions{ttl: l.cfg.LeaseDuration}
	for _, opt := range opts {
		if opt == nil {
			return nil, ErrInvalidOption
		}
		if err := opt(&o); err != nil {
			return nil, err
		}
	}
	for {
		owner := rand.Text()
		sent := time.Now()
		reqCtx, cancel := context.WithTimeout(ctx, min(l.cfg.RequestTimeout, o.ttl/3))
		token, err := postgres.New(l.cfg.DB.DB()).AcquireLease(reqCtx, postgres.AcquireLeaseParams{
			Namespace: l.cfg.Namespace, Key: key, OwnerID: owner, TtlUs: int64(o.ttl / time.Microsecond),
		})
		cancel()
		if err != nil {
			// The grant may have committed despite an unknown response. Owner-only
			// cleanup cannot clear a successor; TTL bounds effects if cleanup fails.
			cleanupErr := l.cleanupOwner(ctx, key, owner)
			var pgErr *pq.Error
			if errors.As(err, &pgErr) && pgErr.Code == "22003" {
				err = errors.Join(ErrTokenExhausted, err)
			}
			return nil, errors.Join(fmt.Errorf("lease: acquire: %w", err), context.Cause(ctx), cleanupErr)
		}
		if token > 0 {
			deadline := renew.Deadline(sent, o.ttl)
			if context.Cause(ctx) != nil || !time.Now().Before(deadline) {
				return nil, errors.Join(ErrLeaseLost, context.Cause(ctx), l.cleanupOwner(ctx, key, owner))
			}
			return newLease(l, key, owner, token, o.ttl, deadline), nil
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		if try {
			return nil, ErrContended
		}
		delay := l.cfg.RetryInterval/2 + time.Duration(randv2.Int64N(int64(l.cfg.RetryInterval/2)+1))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// cleanupOwner does not need the token if an acquire response was lost.
func (l *Locker) cleanupOwner(ctx context.Context, key, owner string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.CleanupTimeout)
	defer cancel()
	_, err := postgres.New(l.cfg.DB.DB()).ReleaseOwner(cleanupCtx, postgres.ReleaseOwnerParams{
		Namespace: l.cfg.Namespace, Key: key, OwnerID: sql.NullString{String: owner, Valid: true},
	})
	return err
}
