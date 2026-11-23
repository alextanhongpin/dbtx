package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/cache"
)

func TestExistsAndTTL(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	if ok, err := c.Exists(ctx, "k"); err != nil || ok {
		t.Fatalf("missing key: %v %v", ok, err)
	}
	if _, err := c.TTL(ctx, "k"); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("TTL of missing key: %v", err)
	}
	_ = c.Set(ctx, "k", 1, time.Minute, 0)
	if ok, _ := c.Exists(ctx, "k"); !ok {
		t.Fatal("want exists")
	}
	ttl, err := c.TTL(ctx, "k")
	if err != nil || ttl <= 50*time.Second || ttl > time.Minute {
		t.Fatalf("TTL %v %v", ttl, err)
	}
	_ = c.Delete(ctx, "k")
	if ok, _ := c.Exists(ctx, "k"); ok {
		t.Fatal("tombstone exists")
	}
}

// A stale value does not exist for the key-value operations.
func TestStaleIsNotExists(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	_ = c.Set(ctx, "k", 1, 10*time.Millisecond, time.Minute)
	time.Sleep(20 * time.Millisecond)
	if ok, _ := c.Exists(ctx, "k"); ok {
		t.Fatal("stale value exists")
	}
	if _, err := c.TTL(ctx, "k"); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("TTL: %v", err)
	}
	if err := c.StoreOnce(ctx, "k", 2, time.Minute, 0); err != nil {
		t.Fatalf("StoreOnce over stale: %v", err)
	}
	if v, _, _ := c.Get[int](ctx, "k"); v != 2 {
		t.Fatalf("got %d", v)
	}
}

func TestExpire(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	if err := c.Expire(ctx, "k", time.Minute, 0); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	if err := c.Expire(ctx, "k", 0, 0); !errors.Is(err, durable.ErrInvalidArgument) {
		t.Fatalf("zero TTL: %v", err)
	}
	_ = c.Set(ctx, "k", 1, time.Minute, 0)
	if err := c.Expire(ctx, "k", time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if ttl, _ := c.TTL(ctx, "k"); ttl < 59*time.Minute {
		t.Fatalf("TTL %v after Expire(1h)", ttl)
	}
	// Shorten to expire with a stale window: served stale while recomputing.
	_ = c.Expire(ctx, "k", 10*time.Millisecond, time.Minute)
	time.Sleep(20 * time.Millisecond)
	if _, ok, _ := c.Get[int](ctx, "k"); ok {
		t.Fatal("still fresh")
	}
	started, release := make(chan struct{}), make(chan struct{})
	opts := cache.Options{TTL: time.Minute, StaleFor: time.Minute}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.GetOrCompute(ctx, "k", opts, func(context.Context) (int, error) {
			close(started)
			<-release
			return 2, nil
		})
	}()
	<-started
	if v, err := c.GetOrCompute(ctx, "k", opts, func(context.Context) (int, error) { return 3, nil }); err != nil || v != 1 {
		t.Fatalf("want stale 1, got %v %v", v, err)
	}
	close(release)
	<-done
}

func TestStoreOnce(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	if err := c.StoreOnce(ctx, "k", "a", time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.StoreOnce(ctx, "k", "b", time.Minute, 0); !errors.Is(err, cache.ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	if v, _, _ := c.Get[string](ctx, "k"); v != "a" {
		t.Fatalf("got %q", v)
	}
	// After invalidation the tombstone is replaced.
	_ = c.Delete(ctx, "k")
	if err := c.StoreOnce(ctx, "k", "c", time.Minute, 0); err != nil {
		t.Fatal(err)
	}
}

func TestStoreOnceConcurrent(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	var stored atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			err := c.StoreOnce(ctx, "k", i, time.Minute, 0)
			switch {
			case err == nil:
				stored.Add(1)
			case !errors.Is(err, cache.ErrExists):
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if stored.Load() != 1 {
		t.Fatalf("stored %d times", stored.Load())
	}
}

func TestLoadOrStore(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	v, loaded, err := c.LoadOrStore(ctx, "k", chart{Items: []string{"a"}}, time.Minute, 0)
	if err != nil || loaded || v.Items[0] != "a" {
		t.Fatalf("first: %v %v %v", v, loaded, err)
	}
	v, loaded, err = c.LoadOrStore(ctx, "k", chart{Items: []string{"b"}}, time.Minute, 0)
	if err != nil || !loaded || v.Items[0] != "a" {
		t.Fatalf("second: %v %v %v", v, loaded, err)
	}
}

// Concurrent LoadOrStore callers agree on one value.
func TestLoadOrStoreConcurrent(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	var mu sync.Mutex
	seen := map[int]bool{}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			v, _, err := c.LoadOrStore(ctx, "k", i, time.Minute, 0)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			seen[v] = true
			mu.Unlock()
		})
	}
	wg.Wait()
	if len(seen) != 1 {
		t.Fatalf("callers saw %d different values", len(seen))
	}
}

func TestCompareAndSwap(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	if err := c.CompareAndSwap(ctx, "k", 1, 2, time.Minute, 0); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	_ = c.Set(ctx, "k", 1, time.Minute, 0)
	if err := c.CompareAndSwap(ctx, "k", 5, 2, time.Minute, 0); !errors.Is(err, cache.ErrConflict) {
		t.Fatalf("mismatch: %v", err)
	}
	if err := c.CompareAndSwap(ctx, "k", 1, 2, time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := c.Get[int](ctx, "k"); v != 2 {
		t.Fatalf("got %d", v)
	}
	// Equality is jsonb equality: key order and formatting do not matter.
	_ = c.Set(ctx, "j", json.RawMessage(`{"a":1,"b":2}`), time.Minute, 0)
	if err := c.CompareAndSwap(ctx, "j", json.RawMessage(`{ "b": 2, "a": 1 }`), json.RawMessage(`{}`), time.Minute, 0); err != nil {
		t.Fatal(err)
	}
}

// Exactly one of many concurrent swaps from the same old value wins.
func TestCompareAndSwapConcurrent(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	_ = c.Set(ctx, "k", 0, time.Minute, 0)
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			err := c.CompareAndSwap(ctx, "k", 0, i+1, time.Minute, 0)
			switch {
			case err == nil:
				won.Add(1)
			case !errors.Is(err, cache.ErrConflict):
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d swaps won", won.Load())
	}
}

func TestCompareAndDelete(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	if err := c.CompareAndDelete(ctx, "k", 1); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	_ = c.Set(ctx, "k", 1, time.Minute, 0)
	if err := c.CompareAndDelete(ctx, "k", 2); !errors.Is(err, cache.ErrConflict) {
		t.Fatalf("mismatch: %v", err)
	}
	if err := c.CompareAndDelete(ctx, "k", 1); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Exists(ctx, "k"); ok {
		t.Fatal("still exists")
	}
	if err := c.CompareAndDelete(ctx, "k", 1); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("deleted key: %v", err)
	}
}

func TestLoadAndDelete(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	if _, err := c.LoadAndDelete[int](ctx, "k"); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	_ = c.Set(ctx, "k", 1, time.Minute, 0)
	v, err := c.LoadAndDelete[int](ctx, "k")
	if err != nil || v != 1 {
		t.Fatalf("%v %v", v, err)
	}
	if _, err := c.LoadAndDelete[int](ctx, "k"); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("deleted key: %v", err)
	}
	// A stale value is not returned, but it is invalidated.
	_ = c.Set(ctx, "s", 1, 10*time.Millisecond, time.Minute)
	time.Sleep(20 * time.Millisecond)
	if _, err := c.LoadAndDelete[int](ctx, "s"); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("stale key: %v", err)
	}
	v, err = c.GetOrCompute(ctx, "s", cache.Options{TTL: time.Minute, StaleFor: time.Minute},
		func(context.Context) (int, error) { return 2, nil })
	if err != nil || v != 2 {
		t.Fatalf("stale value served after LoadAndDelete: %v %v", v, err)
	}
}

// Exactly one concurrent LoadAndDelete gets the value.
func TestLoadAndDeleteConcurrent(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	_ = c.Set(ctx, "k", 1, time.Minute, 0)
	var got atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, err := c.LoadAndDelete[int](ctx, "k")
			switch {
			case err == nil:
				got.Add(1)
			case !errors.Is(err, durable.ErrNotFound):
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got.Load() != 1 {
		t.Fatalf("%d callers got the value", got.Load())
	}
}

// Writes during a computation win: the computation cannot store its value.
func TestWritesWinOverComputation(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	writes := map[string]func() error{
		"StoreOnce":   func() error { return c.StoreOnce(ctx, "k", 2, time.Minute, 0) },
		"LoadOrStore": func() error { _, _, err := c.LoadOrStore(ctx, "k", 2, time.Minute, 0); return err },
		"Set":         func() error { return c.Set(ctx, "k", 2, time.Minute, 0) },
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			_ = c.Delete(ctx, "k")
			started, release := make(chan struct{}), make(chan struct{})
			done := make(chan int)
			go func() {
				v, _ := c.GetOrCompute(ctx, "k", cache.Options{TTL: time.Minute}, func(context.Context) (int, error) {
					close(started)
					<-release
					return 1, nil
				})
				done <- v
			}()
			<-started
			if err := write(); err != nil {
				t.Fatal(err)
			}
			close(release)
			if v := <-done; v != 1 {
				t.Fatalf("computation returned %d", v)
			}
			if v, _, _ := c.Get[int](ctx, "k"); v != 2 {
				t.Fatalf("stored %d, want the write", v)
			}
		})
	}
}

// Key-value writes join the transaction in ctx.
func TestKVInTx(t *testing.T) {
	ctx := t.Context()
	c, db := setup(t)
	_ = c.Set(ctx, "k", 1, time.Minute, 0)
	_ = db.RunInTx(ctx, func(txCtx context.Context) error {
		if err := c.CompareAndSwap(txCtx, "k", 1, 2, time.Minute, 0); err != nil {
			t.Fatal(err)
		}
		if err := c.StoreOnce(txCtx, "n", 1, time.Minute, 0); err != nil {
			t.Fatal(err)
		}
		return errors.New("rollback")
	})
	if v, _, _ := c.Get[int](ctx, "k"); v != 1 {
		t.Fatalf("rolled-back swap stored %d", v)
	}
	if ok, _ := c.Exists(ctx, "n"); ok {
		t.Fatal("rolled-back StoreOnce stored a value")
	}
}

func TestInvalidArguments(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	for name, err := range map[string]error{
		"StoreOnce":      c.StoreOnce(ctx, "k", 1, 0, 0),
		"CompareAndSwap": c.CompareAndSwap(ctx, "k", 1, 2, time.Minute, -1),
		"Expire":         c.Expire(ctx, "k", -time.Second, 0),
	} {
		if !errors.Is(err, durable.ErrInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := c.LoadOrStore(ctx, "k", 1, 0, 0); !errors.Is(err, durable.ErrInvalidArgument) {
		t.Errorf("LoadOrStore: %v", err)
	}
}

func TestPrefix(t *testing.T) {
	ctx := t.Context()
	c, db := setup(t) // prefix "test:"
	other, _ := cache.New(cache.Config{DB: db, Prefix: "other:"})
	_ = c.Set(ctx, "k", 1, time.Minute, 0)
	if ok, _ := other.Exists(ctx, "k"); ok {
		t.Fatal("prefixes share keys")
	}
	if ok, _ := other.Exists(ctx, "test:k"); ok {
		t.Fatal("prefix was not applied")
	}
}
