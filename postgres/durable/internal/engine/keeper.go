// Package engine holds the lease runtime shared by every durable feature.
package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/durable"
)

// RenewFunc extends a lease in the database. ok=false means the database
// authoritatively rejected the owner/token (lost); err means the outcome is
// unknown and the existing deadline stands.
type RenewFunc func(ctx context.Context) (ok bool, err error)

// Keeper tracks one lease: a conservative local deadline (request start +
// TTL - TTL/6), automatic heartbeats every TTL/3 once Start is called, and a
// single terminal cause. A late renewal can never revive a terminal lease.
type Keeper struct {
	ttl            time.Duration
	requestTimeout time.Duration
	renew          RenewFunc
	beforeRenew    func(context.Context)

	mu             sync.Mutex
	deadline       time.Time
	protectedUntil time.Time
	terminal       error
	busy           bool
	automatic      bool
	opDone         chan struct{}
	done           chan struct{}
	wake           chan struct{}
	renewNow       chan struct{}
	monitorDone    chan struct{}
	workCancel     context.CancelCauseFunc
}

// NewKeeper starts the expiry monitor. sent is when the acquiring request was
// sent; requestTimeout bounds each renewal (capped at TTL/3).
func NewKeeper(ttl, requestTimeout time.Duration, sent time.Time, renew RenewFunc, beforeRenew func(context.Context)) *Keeper {
	k := &Keeper{
		ttl: ttl, requestTimeout: min(requestTimeout, ttl/3), renew: renew, beforeRenew: beforeRenew,
		deadline: sent.Add(ttl - ttl/6),
		done:     make(chan struct{}), wake: make(chan struct{}, 1),
		renewNow: make(chan struct{}, 1), monitorDone: make(chan struct{}),
	}
	go k.monitor()
	return k
}

// SafeDeadline is the latest moment local ownership may be assumed.
func SafeDeadline(sent time.Time, ttl time.Duration) time.Time { return sent.Add(ttl - ttl/6) }

func (k *Keeper) Done() <-chan struct{} { return k.done }

// TTL is the lease length this keeper renews with.
func (k *Keeper) TTL() time.Duration { return k.ttl }

func (k *Keeper) Err() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.expireLocked()
	return k.terminal
}

// Lose marks the lease terminal with cause (ErrLeaseLost when nil).
func (k *Keeper) Lose(cause error) {
	if cause == nil {
		cause = durable.ErrLeaseLost
	}
	k.mu.Lock()
	k.terminalLocked(cause)
	k.mu.Unlock()
}

// Close marks the lease terminal (ErrReleased) and waits for the monitor.
func (k *Keeper) Close() {
	k.Lose(durable.ErrReleased)
	<-k.monitorDone
}

func (k *Keeper) terminalLocked(err error) {
	if k.terminal == nil {
		k.terminal = err
		close(k.done)
	}
	k.notify()
}

func (k *Keeper) notify() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

func (k *Keeper) effectiveDeadlineLocked() time.Time {
	if !k.protectedUntil.IsZero() {
		return k.protectedUntil
	}
	return k.deadline
}

func (k *Keeper) expireLocked() {
	if k.terminal == nil && !time.Now().Before(k.effectiveDeadlineLocked()) {
		k.terminalLocked(durable.ErrLeaseLost)
	}
}

func (k *Keeper) monitor() {
	defer close(k.monitorDone)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		k.mu.Lock()
		k.expireLocked()
		if k.terminal != nil {
			k.mu.Unlock()
			return
		}
		wait := time.Until(k.effectiveDeadlineLocked())
		k.mu.Unlock()
		timer.Reset(wait)
		select {
		case <-k.done:
			return
		case <-k.wake:
		case <-timer.C:
		}
	}
}

// BeginOp serializes lifecycle operations. A user operation may wait for an
// automatic heartbeat; it never waits for another user operation (which could
// be reentrant), returning ErrLeaseBusy instead.
func (k *Keeper) BeginOp(ctx context.Context, automatic bool) error {
	for {
		k.mu.Lock()
		if !k.busy {
			k.busy, k.automatic = true, automatic
			k.opDone = make(chan struct{})
			k.mu.Unlock()
			return nil
		}
		done := k.opDone
		canWait := !automatic && k.automatic
		k.mu.Unlock()
		if !canWait {
			return durable.ErrLeaseBusy
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-done:
		}
	}
}

// EndOp ends the operation started by BeginOp. After a user operation (which
// may have suppressed heartbeats) it asks for an immediate renewal; after an
// automatic heartbeat it does not, or heartbeats would run back to back.
func (k *Keeper) EndOp() {
	k.mu.Lock()
	wasUser := !k.automatic
	k.busy, k.automatic = false, false
	close(k.opDone)
	k.mu.Unlock()
	if wasUser {
		select {
		case k.renewNow <- struct{}{}:
		default:
		}
	}
}

// Renew extends the lease once. Call between BeginOp and EndOp.
func (k *Keeper) RenewLocked(ctx context.Context) error {
	k.mu.Lock()
	k.expireLocked()
	if k.terminal != nil {
		err := k.terminal
		k.mu.Unlock()
		return err
	}
	old := k.deadline
	k.mu.Unlock()
	sent := time.Now()
	reqCtx, cancel := context.WithDeadline(ctx, minTime(old, sent.Add(k.requestTimeout)))
	defer cancel()
	ok, err := k.renew(reqCtx)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.expireLocked()
	if err != nil {
		return errors.Join(err, k.terminal)
	}
	if !ok {
		k.terminalLocked(durable.ErrLeaseLost)
		return durable.ErrLeaseLost
	}
	if k.terminal != nil {
		return k.terminal
	}
	next := SafeDeadline(sent, k.ttl)
	if !time.Now().Before(next) {
		k.terminalLocked(durable.ErrLeaseLost)
		return durable.ErrLeaseLost
	}
	k.deadline = next
	k.notify()
	return nil
}

// Renew is BeginOp + RenewLocked + EndOp.
func (k *Keeper) Renew(ctx context.Context, automatic bool) error {
	if err := k.BeginOp(ctx, automatic); err != nil {
		return err
	}
	defer k.EndOp()
	return k.RenewLocked(ctx)
}

// Protect keeps the lease locally valid until t: used while a transaction
// holds the lease row, which blocks any successor until commit.
func (k *Keeper) Protect(t time.Time) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.expireLocked()
	if k.terminal != nil {
		return k.terminal
	}
	k.protectedUntil = t
	k.notify()
	return nil
}

// Unprotect ends Protect and applies the normal deadline again.
func (k *Keeper) Unprotect() {
	k.mu.Lock()
	k.protectedUntil = time.Time{}
	k.expireLocked()
	k.notify()
	k.mu.Unlock()
}

// Run heartbeats every TTL/3 until ctx ends or the lease is terminal, then
// calls onTerminal with the cause. Transport failures leave the deadline as is.
func (k *Keeper) Run(ctx context.Context, key string, onTerminal func(error)) {
	ticker := time.NewTicker(max(k.ttl/3, time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.done:
			onTerminal(k.Err())
			return
		case <-ticker.C:
		case <-k.renewNow:
		}
		if k.beforeRenew != nil {
			k.beforeRenew(ctx)
		}
		err := k.Renew(ctx, true)
		if terminal := k.Err(); terminal != nil {
			onTerminal(terminal)
			return
		}
		if err != nil && ctx.Err() != nil {
			return
		}
	}
}

// Supervise runs fn with a context cancelled (with cause) when the lease
// becomes terminal, heartbeating meanwhile. It waits for fn to return even if
// fn ignores cancellation, and joins its goroutines before returning.
func (k *Keeper) Supervise(ctx context.Context, key string, fn func(context.Context) error) (fnErr, cause error) {
	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	k.mu.Lock()
	k.workCancel = cancel
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		k.workCancel = nil
		k.mu.Unlock()
	}()
	hbCtx, stopHB := context.WithCancel(workCtx)
	watchCtx, stopWatch := context.WithCancel(ctx)
	watchDone, hbDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-watchCtx.Done():
		case <-k.done:
			cancel(k.Err())
		}
	}()
	go func() { defer close(hbDone); k.Run(hbCtx, key, func(err error) { cancel(err) }) }()
	fnErr = fn(workCtx)
	stopHB()
	stopWatch()
	<-watchDone
	<-hbDone
	return fnErr, context.Cause(workCtx)
}

// CancelWork cancels the context of a running Supervise call with cause,
// without ending the lease (used for cooperative job cancellation).
func (k *Keeper) CancelWork(cause error) {
	k.mu.Lock()
	cancel := k.workCancel
	k.mu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
