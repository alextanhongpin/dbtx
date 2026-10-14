package lease

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alextanhongpin/dbtx"
)

// A context-insensitive response models a delayed network reply. Production
// database drivers must honor request contexts for goroutine drain on shutdown.
type faultState struct {
	acquire func() (driver.Value, error)
	renew   func() (driver.Value, error)
	release func() error
}

var faultStates sync.Map
var faultID atomic.Int64

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
		if c.state.acquire != nil {
			value, err = c.state.acquire()
		} else {
			value = int64(1)
		}
	case strings.Contains(query, "renew_lease"):
		if c.state.renew != nil {
			value, err = c.state.renew()
		} else {
			value = true
		}
	default:
		return nil, errors.New("unexpected Query")
	}
	if err != nil {
		return nil, err
	}
	return &faultRows{value: value}, nil
}
func (c *faultConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
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
func init() { sql.Register("lease-fault", faultDriver{}) }

func newFaultLocker(t *testing.T, state *faultState) *Locker {
	t.Helper()
	name := string(rune(faultID.Add(1)))
	faultStates.Store(name, state)
	db, err := sql.Open("lease-fault", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); faultStates.Delete(name) })
	l, err := New(Config{DB: dbtx.New(db), Namespace: "fault", LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLateAcquireResponseDoesNotInvokeCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cleaned bool
		l := newFaultLocker(t, &faultState{
			acquire: func() (driver.Value, error) { time.Sleep(time.Second); return int64(1), nil },
			release: func() error { cleaned = true; return nil },
		})
		called := false
		err := l.Activity(context.Background(), "resource", func(context.Context, Lease) error { called = true; return nil })
		if !errors.Is(err, ErrLeaseLost) || called || !cleaned {
			t.Fatalf("%v called=%v cleaned=%v", err, called, cleaned)
		}
	})
}

func TestLateRenewalDoesNotReviveAndCancelsActivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		unblock := make(chan struct{})
		observed := make(chan error, 1)
		result := make(chan error, 1)
		l := newFaultLocker(t, &faultState{renew: func() (driver.Value, error) {
			close(entered)
			<-unblock
			return true, nil
		}})
		var held Lease
		go func() {
			result <- l.Activity(context.Background(), "resource", func(ctx context.Context, h Lease) error {
				held = h
				<-ctx.Done()
				observed <- context.Cause(ctx)
				return nil
			})
		}()
		time.Sleep(time.Second / 3)
		synctest.Wait()
		<-entered
		time.Sleep(time.Second)
		synctest.Wait()
		// Cancellation is observable before the delayed renewal returns.
		if err := <-observed; !errors.Is(err, ErrLeaseLost) {
			t.Fatal(err)
		}
		close(unblock)
		if err := <-result; !errors.Is(err, ErrLeaseLost) {
			t.Fatal(err)
		}
		if !errors.Is(held.Err(), ErrLeaseLost) {
			t.Fatal("late reply revived lease")
		}
	})
}

func TestReleaseFailureCanBeRetried(t *testing.T) {
	failure := errors.New("release reply lost")
	calls := 0
	l := newFaultLocker(t, &faultState{release: func() error {
		calls++
		if calls == 1 {
			return failure
		}
		return nil
	}})
	held, err := l.Acquire(context.Background(), "resource")
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Release(context.Background()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if !errors.Is(held.Err(), ErrReleased) {
		t.Fatal("failed release left lease active")
	}
	if err := held.Renew(context.Background()); !errors.Is(err, ErrReleased) {
		t.Fatal(err)
	}
	if err := held.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := held.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("release calls=%d", calls)
	}
}
