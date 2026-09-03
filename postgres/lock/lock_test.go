package lock_test

import (
	_ "github.com/lib/pq"

	"context"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/lock"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
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
