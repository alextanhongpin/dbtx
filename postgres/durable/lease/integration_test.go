package lease_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/lease"
)

func TestConfigValidation(t *testing.T) {
	db, err := sql.Open("postgres", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base := lease.Config{DB: dbtx.New(db), Namespace: "test"}
	if _, err := lease.New(base); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*lease.Config){
		"nil DB":         func(c *lease.Config) { c.DB = nil },
		"no namespace":   func(c *lease.Config) { c.Namespace = "" },
		"negative TTL":   func(c *lease.Config) { c.LeaseDuration = -1 },
		"tiny TTL":       func(c *lease.Config) { c.LeaseDuration = time.Millisecond },
		"tx timeout max": func(c *lease.Config) { c.TransactionTimeout = 2147483648 * time.Millisecond },
	} {
		cfg := base
		mut(&cfg)
		if _, err := lease.New(cfg); !errors.Is(err, durable.ErrInvalidConfig) {
			t.Errorf("%s: want ErrInvalidConfig, got %v", name, err)
		}
	}
	cfg := base
	cfg.TransactionTimeout = 2147483647 * time.Millisecond
	if _, err := lease.New(cfg); err != nil {
		t.Fatalf("maximum tx timeout rejected: %v", err)
	}
	l, _ := lease.New(base)
	for _, call := range []func(context.Context) (lease.Lease, error){
		func(ctx context.Context) (lease.Lease, error) { return l.Acquire(ctx, "") },
		func(ctx context.Context) (lease.Lease, error) {
			return l.TryAcquire(ctx, "x", lease.WithLeaseDuration(0))
		},
		func(ctx context.Context) (lease.Lease, error) { return l.Acquire(ctx, "x", nil) },
	} {
		if _, err := call(context.Background()); !errors.Is(err, durable.ErrInvalidArgument) {
			t.Errorf("options: %v", err)
		}
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("shutdown")
	cancel(cause)
	if _, err := l.Acquire(ctx, "x"); !errors.Is(err, cause) {
		t.Fatalf("cause: %v", err)
	}
}

func expireLease(t *testing.T, db *dbtx.DB, key string) {
	t.Helper()
	mustExec(t, db, fmt.Sprintf(`UPDATE durable.leases SET expires_at = clock_timestamp() - interval '1 second' WHERE namespace = 'test' AND key = '%s'`, key))
}

func count(t *testing.T, db *dbtx.DB, q string) int {
	t.Helper()
	var n int
	if err := db.DB().QueryRowContext(t.Context(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustReacquire(t *testing.T, l *lease.Locker, key string) {
	t.Helper()
	h, err := l.TryAcquire(t.Context(), key)
	if err != nil {
		t.Fatalf("lease not released: %v", err)
	}
	if err := h.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestTryCallbacksNeverRunOnContention(t *testing.T) {
	ctx := t.Context()
	l, _ := newLocker(t, lease.Config{})
	a, err := l.Acquire(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release(ctx)
	called := false
	fn := func(context.Context, lease.Lease) error { called = true; return nil }
	if err := l.TryActivity(ctx, "r", fn); !errors.Is(err, lease.ErrContended) {
		t.Fatal(err)
	}
	if err := l.TryRunInFencedTx(ctx, "r", fn); !errors.Is(err, lease.ErrContended) {
		t.Fatal(err)
	}
	if called {
		t.Fatal("callback called")
	}
}

func TestExpiredWithoutSuccessorRejectsFencedTxAndRenew(t *testing.T) {
	ctx := t.Context()
	l, db := newLocker(t, lease.Config{})
	a, err := l.Acquire(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release(ctx)
	expireLease(t, db, "r")
	called := false
	err = a.RunInFencedTx(ctx, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, lease.ErrLeaseLost) || called {
		t.Fatalf("%v called=%v", err, called)
	}
	if err := a.Renew(ctx); !errors.Is(err, lease.ErrLeaseLost) {
		t.Fatal(err)
	}
}

func TestFencedTxReentrancy(t *testing.T) {
	l, _ := newLocker(t, lease.Config{})
	err := l.RunInFencedTx(t.Context(), "r", func(txCtx context.Context, held lease.Lease) error {
		if err := held.Renew(txCtx); !errors.Is(err, lease.ErrTxInContext) {
			t.Error(err)
		}
		if err := held.Release(context.Background()); !errors.Is(err, lease.ErrLeaseBusy) {
			t.Error(err)
		}
		if err := held.RunInFencedTx(txCtx, func(context.Context) error { t.Error("nested called"); return nil }); !errors.Is(err, lease.ErrTxInContext) {
			t.Error(err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mustReacquire(t, l, "r")
}

// The row lock held by an admitted transaction blocks takeover even after
// the lease expires inside it.
func TestRowLockPreventsTakeover(t *testing.T) {
	ctx := t.Context()
	l, db := newLocker(t, lease.Config{})
	b, err := l.Acquire(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release(ctx)
	entered, unblock := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- b.RunInFencedTx(ctx, func(txCtx context.Context) error {
			if _, err := db.DBTx(txCtx).ExecContext(txCtx, `UPDATE durable.leases SET expires_at = clock_timestamp() - interval '1 second' WHERE namespace = 'test' AND key = 'r'`); err != nil {
				return err
			}
			close(entered)
			<-unblock
			return nil
		})
	}()
	<-entered
	if h, err := l.TryAcquire(ctx, "r"); h != nil || !errors.Is(err, lease.ErrContended) {
		t.Fatalf("takeover while admitted: %v %v", h, err)
	}
	close(unblock)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	c, err := l.TryAcquire(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release(ctx)
	if c.Token() <= b.Token() {
		t.Fatal("no takeover")
	}
}

func TestConcurrentFirstAcquisitionHasOneWinner(t *testing.T) {
	ctx := t.Context()
	l, _ := newLocker(t, lease.Config{})
	const nodes = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	leases := make([]lease.Lease, nodes)
	errs := make([]error, nodes)
	for i := range nodes {
		wg.Go(func() { <-start; leases[i], errs[i] = l.TryAcquire(ctx, "new") })
	}
	close(start)
	wg.Wait()
	winners := 0
	for i := range nodes {
		if errs[i] == nil {
			winners++
			defer leases[i].Release(ctx)
		} else if !errors.Is(errs[i], lease.ErrContended) {
			t.Error(errs[i])
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d", winners)
	}
}

func TestActivityObservesLossAndCallerCancellation(t *testing.T) {
	ctx := t.Context()
	l, db := newLocker(t, lease.Config{LeaseDuration: 300 * time.Millisecond})
	err := l.Activity(ctx, "r", func(actCtx context.Context, _ lease.Lease) error {
		expireLease(t, db, "r")
		select {
		case <-actCtx.Done():
			return nil
		case <-time.After(time.Second):
			return errors.New("loss not observed")
		}
	})
	if !errors.Is(err, lease.ErrLeaseLost) {
		t.Fatal(err)
	}
	cancelCtx, stop := context.WithCancelCause(ctx)
	cause := errors.New("caller shutdown")
	err = l.Activity(cancelCtx, "r", func(actCtx context.Context, _ lease.Lease) error {
		stop(cause)
		<-actCtx.Done()
		return nil
	})
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	mustReacquire(t, l, "r")
}

func TestPanicRollsBackAndReleases(t *testing.T) {
	l, db := newLocker(t, lease.Config{})
	mustExec(t, db, `CREATE TABLE results (key text PRIMARY KEY)`)
	func() {
		defer func() {
			if recover() != "boom" {
				t.Error("panic not propagated")
			}
		}()
		_ = l.RunInFencedTx(t.Context(), "r", func(txCtx context.Context, _ lease.Lease) error {
			if _, err := db.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO results VALUES ('panic')`); err != nil {
				t.Error(err)
			}
			panic("boom")
		})
	}()
	if n := count(t, db, `SELECT count(*) FROM results`); n != 0 {
		t.Fatalf("rows=%d after panic", n)
	}
	mustReacquire(t, l, "r")
	func() {
		defer func() {
			if recover() != "activity boom" {
				t.Error("activity panic not propagated")
			}
		}()
		_ = l.Activity(t.Context(), "r", func(context.Context, lease.Lease) error { panic("activity boom") })
	}()
	mustReacquire(t, l, "r")
}

func TestTransactionTimeoutRollsBack(t *testing.T) {
	l, db := newLocker(t, lease.Config{TransactionTimeout: 100 * time.Millisecond})
	start := time.Now()
	err := l.RunInFencedTx(t.Context(), "r", func(txCtx context.Context, _ lease.Lease) error {
		_, err := db.DBTx(txCtx).ExecContext(txCtx, `SELECT pg_sleep(2)`)
		return err
	})
	var outcome *durable.TxOutcomeError
	if !errors.As(err, &outcome) || outcome.Outcome != durable.TxRolledBack {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
	mustReacquire(t, l, "r")
}

func TestConfirmedCommitSurvivesCleanupFailure(t *testing.T) {
	_, plain := newLocker(t, lease.Config{})
	mustExec(t, plain, `CREATE TABLE results (key text PRIMARY KEY)`)
	failure := errors.New("release unavailable")
	var fail sync.Map
	wrapped := dbtx.New(plain.Unwrap(), func(q dbtx.DBTX) dbtx.DBTX { return &failReleaseDB{DBTX: q, err: failure, on: &fail} })
	l, _ := newLocker(t, lease.Config{DB: wrapped})
	fail.Store("on", true)
	var held lease.Lease
	err := l.RunInFencedTx(t.Context(), "r", func(txCtx context.Context, h lease.Lease) error {
		held = h
		_, err := wrapped.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO results VALUES ('cleanup')`)
		return err
	})
	var outcome *durable.TxOutcomeError
	if !errors.Is(err, failure) || !errors.As(err, &outcome) || outcome.Outcome != durable.TxCommitted {
		t.Fatal(err)
	}
	if n := count(t, plain, `SELECT count(*) FROM results`); n != 1 {
		t.Fatalf("rows=%d", n)
	}
	fail.Delete("on")
	if err := held.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	mustReacquire(t, l, "r")
}

func TestUnconfirmedCommitIsUnknown(t *testing.T) {
	l, db := newLocker(t, lease.Config{})
	mustExec(t, db, `CREATE TABLE results (key text PRIMARY KEY)`)
	err := l.RunInFencedTx(t.Context(), "r", func(txCtx context.Context, _ lease.Lease) error {
		var pid int
		if err := db.DBTx(txCtx).QueryRowContext(txCtx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		if _, err := db.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO results VALUES ('unknown')`); err != nil {
			return err
		}
		// The connection dies before COMMIT's reply, so the outcome is unknown.
		var ok bool
		return db.DB().QueryRowContext(t.Context(), `SELECT pg_terminate_backend($1)`, pid).Scan(&ok)
	})
	var outcome *durable.TxOutcomeError
	if !errors.As(err, &outcome) || outcome.Outcome != durable.TxUnknown {
		t.Fatal(err)
	}
}

func TestAdmittedTxSerializedBeyondTTL(t *testing.T) {
	l, db := newLocker(t, lease.Config{LeaseDuration: 300 * time.Millisecond})
	err := l.RunInFencedTx(t.Context(), "r", func(txCtx context.Context, held lease.Lease) error {
		if _, err := db.DBTx(txCtx).ExecContext(txCtx, `SELECT pg_sleep(0.36)`); err != nil {
			return err
		}
		if err := held.Err(); err != nil {
			return err
		}
		if h, err := l.TryAcquire(t.Context(), "r"); h != nil || !errors.Is(err, lease.ErrContended) {
			return fmt.Errorf("takeover while admitted: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTokenExhaustionFailsClosed(t *testing.T) {
	l, db := newLocker(t, lease.Config{})
	mustReacquire(t, l, "r")
	mustExec(t, db, `UPDATE durable.leases SET token = 9223372036854775807 WHERE namespace = 'test' AND key = 'r'`)
	if h, err := l.TryAcquire(t.Context(), "r"); h != nil || !errors.Is(err, durable.ErrTokenExhausted) {
		t.Fatalf("%v %v", h, err)
	}
}

// failReleaseDB fails only the owner-qualified release while enabled.
type failReleaseDB struct {
	dbtx.DBTX
	err error
	on  *sync.Map
}

func (d *failReleaseDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if _, ok := d.on.Load("on"); ok && strings.Contains(query, "-- name: ReleaseResourceLease ") {
		return nil, d.err
	}
	return d.DBTX.ExecContext(ctx, query, args...)
}
