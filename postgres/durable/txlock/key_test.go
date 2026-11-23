package txlock_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/txlock"
)

func newLocker(t *testing.T) (*txlock.Locker, *dbtx.DB) {
	t.Helper()
	db := dbtx.New(durabletest.NewDB(t))
	return txlock.New(db), db
}

func noop(context.Context) error { return nil }

// A lock_timeout inside a joined caller transaction must not abort it.
func TestWaitTimeoutLeavesCallerTxUsable(t *testing.T) {
	ctx := t.Context()
	l, db := newLocker(t)
	if _, err := db.DB().ExecContext(ctx, `CREATE TABLE t (v int)`); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- l.WithLock(ctx, "k", txlock.Options{}, func(context.Context) error { close(held); <-release; return nil })
	}()
	<-held
	defer func() { close(release); <-done }()
	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		if _, err := db.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO t VALUES (1)`); err != nil {
			return err
		}
		err := l.WithLock(txCtx, "k", txlock.Options{Wait: 50 * time.Millisecond}, func(context.Context) error {
			t.Error("fn called on contention")
			return nil
		})
		if !errors.Is(err, txlock.ErrContended) {
			t.Errorf("WithLock: want ErrContended, got %v", err)
		}
		if err := l.Lock(txCtx, txlock.Options{Wait: 50 * time.Millisecond}, txlock.StringKey("k")); !errors.Is(err, txlock.ErrContended) {
			t.Errorf("Lock: want ErrContended, got %v", err)
		}
		// lock_timeout was restored with the savepoint.
		var lt string
		if err := db.DBTx(txCtx).QueryRowContext(txCtx, `SHOW lock_timeout`).Scan(&lt); err != nil {
			return err
		}
		if lt != "0" {
			t.Errorf("lock_timeout = %q after failed wait", lt)
		}
		_, err = db.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO t VALUES (2)`)
		return err
	})
	if err != nil {
		t.Fatalf("caller tx aborted: %v", err)
	}
	var n int
	if err := db.DB().QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d, %v", n, err)
	}
}

// A contended TryLock of several keys keeps none of them.
func TestTryLockAllOrNothing(t *testing.T) {
	ctx := t.Context()
	l, db := newLocker(t)
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- l.WithKeys(ctx, []txlock.Key{txlock.Int64Key(2)}, txlock.Options{}, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		if err := l.TryLock(txCtx, txlock.Int64Key(1), txlock.Int64Key(2)); !errors.Is(err, txlock.ErrContended) {
			t.Errorf("want ErrContended, got %v", err)
		}
		// Key 1 is not held by this transaction, so another one can take it.
		return l.TryWithKeys(context.WithoutCancel(ctx), []txlock.Key{txlock.Int64Key(1)}, noop)
	})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLockRequiresTx(t *testing.T) {
	l, _ := newLocker(t)
	if err := l.Lock(t.Context(), txlock.Options{}, txlock.Int64Key(1)); !errors.Is(err, txlock.ErrNotInTx) {
		t.Fatal(err)
	}
	if err := l.TryLock(t.Context(), txlock.Int64Key(1)); !errors.Is(err, txlock.ErrNotInTx) {
		t.Fatal(err)
	}
}

func TestInvalidArguments(t *testing.T) {
	l, _ := newLocker(t)
	ctx := t.Context()
	for name, err := range map[string]error{
		"empty key":    l.WithLock(ctx, "", txlock.Options{}, noop),
		"no keys":      l.WithLocks(ctx, nil, txlock.Options{}, noop),
		"nil fn":       l.WithLock(ctx, "k", txlock.Options{}, nil),
		"zero Key":     l.WithKeys(ctx, []txlock.Key{{}}, txlock.Options{}, noop),
		"empty pair":   l.WithKeys(ctx, []txlock.Key{txlock.StringPairKey("a", "")}, txlock.Options{}, noop),
		"negative":     l.WithLock(ctx, "k", txlock.Options{Wait: -1}, noop),
		"wait too big": l.WithLock(ctx, "k", txlock.Options{Wait: 2147483648 * time.Millisecond}, noop),
	} {
		if !errors.Is(err, durable.ErrInvalidArgument) {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
	}
}

func TestReentrantAndBoundaryKeys(t *testing.T) {
	l, db := newLocker(t)
	keys := []txlock.Key{
		txlock.StringKey(t.Name()),
		txlock.Int64Key(math.MinInt64), txlock.Int64Key(math.MaxInt64),
		txlock.PairKey(math.MinInt32, math.MaxInt32),
		txlock.StringPairKey("Foo", "Bar"),
	}
	err := db.RunInTx(t.Context(), func(txCtx context.Context) error {
		for range 3 {
			if err := l.TryLock(txCtx, keys...); err != nil {
				return err
			}
			if err := l.Lock(txCtx, txlock.Options{}, keys...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestKeyKindsConflictOnlyWithThemselves(t *testing.T) {
	ctx := t.Context()
	l, db := newLocker(t)
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- l.WithKeys(ctx, []txlock.Key{txlock.Int64Key(7), txlock.PairKey(1, 2), txlock.StringPairKey("a", "b")}, txlock.Options{}, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	try := func(k txlock.Key) error { return l.TryWithKeys(ctx, []txlock.Key{k}, noop) }
	for _, k := range []txlock.Key{txlock.Int64Key(7), txlock.PairKey(1, 2), txlock.StringPairKey("a", "b")} {
		if err := try(k); !errors.Is(err, txlock.ErrContended) {
			t.Errorf("%v: want ErrContended, got %v", k, err)
		}
	}
	for _, k := range []txlock.Key{txlock.Int64Key(8), txlock.PairKey(2, 1), txlock.StringPairKey("b", "a"), txlock.PairKey(0, 7)} {
		if err := try(k); err != nil {
			t.Errorf("%v: %v", k, err)
		}
	}
	close(release)
	_ = db
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTryWithLockConcurrent(t *testing.T) {
	ctx := t.Context()
	l, _ := newLocker(t)
	held, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = l.TryWithLock(ctx, "k", func(context.Context) error { close(held); <-release; return nil })
	})
	<-held
	for range 3 {
		if err := l.TryWithLock(ctx, "k", noop); !errors.Is(err, txlock.ErrContended) {
			t.Errorf("want ErrContended, got %v", err)
		}
	}
	close(release)
	wg.Wait()
}

// Waiters run one at a time after the holder.
func TestWithLockSerializes(t *testing.T) {
	ctx := t.Context()
	l, _ := newLocker(t)
	var mu sync.Mutex
	inside, maxInside, total := 0, 0, 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			err := l.WithKeys(ctx, []txlock.Key{txlock.PairKey(1, 1)}, txlock.Options{Wait: 10 * time.Second}, func(context.Context) error {
				mu.Lock()
				inside++
				total++
				maxInside = max(maxInside, inside)
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if maxInside != 1 || total != 8 {
		t.Fatalf("max concurrent = %d, total = %d", maxInside, total)
	}
}

func TestLockReleasedOnRollback(t *testing.T) {
	ctx := t.Context()
	l, db := newLocker(t)
	boom := errors.New("boom")
	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		if err := l.Lock(txCtx, txlock.Options{}, txlock.Int64Key(5)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := l.TryWithKeys(ctx, []txlock.Key{txlock.Int64Key(5)}, noop); err != nil {
		t.Fatalf("lock survived rollback: %v", err)
	}
}

func TestKeyString(t *testing.T) {
	for k, want := range map[txlock.Key]string{
		txlock.StringKey("a"): "a", txlock.Int64Key(-1): "-1",
		txlock.PairKey(1, 2): "1, 2", txlock.StringPairKey("Foo", "Bar"): "Foo, Bar", {}: "<invalid>",
	} {
		if got := k.String(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
