package durable_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/durable"
)

func TestRetryClassification(t *testing.T) {
	base := errors.New("boom")
	if durable.Permanent(nil) != nil || durable.RetryAfter(nil, time.Second) != nil {
		t.Fatal("nil errors must stay nil")
	}
	p := fmt.Errorf("wrapped: %w", durable.Permanent(base))
	if !durable.IsPermanent(p) || !errors.Is(p, base) {
		t.Fatal("Permanent must survive wrapping and unwrap to the cause")
	}
	r := durable.RetryAfter(base, 3*time.Second)
	if d := durable.NextDelay(r, 1, nil); d != 3*time.Second {
		t.Fatalf("RetryAfter delay = %v", d)
	}
	if durable.IsPermanent(r) {
		t.Fatal("RetryAfter is retryable")
	}
}

func TestExponentialBackoff(t *testing.T) {
	b := durable.ExponentialBackoff(time.Second, 10*time.Second)
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 10: 10 * time.Second} {
		got := b(attempt)
		if got < want || got > want+want/4 {
			t.Errorf("attempt %d: %v not in [%v, %v]", attempt, got, want, want+want/4)
		}
	}
}
