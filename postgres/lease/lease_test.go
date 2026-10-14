package lease

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/lease/repository"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	_ "github.com/lib/pq"
)

func TestConfig(t *testing.T) {
	db, err := sql.Open("postgres", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := Config{DB: dbtx.New(db), Namespace: "test"}.WithDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil DB: %v", err)
	}
	cfg.LeaseDuration = -1
	if _, err := New(cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative TTL: %v", err)
	}
	cfg = Config{DB: dbtx.New(db), Namespace: "test"}.WithDefaults()
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Namespace = "mutated"
	if l.cfg.Namespace != "test" {
		t.Fatal("configuration not owned")
	}
	for _, call := range []func(context.Context) (Lease, error){
		func(ctx context.Context) (Lease, error) { return l.Acquire(ctx, "") },
		func(ctx context.Context) (Lease, error) { return l.TryAcquire(ctx, "x", WithLeaseDuration(0)) },
		func(ctx context.Context) (Lease, error) { return l.Acquire(ctx, "x", nil) },
	} {
		if _, err := call(context.Background()); !errors.Is(err, ErrInvalidOption) {
			t.Fatalf("options: %v", err)
		}
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("shutdown")
	cancel(cause)
	if _, err := l.Acquire(ctx, "x"); !errors.Is(err, cause) {
		t.Fatalf("cause: %v", err)
	}
}

func TestLocalExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLease(nil, "key", "owner", 7, time.Second, time.Now().Add(time.Second))
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case <-h.Done():
		default:
			t.Fatal("deadline did not close Done")
		}
		if !errors.Is(h.Err(), ErrLeaseLost) {
			t.Fatal(h.Err())
		}
		<-h.monitorDone
		// A late response/update cannot change the terminal state.
		h.mu.Lock()
		h.deadline = time.Now().Add(time.Hour)
		h.mu.Unlock()
		if !errors.Is(h.Err(), ErrLeaseLost) {
			t.Fatal("lease revived")
		}
	})
}

func TestLocalExpiryWhileProtected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLease(nil, "key", "owner", 7, time.Second, time.Now().Add(time.Second))
		h.mu.Lock()
		h.protectedUntil = time.Now().Add(2 * time.Second)
		h.notify()
		h.mu.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		if h.Err() != nil {
			t.Fatal("admitted transaction expired before its own budget")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !errors.Is(h.Err(), ErrLeaseLost) {
			t.Fatal("transaction budget did not terminate ownership")
		}
		<-h.monitorDone
	})
}

func TestIntegration(t *testing.T) {
	client := dbtest.New(t, dbtest.Options{Image: "postgres:17.4", Duration: 5 * time.Minute})
	db := client.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := db.ExecContext(ctx, repository.Schema); err != nil {
		t.Fatal(err)
	}
	// Schema is reusable for clean installations; it never clears generation rows.
	if _, err := db.ExecContext(ctx, repository.Schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE lease_test_results (key text PRIMARY KEY, value text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	unit := dbtx.New(db)
	var ns atomic.Int64
	newLocker := func(t *testing.T, ttl time.Duration) *Locker {
		t.Helper()
		l, err := New(Config{DB: unit, Namespace: fmt.Sprint("test-", ns.Add(1)), LeaseDuration: ttl})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	release := func(t *testing.T, h Lease) {
		t.Helper()
		if err := h.Release(ctx); err != nil {
			t.Error(err)
		}
	}
	expire := func(t *testing.T, l *Locker, key string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `UPDATE dbtx.leases SET expires_at = clock_timestamp() - interval '1 second' WHERE namespace=$1 AND key=$2`, l.cfg.Namespace, key); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("contention and increasing generations", func(t *testing.T) {
		l := newLocker(t, 0)
		a, err := l.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		defer release(t, a)
		if a.Token() != 1 {
			t.Fatal(a.Token())
		}
		if h, err := l.TryAcquire(ctx, "resource"); h != nil || !errors.Is(err, ErrContended) {
			t.Fatalf("%v %v", h, err)
		}
		// A separate node uses its own dbtx wrapper and the same authority/namespace.
		other, err := New(Config{DB: dbtx.New(db), Namespace: l.cfg.Namespace})
		if err != nil {
			t.Fatal(err)
		}
		b, err := other.TryAcquire(ctx, "different")
		if err != nil {
			t.Fatal(err)
		}
		release(t, b)
		release(t, a)
		if !errors.Is(a.Err(), ErrReleased) {
			t.Fatal(a.Err())
		}
		c, err := other.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		defer release(t, c)
		if c.Token() != 2 {
			t.Fatal(c.Token())
		}
		if err := a.Renew(ctx); !errors.Is(err, ErrReleased) {
			t.Fatalf("released renewal: %v", err)
		}
	})

	t.Run("wait and cancel", func(t *testing.T) {
		l := newLocker(t, 0)
		a, err := l.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		defer release(t, a)
		waitingCtx, stop := context.WithCancelCause(ctx)
		entered := make(chan struct{})
		result := make(chan error, 1)
		go func() { close(entered); _, err := l.Acquire(waitingCtx, "resource"); result <- err }()
		<-entered
		cause := errors.New("stop waiting")
		stop(cause)
		if err := <-result; !errors.Is(err, cause) {
			t.Fatal(err)
		}
		release(t, a)
		b, err := l.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		release(t, b)
	})

	t.Run("try callbacks never run on contention", func(t *testing.T) {
		l := newLocker(t, 0)
		a, err := l.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		defer release(t, a)
		called := false
		fn := func(context.Context, Lease) error { called = true; return nil }
		if err := l.TryActivity(ctx, "resource", fn); !errors.Is(err, ErrContended) {
			t.Fatal(err)
		}
		if err := l.TryRunInFencedTx(ctx, "resource", fn); !errors.Is(err, ErrContended) {
			t.Fatal(err)
		}
		if called {
			t.Fatal("callback called")
		}
	})

	t.Run("expired without successor rejects finalization and renewal", func(t *testing.T) {
		l := newLocker(t, 0)
		a, err := l.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		defer release(t, a)
		expire(t, l, "resource")
		called := false
		err = a.RunInFencedTx(ctx, func(context.Context) error { called = true; return nil })
		if !errors.Is(err, ErrLeaseLost) || called {
			t.Fatalf("%v called=%v", err, called)
		}
		if err := a.Renew(ctx); !errors.Is(err, ErrLeaseLost) {
			t.Fatal(err)
		}
	})

	t.Run("stale worker cannot publish or release successor", func(t *testing.T) {
		l := newLocker(t, 0)
		a, err := l.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		defer release(t, a)
		expire(t, l, "resource")
		b, err := l.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		defer release(t, b)
		if b.Token() <= a.Token() {
			t.Fatal("generation did not advance")
		}
		if err := b.RunInFencedTx(ctx, func(txCtx context.Context) error {
			_, err := unit.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO lease_test_results VALUES ('stale', 'successor')`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		err = a.RunInFencedTx(ctx, func(txCtx context.Context) error {
			_, err := unit.DBTx(txCtx).ExecContext(txCtx, `UPDATE lease_test_results SET value='stale' WHERE key='stale'`)
			return err
		})
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatal(err)
		}
		release(t, a)
		if err := b.Renew(ctx); err != nil {
			t.Fatal("stale release changed successor", err)
		}
		var value string
		if err := db.QueryRowContext(ctx, `SELECT value FROM lease_test_results WHERE key='stale'`).Scan(&value); err != nil || value != "successor" {
			t.Fatalf("%q %v", value, err)
		}
		release(t, b)
		if err := a.RunInFencedTx(ctx, func(context.Context) error { t.Error("stale callback after successor release"); return nil }); !errors.Is(err, ErrLeaseLost) {
			t.Fatal(err)
		}
	})

	t.Run("commit rollback and reentrancy", func(t *testing.T) {
		l := newLocker(t, 0)
		err := l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, held Lease) error {
			if !unit.IsTx(txCtx) {
				t.Error("no callback transaction")
			}
			if err := held.Renew(txCtx); !errors.Is(err, ErrTxInContext) {
				t.Error(err)
			}
			if err := held.Release(context.Background()); !errors.Is(err, ErrLeaseBusy) {
				t.Error(err)
			}
			_, err := unit.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO lease_test_results VALUES ('commit', 'ok')`)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		failure := errors.New("callback failed")
		err = l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, held Lease) error {
			if _, err := unit.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO lease_test_results VALUES ('rollback', 'bad')`); err != nil {
				return err
			}
			return failure
		})
		var outcome *TxOutcomeError
		if !errors.Is(err, failure) || !errors.As(err, &outcome) || outcome.Outcome != TxRolledBack {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM lease_test_results WHERE key='rollback'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%d %v", count, err)
		}
	})

	t.Run("row lock prevents takeover including expired lease", func(t *testing.T) {
		l := newLocker(t, 0)
		a, err := l.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		defer release(t, a)
		expire(t, l, "resource")
		b, err := l.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		defer release(t, b)
		entered := make(chan struct{})
		unblock := make(chan struct{})
		finished := make(chan error, 1)
		go func() {
			finished <- b.RunInFencedTx(ctx, func(txCtx context.Context) error {
				// Expire inside our own transaction while retaining its row lock.
				if _, err := unit.DBTx(txCtx).ExecContext(txCtx, `UPDATE dbtx.leases SET expires_at=clock_timestamp()-interval '1 second' WHERE namespace=$1 AND key='resource'`, l.cfg.Namespace); err != nil {
					return err
				}
				close(entered)
				<-unblock
				return nil
			})
		}()
		<-entered
		if h, err := l.TryAcquire(ctx, "resource"); h != nil || !errors.Is(err, ErrContended) {
			t.Fatalf("%v %v", h, err)
		}
		close(unblock)
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
		c, err := l.TryAcquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		defer release(t, c)
		if c.Token() <= b.Token() {
			t.Fatal("no takeover")
		}
	})

	t.Run("concurrent first acquisition has one winner", func(t *testing.T) {
		l := newLocker(t, 0)
		const nodes = 12
		start := make(chan struct{})
		var wg sync.WaitGroup
		leases := make([]Lease, nodes)
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
				defer release(t, leases[i])
			} else if !errors.Is(errs[i], ErrContended) {
				t.Error(errs[i])
			}
		}
		if winners != 1 {
			t.Fatalf("winners=%d", winners)
		}
	})

	t.Run("activity has no enclosing transaction and renews", func(t *testing.T) {
		l := newLocker(t, 300*time.Millisecond)
		err := l.Activity(ctx, "resource", func(activityCtx context.Context, held Lease) error {
			if unit.IsTx(activityCtx) {
				t.Error("activity transaction")
			}
			// Holding the callback beyond several TTLs exercises real heartbeats.
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-activityCtx.Done():
				return context.Cause(activityCtx)
			case <-timer.C:
			}
			if h, err := l.TryAcquire(ctx, "resource"); h != nil || !errors.Is(err, ErrContended) {
				return fmt.Errorf("unexpected takeover: %v", err)
			}
			return held.RunInFencedTx(activityCtx, func(txCtx context.Context) error {
				if !unit.IsTx(txCtx) {
					t.Error("finalization has no transaction")
				}
				return nil
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		h, err := l.TryAcquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		release(t, h)
	})

	t.Run("activity observes loss and caller cancellation", func(t *testing.T) {
		l := newLocker(t, 300*time.Millisecond)
		err := l.Activity(ctx, "resource", func(activityCtx context.Context, held Lease) error {
			expire(t, l, "resource")
			select {
			case <-activityCtx.Done():
				return nil
			case <-time.After(time.Second):
				return errors.New("loss not observed")
			}
		})
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatal(err)
		}
		cancelCtx, stop := context.WithCancelCause(ctx)
		cause := errors.New("caller shutdown")
		err = l.Activity(cancelCtx, "resource", func(activityCtx context.Context, held Lease) error {
			stop(cause)
			<-activityCtx.Done()
			return nil
		})
		if !errors.Is(err, cause) {
			t.Fatal(err)
		}
		h, err := l.TryAcquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		release(t, h)
	})

	t.Run("reject enclosing transaction", func(t *testing.T) {
		l := newLocker(t, 0)
		err := unit.RunInTx(ctx, func(txCtx context.Context) error {
			if _, err := l.Acquire(txCtx, "resource"); !errors.Is(err, ErrTxInContext) {
				t.Error(err)
			}
			if err := l.Activity(txCtx, "resource", func(context.Context, Lease) error { t.Error("called"); return nil }); !errors.Is(err, ErrTxInContext) {
				t.Error(err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("panic rollback and release", func(t *testing.T) {
		l := newLocker(t, 0)
		func() {
			defer func() {
				if recover() != "boom" {
					t.Error("panic not propagated")
				}
			}()
			_ = l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, held Lease) error {
				if _, err := unit.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO lease_test_results VALUES ('panic', 'bad')`); err != nil {
					t.Fatal(err)
				}
				panic("boom")
			})
		}()
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM lease_test_results WHERE key='panic'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%d %v", count, err)
		}
		h, err := l.TryAcquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		release(t, h)
		func() {
			defer func() {
				if recover() != "activity boom" {
					t.Error("activity panic not propagated")
				}
			}()
			_ = l.Activity(ctx, "resource", func(context.Context, Lease) error { panic("activity boom") })
		}()
		h, err = l.TryAcquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		release(t, h)
	})

	t.Run("transaction timeout rolls back", func(t *testing.T) {
		l := newLocker(t, 0)
		l.cfg.TransactionTimeout = 100 * time.Millisecond
		err := l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, held Lease) error {
			_, err := unit.DBTx(txCtx).ExecContext(txCtx, `SELECT pg_sleep(1)`)
			return err
		})
		if err == nil {
			t.Fatal("timeout succeeded")
		}
		var outcome *TxOutcomeError
		if !errors.As(err, &outcome) || outcome.Outcome != TxRolledBack {
			t.Fatal(err)
		}
		h, err := l.TryAcquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		release(t, h)
	})

	t.Run("confirmed commit survives cleanup failure", func(t *testing.T) {
		l := newLocker(t, 0)
		failure := errors.New("release unavailable")
		wrapped := dbtx.New(db, func(q dbtx.DBTX) dbtx.DBTX { return &failReleaseDB{DBTX: q, err: failure} })
		l.cfg.DB = wrapped
		var held Lease
		err := l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, h Lease) error {
			held = h
			_, err := wrapped.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO lease_test_results VALUES ('cleanup', 'committed')`)
			return err
		})
		var outcome *TxOutcomeError
		if !errors.Is(err, failure) || !errors.As(err, &outcome) || outcome.Outcome != TxCommitted {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM lease_test_results WHERE key='cleanup'`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%d %v", count, err)
		}
		l.cfg.DB = unit // Restore transport so the owner-qualified cleanup retry succeeds.
		release(t, held)
	})

	t.Run("unconfirmed commit is classified unknown", func(t *testing.T) {
		l := newLocker(t, 0)
		err := l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, held Lease) error {
			var pid int
			if err := unit.DBTx(txCtx).QueryRowContext(txCtx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			if _, err := unit.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO lease_test_results VALUES ('unknown', 'unconfirmed')`); err != nil {
				return err
			}
			// A lost connection before receiving COMMIT's result cannot be inferred
			// as committed or rolled back by the generic client.
			var terminated bool
			return db.QueryRowContext(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated)
		})
		var outcome *TxOutcomeError
		if !errors.As(err, &outcome) || outcome.Outcome != TxUnknown {
			t.Fatal(err)
		}
	})

	t.Run("admitted transaction remains serialized beyond TTL", func(t *testing.T) {
		l := newLocker(t, 300*time.Millisecond)
		err := l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, held Lease) error {
			if _, err := unit.DBTx(txCtx).ExecContext(txCtx, `SELECT pg_sleep(0.36)`); err != nil {
				return err
			}
			if err := held.Err(); err != nil {
				return err
			}
			if h, err := l.TryAcquire(ctx, "resource"); h != nil || !errors.Is(err, ErrContended) {
				return fmt.Errorf("takeover while admitted: %v", err)
			}
			_, err := unit.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO lease_test_results VALUES ('beyond-ttl', 'committed')`)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("activity resumes renewal between checkpoints", func(t *testing.T) {
		l := newLocker(t, 900*time.Millisecond)
		err := l.Activity(ctx, "resource", func(activityCtx context.Context, held Lease) error {
			for range 2 {
				if err := held.RunInFencedTx(activityCtx, func(txCtx context.Context) error {
					_, err := unit.DBTx(txCtx).ExecContext(txCtx, `SELECT pg_sleep(0.4)`)
					return err
				}); err != nil {
					return err
				}
				// Let the activity continue beyond a single original lease budget.
				timer := time.NewTimer(300 * time.Millisecond)
				select {
				case <-activityCtx.Done():
					timer.Stop()
					return context.Cause(activityCtx)
				case <-timer.C:
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("named transaction context and successful try helpers", func(t *testing.T) {
		l := newLocker(t, 0)
		named := dbtx.New(db)
		named.SetID("lease-custom-context")
		l.cfg.DB = named
		err := l.TryRunInFencedTx(ctx, "resource", func(txCtx context.Context, held Lease) error {
			if !named.IsTx(txCtx) || unit.IsTx(txCtx) {
				t.Error("transaction ID was not propagated")
			}
			if err := held.RunInFencedTx(txCtx, func(context.Context) error { t.Error("nested transaction called"); return nil }); !errors.Is(err, ErrTxInContext) {
				t.Error(err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = l.TryActivity(ctx, "resource", func(activityCtx context.Context, held Lease) error {
			if named.IsTx(activityCtx) {
				t.Error("activity has a transaction")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("token exhaustion fails closed", func(t *testing.T) {
		l := newLocker(t, 0)
		h, err := l.Acquire(ctx, "resource")
		if err != nil {
			t.Fatal(err)
		}
		release(t, h)
		if _, err := db.ExecContext(ctx, `UPDATE dbtx.leases SET fencing_token=9223372036854775807 WHERE namespace=$1 AND key='resource'`, l.cfg.Namespace); err != nil {
			t.Fatal(err)
		}
		if h, err := l.TryAcquire(ctx, "resource"); h != nil || !errors.Is(err, ErrTokenExhausted) {
			t.Fatalf("%v %v", h, err)
		}
	})
}

// Fail only owner-qualified release, while leaving acquisition, transaction
// setup, and business writes on the real PostgreSQL connection.
type failReleaseDB struct {
	dbtx.DBTX
	err error
}

func (d *failReleaseDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "UPDATE dbtx.leases SET owner_id = NULL") {
		return nil, d.err
	}
	return d.DBTX.ExecContext(ctx, query, args...)
}
