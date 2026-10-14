package durable

import "context"

// Hooks are fault-injection points for tests. Each hook runs synchronously at
// the named moment; a hook that blocks simulates a stop-the-world pause there.
// Leave them nil in production.
type Hooks struct {
	// BeforeComplete runs after a handler returned successfully and before the
	// completion is written. key identifies the execution or stream.
	BeforeComplete func(ctx context.Context, key string)
	// BeforeHeartbeat runs before each automatic lease renewal.
	BeforeHeartbeat func(ctx context.Context, key string)
	// BetweenSteps runs after a checkpoint or remote step was saved.
	BetweenSteps func(ctx context.Context, key, step string)
	// AfterRemoteCall runs after a remote step returned, before its response
	// is saved.
	AfterRemoteCall func(ctx context.Context, key, step string)
	// AfterPublishBeforeAdvance runs after the publisher accepted messages and
	// before the outbox records them as published.
	AfterPublishBeforeAdvance func(ctx context.Context, key string, seq int64)
}

// Call helpers tolerate nil hooks.

func (h *Hooks) CallBeforeComplete(ctx context.Context, key string) {
	if h != nil && h.BeforeComplete != nil {
		h.BeforeComplete(ctx, key)
	}
}

func (h *Hooks) CallBeforeHeartbeat(ctx context.Context, key string) {
	if h != nil && h.BeforeHeartbeat != nil {
		h.BeforeHeartbeat(ctx, key)
	}
}

func (h *Hooks) CallBetweenSteps(ctx context.Context, key, step string) {
	if h != nil && h.BetweenSteps != nil {
		h.BetweenSteps(ctx, key, step)
	}
}

func (h *Hooks) CallAfterRemoteCall(ctx context.Context, key, step string) {
	if h != nil && h.AfterRemoteCall != nil {
		h.AfterRemoteCall(ctx, key, step)
	}
}

func (h *Hooks) CallAfterPublishBeforeAdvance(ctx context.Context, key string, seq int64) {
	if h != nil && h.AfterPublishBeforeAdvance != nil {
		h.AfterPublishBeforeAdvance(ctx, key, seq)
	}
}
