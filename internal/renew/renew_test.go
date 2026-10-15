package renew

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestWatchExpiresIndependentlyOfBlockedRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		expired := errors.New("expired")
		deadline := Deadline(time.Now(), time.Second)
		entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(joined)
			Every(ctx, time.Second, nil, func(context.Context) bool { close(entered); <-release; return false })
		}()
		observed := make(chan error, 1)
		go func() {
			observed <- Watch(ctx, nil, func() (time.Time, error) {
				if !time.Now().Before(deadline) {
					return deadline, expired
				}
				return deadline, nil
			})
		}()
		<-entered
		time.Sleep(time.Second)
		if err := <-observed; !errors.Is(err, expired) {
			t.Fatal(err)
		}
		close(release)
		<-joined
	})
}

func TestWatchWakeAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		changed := make(chan struct{}, 1)
		deadline := time.Now().Add(time.Hour)
		observed := make(chan error, 1)
		go func() {
			observed <- Watch(ctx, changed, func() (time.Time, error) {
				if !time.Now().Before(deadline) {
					return deadline, context.DeadlineExceeded
				}
				return deadline, nil
			})
		}()
		synctest.Wait()
		deadline = time.Now().Add(time.Second)
		changed <- struct{}{}
		synctest.Wait()
		time.Sleep(time.Second)
		if err := <-observed; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		cause := errors.New("stopped")
		cancel(cause)
		if err := Watch(ctx, nil, func() (time.Time, error) { return time.Now().Add(time.Hour), nil }); !errors.Is(err, cause) {
			t.Fatal(err)
		}
	})
}

func TestEveryExplicitRequestAndJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		requests := make(chan struct{}, 1)
		calls := make(chan struct{}, 2)
		done := make(chan struct{})
		go func() {
			defer close(done)
			Every(ctx, time.Second, requests, func(context.Context) bool { calls <- struct{}{}; return true })
		}()
		requests <- struct{}{}
		<-calls
		time.Sleep(time.Second / 3)
		<-calls
		cancel()
		<-done
	})
}
