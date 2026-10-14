// Package lease coordinates renewable PostgreSQL ownership of a resource and
// fences short database transactions. It follows postgres/lease: activities
// can overlap after expiry, while fenced transactions reject expired owners.
// External effects need their own fencing or idempotency.
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
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/engine"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// Re-exported errors; they are the shared durable errors.
var (
	ErrContended   = durable.ErrContended
	ErrLeaseLost   = durable.ErrLeaseLost
	ErrReleased    = durable.ErrReleased
	ErrLeaseBusy   = durable.ErrLeaseBusy
	ErrTxInContext = durable.ErrTxInContext
	ErrStaleToken  = durable.ErrStaleToken
)

// Config is copied at construction. Zero durations select defaults. DB is
// caller-owned; do not change its ID after construction.
type Config struct {
	DB                 *dbtx.DB
	Namespace          string
	LeaseDuration      time.Duration // default 30s, minimum 6ms
	TransactionTimeout time.Duration // default 2s
	RequestTimeout     time.Duration // default 5s; also capped at TTL/3
	CleanupTimeout     time.Duration // default 2s
	RetryInterval      time.Duration // default 50ms, jittered
	Observer           durable.Observer
	Hooks              *durable.Hooks
}

func (c Config) withDefaults() Config {
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

func (c Config) validate() error {
	if c.DB == nil || c.DB.Unwrap() == nil || c.DB.ID() == "" || c.Namespace == "" {
		return fmt.Errorf("%w: DB, its ID and Namespace are required", durable.ErrInvalidConfig)
	}
	if c.LeaseDuration < 6*time.Millisecond || c.TransactionTimeout < time.Millisecond ||
		c.RequestTimeout < time.Millisecond || c.CleanupTimeout < time.Millisecond || c.RetryInterval < time.Millisecond {
		return fmt.Errorf("%w: lease duration must be >= 6ms, other durations >= 1ms", durable.ErrInvalidConfig)
	}
	return nil
}

// Locker is safe for concurrent use. Construction installs no schema and
// starts no background work.
type Locker struct{ cfg Config }

func New(cfg Config) (*Locker, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Locker{cfg: cfg}, nil
}

// Lease is one acquisition. Token is this acquisition's generation for its
// namespace/key; never replace it with a lookup of the latest token.
type Lease interface {
	Key() string
	Token() int64
	Done() <-chan struct{}
	Err() error
	Renew(context.Context) error
	Release(context.Context) error
	// RunInFencedTx runs fn in a short transaction admitted only while this
	// lease is still valid; successors wait until it commits or rolls back.
	RunInFencedTx(context.Context, func(txCtx context.Context) error) error
}

// Option configures one acquisition.
type Option func(*options) error

type options struct{ ttl time.Duration }

// WithLeaseDuration overrides the TTL for one acquisition.
func WithLeaseDuration(ttl time.Duration) Option {
	return func(o *options) error {
		if ttl < 6*time.Millisecond {
			return fmt.Errorf("%w: lease duration must be >= 6ms", durable.ErrInvalidArgument)
		}
		o.ttl = ttl
		return nil
	}
}

// Acquire waits through contention until ctx ends. It does not renew
// automatically: call Renew, or use Activity.
func (l *Locker) Acquire(ctx context.Context, key string, opts ...Option) (Lease, error) {
	return l.acquire(ctx, key, false, opts...)
}

// TryAcquire attempts once and returns ErrContended when the key is held.
func (l *Locker) TryAcquire(ctx context.Context, key string, opts ...Option) (Lease, error) {
	return l.acquire(ctx, key, true, opts...)
}

func (l *Locker) checkContext(ctx context.Context) error {
	if l.cfg.DB.IsTx(ctx) || dbtx.IsTx(ctx) {
		return ErrTxInContext
	}
	return context.Cause(ctx)
}

func (l *Locker) acquire(ctx context.Context, key string, try bool, opts ...Option) (*held, error) {
	if err := l.checkContext(ctx); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("%w: key is required", durable.ErrInvalidArgument)
	}
	o := options{ttl: l.cfg.LeaseDuration}
	for _, opt := range opts {
		if opt == nil {
			return nil, durable.ErrInvalidArgument
		}
		if err := opt(&o); err != nil {
			return nil, err
		}
	}
	for {
		owner := rand.Text()
		sent := time.Now()
		reqCtx, cancel := context.WithTimeout(ctx, min(l.cfg.RequestTimeout, o.ttl/3))
		token, err := postgres.New(l.cfg.DB.DB()).AcquireResourceLease(reqCtx, postgres.AcquireResourceLeaseParams{
			Namespace: l.cfg.Namespace, Key: key, OwnerID: owner, TtlUs: pg.Micros(o.ttl),
		})
		cancel()
		if err != nil {
			cleanupErr := l.cleanupOwner(ctx, key, owner)
			if pg.SQLState(err) == pg.CodeNumericRange {
				err = errors.Join(durable.ErrTokenExhausted, err)
			}
			return nil, errors.Join(fmt.Errorf("lease: acquire: %w", err), context.Cause(ctx), cleanupErr)
		}
		if token > 0 {
			if context.Cause(ctx) != nil || !time.Now().Before(engine.SafeDeadline(sent, o.ttl)) {
				return nil, errors.Join(ErrLeaseLost, context.Cause(ctx), l.cleanupOwner(ctx, key, owner))
			}
			return l.newHeld(key, owner, token, o.ttl, sent), nil
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		if try {
			return nil, ErrContended
		}
		half := l.cfg.RetryInterval / 2
		timer := time.NewTimer(half + time.Duration(randv2.Int64N(int64(half)+1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// cleanupOwner clears a grant whose response was lost. It cannot clear a
// successor because a successor has a different owner ID.
func (l *Locker) cleanupOwner(ctx context.Context, key, owner string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.cfg.CleanupTimeout)
	defer cancel()
	_, err := postgres.New(l.cfg.DB.DB()).ReleaseResourceOwner(ctx, postgres.ReleaseResourceOwnerParams{
		Namespace: l.cfg.Namespace, Key: key, OwnerID: sql.NullString{String: owner, Valid: true},
	})
	return err
}

type held struct {
	locker     *Locker
	key, owner string
	token      int64
	keeper     *engine.Keeper
	released   bool
}

func (l *Locker) newHeld(key, owner string, token int64, ttl time.Duration, sent time.Time) *held {
	h := &held{locker: l, key: key, owner: owner, token: token}
	h.keeper = engine.NewKeeper(ttl, l.cfg.RequestTimeout, sent, h.renewDB, func(ctx context.Context) {
		l.cfg.Hooks.CallBeforeHeartbeat(ctx, key)
	})
	return h
}

func (h *held) renewDB(ctx context.Context) (bool, error) {
	return postgres.New(h.locker.cfg.DB.DB()).RenewResourceLease(ctx, postgres.RenewResourceLeaseParams{
		Namespace: h.locker.cfg.Namespace, Key: h.key, OwnerID: h.owner,
		Token: h.token, TtlUs: pg.Micros(h.keeper.TTL()),
	})
}

func (h *held) Key() string           { return h.key }
func (h *held) Token() int64          { return h.token }
func (h *held) Done() <-chan struct{} { return h.keeper.Done() }
func (h *held) Err() error            { return h.keeper.Err() }

func (h *held) Renew(ctx context.Context) error {
	if err := h.locker.checkContext(ctx); err != nil {
		return err
	}
	return h.keeper.Renew(ctx, false)
}

// Release ends local ownership before the remote request. A failed remote
// release may be retried; it never clears a successor.
func (h *held) Release(ctx context.Context) error {
	if h.locker.cfg.DB.IsTx(ctx) || dbtx.IsTx(ctx) {
		return ErrTxInContext
	}
	if err := h.keeper.BeginOp(ctx, false); err != nil {
		return err
	}
	defer h.keeper.EndOp()
	h.keeper.Close()
	if h.released {
		return nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, h.locker.cfg.CleanupTimeout)
	defer cancel()
	_, err := postgres.New(h.locker.cfg.DB.DB()).ReleaseResourceLease(reqCtx, postgres.ReleaseResourceLeaseParams{
		Namespace: h.locker.cfg.Namespace, Key: h.key,
		OwnerID: sql.NullString{String: h.owner, Valid: true}, Token: h.token,
	})
	if err == nil {
		h.released = true
	}
	return err
}

func (h *held) RunInFencedTx(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil {
		return durable.ErrInvalidArgument
	}
	if err := h.locker.checkContext(ctx); err != nil {
		return err
	}
	if err := h.keeper.BeginOp(ctx, false); err != nil {
		return err
	}
	defer h.keeper.EndOp()
	if err := h.keeper.Err(); err != nil {
		return &durable.TxOutcomeError{Outcome: durable.TxRolledBack, Err: err}
	}
	cfg := h.locker.cfg
	txCtx, cancel := context.WithTimeout(ctx, cfg.TransactionTimeout)
	defer cancel()
	txCtx = dbtx.WithNamedTxOptions(txCtx, cfg.DB.ID(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	defer h.keeper.Unprotect()
	callbackDone := false
	err := cfg.DB.RunInTx(txCtx, func(txCtx context.Context) error {
		q := postgres.New(cfg.DB.DBTx(txCtx))
		if err := q.SetLocalTimeouts(txCtx, pg.Timeout(cfg.TransactionTimeout)); err != nil {
			return err
		}
		valid, err := q.LockResourceLease(txCtx, postgres.LockResourceLeaseParams{
			OwnerID: h.owner, Token: h.token, Namespace: cfg.Namespace, Key: h.key,
		})
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !valid) {
			h.keeper.Lose(ErrLeaseLost)
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		deadline, _ := txCtx.Deadline()
		if err := h.keeper.Protect(deadline); err != nil {
			return err
		}
		if err := fn(txCtx); err != nil {
			return err
		}
		if err := context.Cause(txCtx); err != nil {
			return err
		}
		callbackDone = true
		return nil
	})
	if err != nil {
		outcome := durable.TxRolledBack
		if callbackDone {
			outcome = durable.TxUnknown
		}
		return &durable.TxOutcomeError{Outcome: outcome, Err: err}
	}
	return nil
}

// CheckFenced converts a fenced resource write that matched no rows into
// ErrStaleToken. Resource tables keep a lock_token bigint column and write
// with "... AND lock_token <= $token" plus "SET lock_token = $token"; "<="
// lets the same holder write repeatedly.
func CheckFenced(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStaleToken
	}
	return nil
}
