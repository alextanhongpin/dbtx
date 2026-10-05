package repository

import (
	"testing"
	"time"
)

func TestTTLParam(t *testing.T) {
	tests := []struct {
		ttl   time.Duration
		us    int64
		valid bool
	}{
		{0, 0, false},
		{500 * time.Nanosecond, 1, true},
		{1500 * time.Nanosecond, 2, true},
		{time.Second, 1_000_000, true},
	}
	for _, tc := range tests {
		got := ttlParam(tc.ttl)
		if got.Int64 != tc.us || got.Valid != tc.valid {
			t.Errorf("ttlParam(%s) = %d µs (valid %t), want %d µs (valid %t)", tc.ttl, got.Int64, got.Valid, tc.us, tc.valid)
		}
	}
}
