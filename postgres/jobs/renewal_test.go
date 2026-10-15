package jobs

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"
)

type delayedHeartbeat struct {
	Repository
	entered chan struct{}
	release chan struct{}
}

func (r *delayedHeartbeat) Heartbeat(context.Context, HeartbeatParams) (Job, error) {
	close(r.entered)
	<-r.release // Deliberately ignore cancellation, as a delayed driver might.
	return Job{}, nil
}

func TestLateHeartbeatCannotReviveLocalOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &delayedHeartbeat{entered: make(chan struct{}), release: make(chan struct{})}
		w := &Worker{repo: repo, cfg: WorkerConfig{LeaseDuration: time.Second, HeartbeatTimeout: time.Second}}
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			w.heartbeatLoop(ctx, cancel, Job{}, time.Now().Add(time.Second), slog.Default())
		}()
		<-repo.entered
		time.Sleep(time.Second)
		synctest.Wait()
		if !errors.Is(context.Cause(ctx), errLeaseExpiredLocally) {
			t.Fatalf("cause before response: %v", context.Cause(ctx))
		}
		close(repo.release)
		<-joined
		if !errors.Is(context.Cause(ctx), errLeaseExpiredLocally) {
			t.Fatalf("late response changed cause: %v", context.Cause(ctx))
		}
	})
}
