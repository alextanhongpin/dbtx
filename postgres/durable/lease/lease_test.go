package lease_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/lease"
)

func newLocker(t *testing.T, cfg lease.Config) (*lease.Locker, *dbtx.DB) {
	t.Helper()
	if cfg.DB == nil {
		cfg.DB = dbtx.New(durabletest.NewDB(t))
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "test"
	}
	l, err := lease.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l, cfg.DB
}

func TestTryAcquireContentionAndIncreasingTokens(t *testing.T) {
	ctx := t.Context()
	l, _ := newLocker(t, lease.Config{})
	a, err := l.TryAcquire(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.TryAcquire(ctx, "k"); !errors.Is(err, lease.ErrContended) {
		t.Fatalf("want ErrContended, got %v", err)
	}
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := l.TryAcquire(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release(ctx)
	if b.Token() <= a.Token() {
		t.Fatalf("token did not increase: %d then %d", a.Token(), b.Token())
	}
	// Releasing a stale lease again is harmless and never clears b.
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.TryAcquire(ctx, "k"); !errors.Is(err, lease.ErrContended) {
		t.Fatalf("stale release cleared successor: %v", err)
	}
}

func TestAcquireWaitsForRelease(t *testing.T) {
	ctx := t.Context()
	l, _ := newLocker(t, lease.Config{})
	a, err := l.Acquire(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(100 * time.Millisecond); _ = a.Release(ctx) }()
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := l.Acquire(waitCtx, "k")
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Release(ctx)
}

// FM-1: a paused owner resumes after takeover and must not write.
func TestStalePausedOwnerCannotEnterFencedTx(t *testing.T) {
	ctx := t.Context()
	l, db := newLocker(t, lease.Config{LeaseDuration: 150 * time.Millisecond})
	mustExec(t, db, `CREATE TABLE account (id int PRIMARY KEY, owner text, lock_token bigint NOT NULL DEFAULT 0)`)
	mustExec(t, db, `INSERT INTO account (id) VALUES (1)`)

	a, err := l.TryAcquire(ctx, "account:1")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond) // "paused" past TTL, no heartbeat
	b, err := l.TryAcquire(ctx, "account:1")
	if err != nil {
		t.Fatalf("successor could not take over: %v", err)
	}
	defer b.Release(ctx)

	err = a.RunInFencedTx(ctx, func(txCtx context.Context) error {
		_, err := db.DBTx(txCtx).ExecContext(txCtx, `UPDATE account SET owner = 'a' WHERE id = 1`)
		return err
	})
	var outcome *durable.TxOutcomeError
	if !errors.As(err, &outcome) || outcome.Outcome != durable.TxRolledBack || !errors.Is(err, lease.ErrLeaseLost) {
		t.Fatalf("want rolled back ErrLeaseLost, got %v", err)
	}
	if err := b.RunInFencedTx(ctx, func(txCtx context.Context) error {
		return lease.CheckFenced(db.DBTx(txCtx).ExecContext(txCtx,
			`UPDATE account SET owner = 'b', lock_token = $1 WHERE id = 1 AND lock_token <= $1`, b.Token()))
	}); err != nil {
		t.Fatal(err)
	}
	// A late write carrying a's older token is rejected by the resource.
	err = lease.CheckFenced(db.DB().ExecContext(ctx,
		`UPDATE account SET owner = 'a', lock_token = $1 WHERE id = 1 AND lock_token <= $1`, a.Token()))
	if !errors.Is(err, lease.ErrStaleToken) {
		t.Fatalf("want ErrStaleToken, got %v", err)
	}
	var owner string
	if err := db.DB().QueryRowContext(ctx, `SELECT owner FROM account WHERE id = 1`).Scan(&owner); err != nil || owner != "b" {
		t.Fatalf("owner = %q, %v", owner, err)
	}
}

func TestActivityHeartbeatsBeyondTTL(t *testing.T) {
	ctx := t.Context()
	l, _ := newLocker(t, lease.Config{LeaseDuration: 150 * time.Millisecond})
	contended := atomic.Bool{}
	err := l.Activity(ctx, "job", func(ctx context.Context, held lease.Lease) error {
		deadline := time.Now().Add(600 * time.Millisecond)
		for time.Now().Before(deadline) {
			if _, err := l.TryAcquire(t.Context(), "job"); errors.Is(err, lease.ErrContended) {
				contended.Store(true)
			} else {
				return errors.New("successor acquired a heartbeating lease")
			}
			time.Sleep(50 * time.Millisecond)
		}
		return ctx.Err()
	})
	if err != nil || !contended.Load() {
		t.Fatalf("activity: %v", err)
	}
}

// FM-3: heartbeats stop (starved); the watchdog cancels work by the deadline.
func TestActivityCancelledWhenHeartbeatsStall(t *testing.T) {
	ctx := t.Context()
	gate := durabletest.NewGate()
	defer gate.Open()
	l, _ := newLocker(t, lease.Config{
		LeaseDuration: 150 * time.Millisecond,
		Hooks:         &durable.Hooks{BeforeHeartbeat: func(context.Context, string) { gate.Wait() }},
	})
	start := time.Now()
	err := l.Activity(ctx, "job", func(ctx context.Context, _ lease.Lease) error {
		<-ctx.Done()
		if !errors.Is(context.Cause(ctx), lease.ErrLeaseLost) {
			t.Errorf("cause = %v", context.Cause(ctx))
		}
		gate.Open()
		return nil
	})
	if !errors.Is(err, lease.ErrLeaseLost) {
		t.Fatalf("want ErrLeaseLost, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("work was cancelled after %v, beyond the TTL", elapsed)
	}
}

func TestRejectsTransactionContext(t *testing.T) {
	l, db := newLocker(t, lease.Config{})
	err := db.RunInTx(t.Context(), func(txCtx context.Context) error {
		_, err := l.TryAcquire(txCtx, "k")
		return err
	})
	if !errors.Is(err, lease.ErrTxInContext) {
		t.Fatalf("want ErrTxInContext, got %v", err)
	}
}

func TestRunInFencedTxCommitsAndReleases(t *testing.T) {
	ctx := t.Context()
	l, db := newLocker(t, lease.Config{})
	mustExec(t, db, `CREATE TABLE t (v int)`)
	if err := l.RunInFencedTx(ctx, "k", func(txCtx context.Context, _ lease.Lease) error {
		_, err := db.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO t VALUES (1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.RunInFencedTx(ctx, "k", func(txCtx context.Context, _ lease.Lease) error {
		_, _ = db.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO t VALUES (2)`)
		return errors.New("boom")
	}); err == nil {
		t.Fatal("want error")
	}
	var n int
	_ = db.DB().QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n)
	if n != 1 {
		t.Fatalf("rows = %d, want 1 (second insert rolled back)", n)
	}
	held, err := l.TryAcquire(ctx, "k")
	if err != nil {
		t.Fatalf("lease not released: %v", err)
	}
	_ = held.Release(ctx)
}

func TestAdvanceFencing(t *testing.T) {
	ctx := t.Context()
	l, db := newLocker(t, lease.Config{})
	a, _ := l.TryAcquire(ctx, "k")
	_ = a.Release(ctx)
	mustExec(t, db, `SELECT durable.advance_fencing(1000)`)
	b, err := l.TryAcquire(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release(ctx)
	if b.Token() < a.Token()+1000 {
		t.Fatalf("token %d not advanced past %d", b.Token(), a.Token()+1000)
	}
}

func mustExec(t *testing.T, db *dbtx.DB, q string) {
	t.Helper()
	if _, err := db.DB().ExecContext(t.Context(), q); err != nil {
		t.Fatal(err)
	}
}

// Regression: heartbeats run every TTL/3, not back to back.
func TestHeartbeatInterval(t *testing.T) {
	var n atomic.Int64
	l, _ := newLocker(t, lease.Config{LeaseDuration: 900 * time.Millisecond,
		Hooks: &durable.Hooks{BeforeHeartbeat: func(context.Context, string) { n.Add(1) }}})
	_ = l.Activity(t.Context(), "k", func(context.Context, lease.Lease) error {
		time.Sleep(time.Second)
		return nil
	})
	if got := n.Load(); got < 1 || got > 6 {
		t.Fatalf("%d heartbeats in 1s with TTL 900ms, want about 3", got)
	}
}
