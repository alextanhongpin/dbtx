package lease

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/lease/repository/postgres"
)

type heldLease struct {
	locker         *Locker
	key, owner     string
	token          int64
	ttl            time.Duration
	busy           bool
	automatic      bool
	opDone         chan struct{}
	mu             sync.Mutex
	deadline       time.Time
	protectedUntil time.Time
	terminal       error
	released       bool
	done           chan struct{}
	wake           chan struct{}
	renewNow       chan struct{}
	monitorDone    chan struct{}
}

func newLease(locker *Locker, key, owner string, token int64, ttl time.Duration, deadline time.Time) *heldLease {
	h := &heldLease{locker: locker, key: key, owner: owner, token: token, ttl: ttl,
		deadline: deadline, done: make(chan struct{}),
		wake: make(chan struct{}, 1), renewNow: make(chan struct{}, 1), monitorDone: make(chan struct{})}
	go h.monitor()
	return h
}

func (h *heldLease) Token() uint64         { return uint64(h.token) }
func (h *heldLease) Done() <-chan struct{} { return h.done }
func (h *heldLease) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked()
	return h.terminal
}

func (h *heldLease) terminalLocked(err error) {
	if h.terminal == nil {
		h.terminal = err
		close(h.done)
	}
	h.notify()
}
func (h *heldLease) notify() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}
func (h *heldLease) effectiveDeadlineLocked() time.Time {
	if !h.protectedUntil.IsZero() {
		return h.protectedUntil
	}
	return h.deadline
}
func (h *heldLease) expireLocked() {
	if h.terminal == nil && !time.Now().Before(h.effectiveDeadlineLocked()) {
		h.terminalLocked(ErrLeaseLost)
	}
}
func (h *heldLease) lose() { h.mu.Lock(); h.terminalLocked(ErrLeaseLost); h.mu.Unlock() }

// A user operation may wait for the helper's bounded heartbeat request,
// but never waits on another user operation (which could be reentrant).
func (h *heldLease) beginOp(ctx context.Context, automatic bool) error {
	for {
		h.mu.Lock()
		if !h.busy {
			h.busy, h.automatic = true, automatic
			h.opDone = make(chan struct{})
			h.mu.Unlock()
			return nil
		}
		done := h.opDone
		canWait := !automatic && h.automatic
		h.mu.Unlock()
		if !canWait {
			return ErrLeaseBusy
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-done:
		}
	}
}
func (h *heldLease) endOp() {
	h.mu.Lock()
	h.busy, h.automatic = false, false
	close(h.opDone)
	h.mu.Unlock()
}

func (h *heldLease) monitor() {
	defer close(h.monitorDone)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		h.mu.Lock()
		h.expireLocked()
		if h.terminal != nil {
			h.mu.Unlock()
			return
		}
		wait := time.Until(h.effectiveDeadlineLocked())
		h.mu.Unlock()
		timer.Reset(wait)
		select {
		case <-h.done:
			return
		case <-h.wake:
		case <-timer.C:
		}
	}
}

func (h *heldLease) Renew(ctx context.Context) error { return h.renew(ctx, false) }

func (h *heldLease) renew(ctx context.Context, automatic bool) error {
	if err := h.locker.checkContext(ctx); err != nil {
		return err
	}
	if err := h.beginOp(ctx, automatic); err != nil {
		return err
	}
	defer h.endOp()
	h.mu.Lock()
	h.expireLocked()
	if h.terminal != nil {
		err := h.terminal
		h.mu.Unlock()
		return err
	}
	oldDeadline := h.deadline
	h.mu.Unlock()
	sent := time.Now()
	reqCtx, cancel := context.WithDeadline(ctx, minTime(oldDeadline, sent.Add(min(h.locker.cfg.RequestTimeout, h.ttl/3))))
	defer cancel()
	renewed, err := postgres.New(h.locker.cfg.DB.DB()).RenewLease(reqCtx, postgres.RenewLeaseParams{
		Namespace: h.locker.cfg.Namespace, Key: h.key, OwnerID: h.owner,
		Token: h.token, TtlUs: int64(h.ttl / time.Microsecond),
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked()
	if err != nil {
		return errors.Join(err, h.terminal)
	}
	if !renewed {
		h.terminalLocked(ErrLeaseLost)
		return ErrLeaseLost
	}
	if h.terminal != nil {
		return h.terminal
	}
	// A late renewal response must never resurrect local ownership.
	next := sent.Add(h.ttl - h.ttl/6)
	if !time.Now().Before(next) {
		h.terminalLocked(ErrLeaseLost)
		return ErrLeaseLost
	}
	h.deadline = next
	h.notify()
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Release terminates local ownership before the remote request. A failed
// remote release may be retried; a successor is never cleared. Releasing an
// already absent ownership is harmless, including after an unknown response.
func (h *heldLease) Release(ctx context.Context) error {
	if h.locker.cfg.DB.IsTx(ctx) || dbtx.IsTx(ctx) {
		return ErrTxInContext
	}
	if err := h.beginOp(ctx, false); err != nil {
		return err
	}
	defer h.endOp()
	h.mu.Lock()
	h.expireLocked()
	h.terminalLocked(ErrReleased)
	released := h.released
	h.mu.Unlock()
	<-h.monitorDone
	if released {
		return nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, h.locker.cfg.CleanupTimeout)
	defer cancel()
	_, err := postgres.New(h.locker.cfg.DB.DB()).ReleaseLease(reqCtx, postgres.ReleaseLeaseParams{
		Namespace: h.locker.cfg.Namespace, Key: h.key,
		OwnerID: sql.NullString{String: h.owner, Valid: true}, Token: h.token,
	})
	if err == nil {
		h.mu.Lock()
		h.released = true
		h.mu.Unlock()
	}
	return err
}

// TxOutcome identifies the effect of one fenced transaction, not an activity.
type TxOutcome string

const (
	TxCommitted  TxOutcome = "committed"
	TxRolledBack TxOutcome = "rolled_back"
	TxUnknown    TxOutcome = "unknown"
)

// TxOutcomeError preserves a confirmed commit plus cleanup failure, a callback
// or admission failure before commit, or an unconfirmed COMMIT response.
// Inspect through errors.As; do not replay work solely because err != nil.
type TxOutcomeError struct {
	Outcome TxOutcome
	Err     error
}

func (e *TxOutcomeError) Error() string {
	return fmt.Sprintf("lease: transaction %s: %v", e.Outcome, e.Err)
}
func (e *TxOutcomeError) Unwrap() error { return e.Err }

func (h *heldLease) RunInFencedTx(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil {
		return ErrInvalidOption
	}
	if err := h.locker.checkContext(ctx); err != nil {
		return err
	}
	if err := h.beginOp(ctx, false); err != nil {
		return err
	}
	defer func() {
		h.endOp()
		// Resume an activity heartbeat immediately, rather than waiting for a
		// ticker that could fall after the remaining lease budget. Raw leases
		// have no receiver and still require explicit manual renewal.
		select {
		case h.renewNow <- struct{}{}:
		default:
		}
	}()
	if err := h.Err(); err != nil {
		return &TxOutcomeError{TxRolledBack, err}
	}
	txCtx, cancel := context.WithTimeout(ctx, h.locker.cfg.TransactionTimeout)
	defer cancel()
	// Always own a fresh READ COMMITTED transaction; the callback must commit here.
	txCtx = dbtx.WithNamedTxOptions(txCtx, h.locker.cfg.DB.ID(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	callbackCompleted := false
	defer func() {
		h.mu.Lock()
		h.protectedUntil = time.Time{}
		h.expireLocked()
		h.notify()
		h.mu.Unlock()
	}()
	err := h.locker.cfg.DB.RunInTx(txCtx, func(txCtx context.Context) error {
		q := postgres.New(h.locker.cfg.DB.DBTx(txCtx))
		timeout := fmt.Sprintf("%dms", max(int64(1), h.locker.cfg.TransactionTimeout.Milliseconds()))
		if err := q.SetTransactionTimeouts(txCtx, timeout); err != nil {
			return err
		}
		_, err := q.LockLeaseRow(txCtx, postgres.LockLeaseRowParams{Namespace: h.locker.cfg.Namespace, Key: h.key})
		if errors.Is(err, sql.ErrNoRows) {
			h.lose()
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		valid, err := q.ValidateLease(txCtx, postgres.ValidateLeaseParams{
			Namespace: h.locker.cfg.Namespace, Key: h.key,
			OwnerID: sql.NullString{String: h.owner, Valid: true}, Token: h.token,
		})
		if err != nil {
			return err
		}
		if !valid {
			h.lose()
			return ErrLeaseLost
		}
		h.mu.Lock()
		h.expireLocked()
		if h.terminal != nil {
			err := h.terminal
			h.mu.Unlock()
			return err
		}
		// An admitted transaction owns the row until commit/rollback. Expiry can
		// no longer admit a successor; the transaction deadline bounds this phase.
		h.protectedUntil, _ = txCtx.Deadline()
		h.notify()
		h.mu.Unlock()
		if err := fn(txCtx); err != nil {
			return err
		}
		if err := context.Cause(txCtx); err != nil {
			return err
		}
		callbackCompleted = true
		return nil
	})
	if err != nil {
		outcome := TxRolledBack
		if callbackCompleted {
			outcome = TxUnknown
		}
		return &TxOutcomeError{outcome, err}
	}
	return nil
}
