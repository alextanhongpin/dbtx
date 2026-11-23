package lease_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable/lease"
)

// faultDriver answers the lease queries without a database. Its responses
// ignore request contexts, modelling a delayed network reply.
type faultState struct {
	acquire func() (driver.Value, error)
	renew   func() (driver.Value, error)
	release func() error
}

var (
	faultStates sync.Map
	faultID     atomic.Int64
)

type faultDriver struct{}

func (faultDriver) Open(name string) (driver.Conn, error) {
	s, _ := faultStates.Load(name)
	return &faultConn{state: s.(*faultState)}, nil
}

type faultConn struct{ state *faultState }

func (*faultConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected Prepare") }
func (*faultConn) Close() error                        { return nil }
func (*faultConn) Begin() (driver.Tx, error)           { return nil, errors.New("unexpected Begin") }

func (c *faultConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	var value driver.Value
	var err error
	switch {
	case strings.Contains(query, "acquire_lease"):
		value = int64(1)
		if c.state.acquire != nil {
			value, err = c.state.acquire()
		}
	case strings.Contains(query, "renew_lease"):
		value = true
		if c.state.renew != nil {
			value, err = c.state.renew()
		}
	default:
		return nil, errors.New("unexpected Query")
	}
	if err != nil {
		return nil, err
	}
	return &faultRows{value: value}, nil
}

func (c *faultConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	if c.state.release != nil {
		if err := c.state.release(); err != nil {
			return nil, err
		}
	}
	return driver.RowsAffected(1), nil
}

type faultRows struct {
	value driver.Value
	read  bool
}

func (*faultRows) Columns() []string { return []string{"result"} }
func (*faultRows) Close() error      { return nil }
func (r *faultRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	dest[0] = r.value
	return nil
}

func init() { sql.Register("durable-lease-fault", faultDriver{}) }

func newFaultLocker(t *testing.T, state *faultState) *lease.Locker {
	t.Helper()
	name := strconv.FormatInt(faultID.Add(1), 10)
	faultStates.Store(name, state)
	db, err := sql.Open("durable-lease-fault", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); faultStates.Delete(name) })
	l, err := lease.New(lease.Config{DB: dbtx.New(db), Namespace: "fault", LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// An acquisition whose reply arrives too late never runs the callback, and
// the possibly granted lease is cleaned up by owner.
func TestLateAcquireResponseDoesNotInvokeCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cleaned atomic.Bool
		l := newFaultLocker(t, &faultState{
			acquire: func() (driver.Value, error) { time.Sleep(time.Second); return int64(1), nil },
			release: func() error { cleaned.Store(true); return nil },
		})
		called := false
		err := l.Activity(context.Background(), "resource", func(context.Context, lease.Lease) error { called = true; return nil })
		if err == nil || called || !cleaned.Load() {
			t.Fatalf("err=%v called=%v cleaned=%v", err, called, cleaned.Load())
		}
	})
}

// A renewal reply that arrives after the local deadline cannot revive the
// lease, and the activity is cancelled before the reply arrives.
func TestLateRenewalDoesNotReviveAndCancelsActivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, unblock := make(chan struct{}), make(chan struct{})
		var once sync.Once
		observed, result := make(chan error, 1), make(chan error, 1)
		l := newFaultLocker(t, &faultState{renew: func() (driver.Value, error) {
			once.Do(func() { close(entered) })
			<-unblock
			return true, nil
		}})
		var held lease.Lease
		go func() {
			result <- l.Activity(context.Background(), "resource", func(ctx context.Context, h lease.Lease) error {
				held = h
				<-ctx.Done()
				observed <- context.Cause(ctx)
				return nil
			})
		}()
		<-entered
		time.Sleep(time.Second)
		synctest.Wait()
		if err := <-observed; !errors.Is(err, lease.ErrLeaseLost) {
			t.Fatal(err)
		}
		close(unblock)
		if err := <-result; !errors.Is(err, lease.ErrLeaseLost) {
			t.Fatal(err)
		}
		if !errors.Is(held.Err(), lease.ErrLeaseLost) {
			t.Fatal("late reply revived lease")
		}
	})
}

func TestReleaseFailureCanBeRetried(t *testing.T) {
	failure := errors.New("release reply lost")
	var calls atomic.Int64
	l := newFaultLocker(t, &faultState{release: func() error {
		if calls.Add(1) == 1 {
			return failure
		}
		return nil
	}})
	ctx := context.Background()
	held, err := l.Acquire(ctx, "resource")
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Release(ctx); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if !errors.Is(held.Err(), lease.ErrReleased) {
		t.Fatal("failed release left lease active")
	}
	if err := held.Renew(ctx); !errors.Is(err, lease.ErrReleased) {
		t.Fatal(err)
	}
	if err := held.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := held.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("release calls=%d", n)
	}
}

func TestActivityPanicReleasesWithoutDatabase(t *testing.T) {
	var released atomic.Bool
	l := newFaultLocker(t, &faultState{release: func() error { released.Store(true); return nil }})
	func() {
		defer func() {
			if recover() != "boom" {
				t.Error("panic not propagated")
			}
		}()
		_ = l.Activity(context.Background(), "resource", func(context.Context, lease.Lease) error { panic("boom") })
	}()
	if !released.Load() {
		t.Fatal("lease not released after panic")
	}
}
