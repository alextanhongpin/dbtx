package lock_test

import (
	_ "github.com/lib/pq"

	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/lock"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/go-openapi/testify/assert"
)

func TestMain(m *testing.M) {
	opts := dbtest.Options{
		Image: "postgres:19beta3-alpine3.24",
		Hook:  migrate,
	}
	stop := dbtest.Init(opts)
	defer stop()
	m.Run()
}

func migrate(dsn string) error {
	return nil
}

func TestLock(t *testing.T) {
	ctx := t.Context()
	db := dbtx.New(dbtest.DB(t))

	// Given a transaction block,
	err := db.RunInTx(ctx, func(ctx context.Context) error {
		for range 3 {
			// When calling lock on the same key
			locked, err := lock.TryLock(ctx, t.Name())
			if err != nil {
				return err
			}
			// Then it should always return true.
			if !locked {
				t.Fatal("want locked")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTryLock_Concurrent(t *testing.T) {
	ctx := t.Context()
	db := dbtx.New(dbtest.DB(t))

	var wg sync.WaitGroup
	work := func(sleep time.Duration, want bool) {
		wg.Go(func() {
			// When a lock is acquired by another process,
			err := db.RunInTx(ctx, func(ctx context.Context) error {
				locked, err := lock.TryLock(ctx, t.Name())
				if err != nil {
					return err
				}
				if want != locked {
					t.Fatalf("want %t, got %t", want, locked)
				}
				time.Sleep(sleep)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}

	// When a process acquire a lock,
	// And has not released it.
	work(50*time.Millisecond, true)

	// Then all other processes will received "locked=false".
	time.Sleep(10 * time.Millisecond)
	work(0, false)
	work(0, false)
	work(0, false)
	wg.Wait()
}

func TestLock_Concurrent(t *testing.T) {
	ctx := t.Context()
	db := dbtx.New(dbtest.DB(t))

	var wg sync.WaitGroup
	ch := make(chan string, 4)
	work := func(sleep time.Duration, id string) {
		wg.Go(func() {
			err := db.RunInTx(ctx, func(ctx context.Context) error {
				err := lock.Lock(ctx, t.Name())
				if err != nil {
					return err
				}
				time.Sleep(sleep)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ch <- id
		})
	}

	// When a lock is acquired by another process,
	// And has not released it,
	work(50*time.Millisecond, "one")
	// Then other process will be waiting,
	// And after completed, the next locker will acquire the lock.
	time.Sleep(10 * time.Millisecond)
	work(10*time.Millisecond, "two")
	time.Sleep(5 * time.Millisecond)
	work(10*time.Millisecond, "three")
	time.Sleep(5 * time.Millisecond)
	work(10*time.Millisecond, "four")
	wg.Wait()
	close(ch)
	res := []string{"one", "two", "three", "four"}
	for v := range ch {
		want := res[0]
		res = res[1:]
		got := v
		if want != got {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

func TestAtomicIntKeyPairLocked(t *testing.T) {
	key := lock.Pair[int32]{Key1: 1, Key2: 1}
	atm := dbtx.New(dbtest.DB(t))
	err := atm.RunInTx(t.Context(), func(txCtx context.Context) error {
		if locked, err := lock.TryLock(txCtx, key); err != nil {
			return err
		} else if !locked {
			return errors.New("failed to acquire lock")
		}

		// Locking twice in the same transaction will not cause a deadlock.
		if locked, err := lock.TryLock(txCtx, key); err != nil {
			return err
		} else if !locked {
			return errors.New("failed to acquire lock")
		}

		return nil
	})
	assert.Nil(t, err)
}

func TestAtomicLockBoundary(t *testing.T) {
	is := assert.New(t)
	tx := dbtx.New(dbtest.DB(t))
	err := tx.RunInTx(t.Context(), func(ctx context.Context) error {
		is.NoError(lock.Lock(ctx, lock.Pair[int32]{Key1: math.MinInt32, Key2: math.MaxInt32}))
		is.NoError(lock.Lock(ctx, int64(math.MinInt64)))
		is.NoError(lock.Lock(ctx, int64(math.MaxInt64)))

		return nil
	})
	is.NoError(err)
}

func TestAtomicIntLockKeyLocked(t *testing.T) {
	atm := dbtx.New(dbtest.DB(t))
	key := int64(10)

	is := assert.New(t)

	var wg sync.WaitGroup

	wg.Go(func() {
		err := atm.RunInTx(t.Context(), func(txCtx context.Context) error {
			if locked, err := lock.TryLock(txCtx, key); err != nil {
				return err
			} else if !locked {
				return errors.New("already locked")
			}

			t.Log("goroutine0: locked=true")
			time.Sleep(100 * time.Millisecond)

			return nil
		})
		is.NoError(err)
	})

	time.Sleep(50 * time.Millisecond)
	err := atm.RunInTx(t.Context(), func(txCtx context.Context) error {
		locked, err := lock.TryLock(txCtx, key)
		if err != nil {
			return err
		}

		// ̃NOTE: Both of this is expected to return false, but it is true now
		// because of the test library which puts everything in a single transaction.
		//assert.False(locked1)
		//assert.False(locked2)
		t.Logf("goroutine1: locked1=%t\n", locked)
		if !locked {
			// To indicate it is locked.
			return assert.AnError
		}
		return err
	})
	is.ErrorIs(err, assert.AnError)
	wg.Wait()
}

func TestAtomicLocker(t *testing.T) {
	is := assert.New(t)

	// Arrange.
	ctx := t.Context()
	key := "The meaning of life..."

	db := dbtx.New(dbtest.DB(t))

	var wg sync.WaitGroup

	wg.Go(func() {

		var errTimeout = errors.New("timeout")
		ctx, cancel := context.WithTimeoutCause(ctx, 1*time.Second, errTimeout)
		defer cancel()

		// Lock1 locks the key successfully. Forgetting to call unlock locks the key
		// forever unless a timeout is set.
		err := db.RunInTx(ctx, func(ctx context.Context) error {
			is.True(dbtx.IsTx(ctx))

			locked, err := lock.TryLock(ctx, key)
			is.NoError(err)
			is.True(locked)

			<-ctx.Done()
			return context.Cause(ctx)
		})
		is.ErrorIs(err, errTimeout)
	})

	wg.Go(func() {
		time.Sleep(10 * time.Millisecond)

		// Lock2 fails when locking the same key.
		err := db.RunInTx(ctx, func(ctx context.Context) error {
			is.True(dbtx.IsTx(ctx))
			locked, err := lock.TryLock(ctx, key)
			if err != nil {
				return err
			}
			if !locked {
				return assert.AnError
			}
			return nil
		})
		is.ErrorIs(err, assert.AnError)
	})

	wg.Go(func() {
		time.Sleep(10 * time.Millisecond)

		// Lock3 will wait for the previous lock to be released.
		err := db.RunInTx(ctx, func(ctx context.Context) error {
			is.True(dbtx.IsTx(ctx))
			return lock.Lock(ctx, key)
		})
		is.NoError(err)
	})

	wg.Wait()
}
