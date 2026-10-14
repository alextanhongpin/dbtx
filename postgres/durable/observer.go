package durable

import (
	"context"
	"log/slog"
	"time"
)

// EventType names something an operator may want to see.
type EventType string

const (
	EventLeaseLost      EventType = "lease_lost"
	EventRetryScheduled EventType = "retry_scheduled"
	EventFailed         EventType = "failed"
	EventCancelled      EventType = "cancelled"
	EventSucceeded      EventType = "succeeded"
	EventStreamBlocked  EventType = "stream_blocked"
	EventStreamParked   EventType = "stream_parked"
	EventInboxGap       EventType = "inbox_gap"
	EventHandedBack     EventType = "handed_back"
)

// Event describes one observation. Fields that do not apply are zero.
type Event struct {
	Type      EventType
	Component string // "lease", "jobs", "outbox", "inbox", "keys", "schedule", "cache"
	Kind      string // job kind or topic
	Key       string // lease key, execution ID, stream key, message ID
	Token     int64
	Attempt   int
	Seq       int64
	RetryAt   time.Time
	Err       error
}

// Observer receives events synchronously; keep it fast and non-blocking.
type Observer interface {
	Observe(ctx context.Context, e Event)
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(ctx context.Context, e Event)

func (f ObserverFunc) Observe(ctx context.Context, e Event) { f(ctx, e) }

// NopObserver discards events.
type NopObserver struct{}

func (NopObserver) Observe(context.Context, Event) {}

// SlogObserver logs events. Failures and losses log at warn level.
type SlogObserver struct{ Logger *slog.Logger }

func (o SlogObserver) Observe(ctx context.Context, e Event) {
	l := o.Logger
	if l == nil {
		l = slog.Default()
	}
	level := slog.LevelInfo
	switch e.Type {
	case EventLeaseLost, EventFailed, EventStreamBlocked, EventStreamParked, EventInboxGap:
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{slog.String("component", e.Component)}
	if e.Kind != "" {
		attrs = append(attrs, slog.String("kind", e.Kind))
	}
	if e.Key != "" {
		attrs = append(attrs, slog.String("key", e.Key))
	}
	if e.Token != 0 {
		attrs = append(attrs, slog.Int64("token", e.Token))
	}
	if e.Attempt != 0 {
		attrs = append(attrs, slog.Int("attempt", e.Attempt))
	}
	if e.Seq != 0 {
		attrs = append(attrs, slog.Int64("seq", e.Seq))
	}
	if !e.RetryAt.IsZero() {
		attrs = append(attrs, slog.Time("retry_at", e.RetryAt))
	}
	if e.Err != nil {
		attrs = append(attrs, slog.String("error", e.Err.Error()))
	}
	l.LogAttrs(ctx, level, "durable: "+string(e.Type), attrs...)
}

// Observe sends e to o, treating a nil Observer as NopObserver.
func Observe(ctx context.Context, o Observer, e Event) {
	if o != nil {
		o.Observe(ctx, e)
	}
}
