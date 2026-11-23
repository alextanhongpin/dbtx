package cache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/durable/cache"
)

// A computation slower than ComputeTTL keeps its lease: nobody else computes.
func TestSlowComputeRenewsLease(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	opts := cache.Options{TTL: time.Minute, ComputeTTL: 150 * time.Millisecond, PollInterval: 10 * time.Millisecond}
	var calls atomic.Int32
	fn := func(context.Context) (int, error) {
		calls.Add(1)
		time.Sleep(600 * time.Millisecond) // several ComputeTTLs
		return 42, nil
	}
	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		v, err := c.GetOrCompute(ctx, "k", opts, func(ctx context.Context) (int, error) {
			close(started)
			return fn(ctx)
		})
		if err != nil || v != 42 {
			t.Errorf("first: %v %v", v, err)
		}
	})
	<-started
	for range 10 {
		wg.Go(func() {
			v, err := c.GetOrCompute(ctx, "k", opts, fn)
			if err != nil || v != 42 {
				t.Errorf("waiter: %v %v", v, err)
			}
		})
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("computed %d times", n)
	}
}

// MaxWait bounds how long a waiter waits for another caller's computation.
func TestMaxWait(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	started, release := make(chan struct{}), make(chan struct{})
	opts := cache.Options{TTL: time.Minute, ComputeTTL: time.Minute, PollInterval: 10 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.GetOrCompute(ctx, "k", opts, func(context.Context) (int, error) {
			close(started)
			<-release
			return 1, nil
		})
	}()
	<-started
	opts.MaxWait = 50 * time.Millisecond
	begin := time.Now()
	v, err := c.GetOrCompute(ctx, "k", opts, func(context.Context) (int, error) { return 2, nil })
	if err != nil || v != 2 {
		t.Fatalf("%v %v", v, err)
	}
	if d := time.Since(begin); d < 50*time.Millisecond {
		t.Fatalf("computed after %v, before MaxWait", d)
	}
	close(release)
	<-done
	if v, _, _ := c.Get[int](ctx, "k"); v != 2 {
		t.Fatalf("stored %d, want the newer value", v)
	}
}

// A waiter without MaxWait gives up when its context is done.
func TestWaitHonoursContext(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	started, release := make(chan struct{}), make(chan struct{})
	opts := cache.Options{TTL: time.Minute, PollInterval: 10 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.GetOrCompute(ctx, "k", opts, func(context.Context) (int, error) {
			close(started)
			<-release
			return 1, nil
		})
	}()
	<-started
	wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := c.GetOrCompute(wctx, "k", opts, func(context.Context) (int, error) {
		t.Error("must not compute")
		return 0, nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	close(release)
	<-done
}

// A panicking fn releases the compute lease before the panic propagates.
func TestComputePanicReleasesLease(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	opts := cache.Options{TTL: time.Minute, ComputeTTL: time.Minute}
	func() {
		defer func() {
			if p := recover(); p != "boom" {
				t.Fatalf("recovered %v, want boom", p)
			}
		}()
		_, _ = c.GetOrCompute(ctx, "k", opts, func(context.Context) (int, error) { panic("boom") })
	}()
	// The lease would otherwise be held for a minute and this call would wait.
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	v, err := c.GetOrCompute(wctx, "k", opts, func(context.Context) (int, error) { return 7, nil })
	if err != nil || v != 7 {
		t.Fatalf("%v %v", v, err)
	}
}
