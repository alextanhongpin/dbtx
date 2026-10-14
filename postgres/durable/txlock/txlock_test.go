package txlock_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/txlock"
)

func TestContentionAndWait(t *testing.T) {
	ctx := t.Context()
	db := dbtx.New(durabletest.NewDB(t))
	l := txlock.New(db)
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = l.WithLock(ctx, "seat:1", txlock.Options{}, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	if err := l.TryWithLock(ctx, "seat:1", func(context.Context) error { return nil }); !errors.Is(err, txlock.ErrContended) {
		t.Fatalf("try: want ErrContended, got %v", err)
	}
	err := l.WithLock(ctx, "seat:1", txlock.Options{Wait: 50 * time.Millisecond}, func(context.Context) error { return nil })
	if !errors.Is(err, txlock.ErrContended) {
		t.Fatalf("wait: want ErrContended, got %v", err)
	}
	close(release)
	if err := l.WithLock(ctx, "seat:1", txlock.Options{Wait: 2 * time.Second}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// S2: opposite lock orders must not deadlock.
func TestWithLocksSortsKeys(t *testing.T) {
	ctx := t.Context()
	db := dbtx.New(durabletest.NewDB(t))
	l := txlock.New(db)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 40 {
		keys := []string{"wallet:a", "wallet:b"}
		if i%2 == 1 {
			keys = []string{"wallet:b", "wallet:a", "wallet:b"}
		}
		wg.Go(func() {
			errs <- l.WithLocks(ctx, keys, txlock.Options{Wait: 10 * time.Second}, func(context.Context) error {
				time.Sleep(time.Millisecond)
				return nil
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
