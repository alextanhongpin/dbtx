package cache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/cache"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
)

type chart struct{ Items []string }

func setup(t *testing.T) (*cache.Cache, *dbtx.DB) {
	t.Helper()
	db := dbtx.New(durabletest.NewDB(t))
	c, err := cache.New(cache.Config{DB: db, Prefix: "test:"})
	if err != nil {
		t.Fatal(err)
	}
	return c, db
}

// FM-29: a stampede computes once.
func TestStampedeComputesOnce(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	var calls atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			v, err := c.GetOrCompute(ctx, "chart:sg", cache.Options{TTL: time.Minute},
				func(context.Context) (chart, error) {
					calls.Add(1)
					time.Sleep(150 * time.Millisecond)
					return chart{Items: []string{"a"}}, nil
				})
			if err != nil || len(v.Items) != 1 {
				t.Errorf("%v %v", v, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("computed %d times", calls.Load())
	}
}

// S31: while one caller recomputes, others are served the stale value.
func TestServesStaleWhileRecomputing(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	if err := c.Set(ctx, "k", chart{Items: []string{"old"}}, 10*time.Millisecond, time.Minute); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	started, release := make(chan struct{}), make(chan struct{})
	opts := cache.Options{TTL: time.Minute, StaleFor: time.Minute}
	done := make(chan chart)
	go func() {
		v, _ := c.GetOrCompute(ctx, "k", opts, func(context.Context) (chart, error) {
			close(started)
			<-release
			return chart{Items: []string{"new"}}, nil
		})
		done <- v
	}()
	<-started
	v, err := c.GetOrCompute(ctx, "k", opts, func(context.Context) (chart, error) {
		t.Error("second caller must not compute")
		return chart{}, nil
	})
	if err != nil || v.Items[0] != "old" {
		t.Fatalf("got %v %v, want stale", v, err)
	}
	close(release)
	if v := <-done; v.Items[0] != "new" {
		t.Fatalf("recompute returned %v", v)
	}
	if v, ok, _ := c.Get[chart](ctx, "k"); !ok || v.Items[0] != "new" {
		t.Fatalf("stored %v %v", v, ok)
	}
}

// FM-30: a slow computation cannot overwrite a newer value.
func TestSlowComputeDoesNotOverwriteNewer(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	started, release := make(chan struct{}), make(chan struct{})
	opts := cache.Options{TTL: time.Minute, ComputeTTL: 100 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.GetOrCompute(ctx, "k", opts, func(context.Context) (chart, error) {
			close(started)
			<-release
			return chart{Items: []string{"slow"}}, nil
		})
	}()
	<-started
	time.Sleep(150 * time.Millisecond) // the slow caller's compute lease expires
	if _, err := c.GetOrCompute(ctx, "k", opts, func(context.Context) (chart, error) {
		return chart{Items: []string{"fast"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if v, _, _ := c.Get[chart](ctx, "k"); v.Items[0] != "fast" {
		t.Fatalf("stored %v", v)
	}
}

// S32: invalidation commits or rolls back with the transaction.
func TestInvalidateTx(t *testing.T) {
	ctx := t.Context()
	c, db := setup(t)
	_ = c.Set(ctx, "k", 1, time.Minute, 0)
	if err := c.InvalidateTx(ctx, "k"); !errors.Is(err, durable.ErrNotInTx) {
		t.Fatalf("want ErrNotInTx, got %v", err)
	}
	_ = db.RunInTx(ctx, func(txCtx context.Context) error {
		_ = c.InvalidateTx(txCtx, "k")
		return errors.New("rollback")
	})
	if _, ok, _ := c.Get[int](ctx, "k"); !ok {
		t.Fatal("rolled-back invalidation removed the value")
	}
	_ = db.RunInTx(ctx, func(txCtx context.Context) error { return c.InvalidateTx(txCtx, "k") })
	if _, ok, _ := c.Get[int](ctx, "k"); ok {
		t.Fatal("value still cached")
	}
}

func TestComputeErrorReleasesLease(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	opts := cache.Options{TTL: time.Minute}
	if _, err := c.GetOrCompute(ctx, "k", opts, func(context.Context) (int, error) {
		return 0, errors.New("upstream down")
	}); err == nil {
		t.Fatal("want error")
	}
	v, err := c.GetOrCompute(ctx, "k", opts, func(context.Context) (int, error) { return 7, nil })
	if err != nil || v != 7 {
		t.Fatalf("%v %v", v, err)
	}
}

// Regression: a computation that started before an invalidation cannot
// store its stale result afterwards.
func TestInvalidateDuringCompute(t *testing.T) {
	ctx := t.Context()
	c, db := setup(t)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.GetOrCompute(ctx, "k", cache.Options{TTL: time.Minute}, func(context.Context) (int, error) {
			close(started)
			<-release
			return 1, nil // computed from data that is now outdated
		})
	}()
	<-started
	if err := db.RunInTx(ctx, func(txCtx context.Context) error { return c.InvalidateTx(txCtx, "k") }); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if v, ok, _ := c.Get[int](ctx, "k"); ok {
		t.Fatalf("stale value %d stored after invalidation", v)
	}
}
