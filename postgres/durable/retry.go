package durable

import (
	"errors"
	"math/rand/v2"
	"time"
)

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent marks err as not worth retrying. A nil err stays nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

// IsPermanent reports whether err, or an error it wraps, was marked Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

type retryAfterError struct {
	err   error
	delay time.Duration
}

func (e *retryAfterError) Error() string { return e.err.Error() }
func (e *retryAfterError) Unwrap() error { return e.err }

// RetryAfter asks for the next attempt no sooner than d, for example to honour
// a remote Retry-After header. A nil err stays nil.
func RetryAfter(err error, d time.Duration) error {
	if err == nil {
		return nil
	}
	return &retryAfterError{err, max(d, 0)}
}

// RetryDelay returns the delay requested with RetryAfter, if any.
func RetryDelay(err error) (time.Duration, bool) {
	var r *retryAfterError
	if errors.As(err, &r) {
		return r.delay, true
	}
	return 0, false
}

// Backoff maps the number of attempts made so far (>= 1) to a retry delay.
type Backoff func(attempt int) time.Duration

// ExponentialBackoff doubles from base up to max and adds up to 25% jitter.
func ExponentialBackoff(base, maxDelay time.Duration) Backoff {
	return func(attempt int) time.Duration {
		d := base
		for i := 1; i < attempt && d < maxDelay; i++ {
			d *= 2
		}
		d = min(d, maxDelay)
		if d <= 0 {
			return 0
		}
		return d + time.Duration(rand.Int64N(int64(d)/4+1))
	}
}

// DefaultBackoff is ExponentialBackoff(time.Second, time.Hour).
var DefaultBackoff = ExponentialBackoff(time.Second, time.Hour)

// NextDelay picks the delay for a failed attempt: an explicit RetryAfter wins,
// otherwise backoff (DefaultBackoff when nil).
func NextDelay(err error, attempt int, backoff Backoff) time.Duration {
	if d, ok := RetryDelay(err); ok {
		return d
	}
	if backoff == nil {
		backoff = DefaultBackoff
	}
	return backoff(attempt)
}
