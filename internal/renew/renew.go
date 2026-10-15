// Package renew supplies scheduling mechanics for renewable ownership.
// Callers retain ownership validation, error policy, and cleanup semantics.
package renew

import (
	"context"
	"time"
)

// Deadline conservatively measures validity from request dispatch, leaving a
// sixth of the TTL for cancellation to propagate.
func Deadline(sent time.Time, ttl time.Duration) time.Time { return sent.Add(ttl - ttl/6) }

// Every invokes step on each tick or explicit request, until cancellation or
// step returns false. Requests may be nil. The caller owns and joins the loop.
func Every(ctx context.Context, ttl time.Duration, requests <-chan struct{}, step func(context.Context) bool) {
	ticker := time.NewTicker(ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-requests:
		}
		if ctx.Err() != nil || !step(ctx) {
			return
		}
	}
}

// Watch independently enforces a caller-owned deadline even while renewal is
// blocked. check must atomically expire ownership before returning its deadline;
// a wake or late response must never resurrect expired ownership. changed may
// be nil. The caller must cancel and join Watch when its work ends.
func Watch(ctx context.Context, changed <-chan struct{}, check func() (time.Time, error)) error {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		deadline, err := check()
		if err != nil {
			return err
		}
		timer.Reset(time.Until(deadline))
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-changed:
		case <-timer.C:
		}
	}
}
