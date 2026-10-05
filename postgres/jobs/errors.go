package jobs

import (
	"errors"
	"time"
)

var (
	// ErrInvalidInput: the caller sent a bad request (map to HTTP 400/422).
	ErrInvalidInput = errors.New("jobs: invalid input")

	// ErrIdempotencyKeyReuse: the key exists but with a different payload
	// (map to HTTP 409/422). The original job is NOT modified.
	ErrIdempotencyKeyReuse = errors.New("jobs: idempotency key reused with a different request")

	// ErrLeaseLost: the server confirmed another worker owns the job now.
	ErrLeaseLost = errors.New("jobs: lease lost")

	// errLeaseExpiredLocally: heartbeats kept failing and our local lease clock
	// is about to run out. Ownership is uncertain, so handlers are told to
	// stop, but we still try to finalize: the fencing token makes that safe.
	errLeaseExpiredLocally = errors.New("jobs: lease expired locally (heartbeats failing)")

	// ErrTxInContext: Worker.Run and Janitor.Run commit each statement on its
	// own, so their ctx must not carry a transaction of the repository.
	ErrTxInContext = errors.New("jobs: ctx must not carry a transaction")

	// ErrInvalidResponse: the handler's response cannot be stored as jsonb.
	// The job is failed instead of retried.
	ErrInvalidResponse = errors.New("jobs: invalid response")

	// ErrInvalidData is what the repository returns when the database rejects
	// a value (SQLSTATE class 22, data exception). Retrying cannot fix it.
	ErrInvalidData = errors.New("jobs: invalid data")

	// ErrShutdown is the cancel cause when the shutdown grace period elapses.
	ErrShutdown = errors.New("jobs: worker shutting down")
)

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks a handler error as non-retryable: the job goes straight to
// 'failed'. Use it for validation errors, 4xx from downstream, etc.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err (or anything it wraps) was marked Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

type retryAfterError struct {
	err   error
	delay time.Duration
}

func (e retryAfterError) Error() string { return e.err.Error() }
func (e retryAfterError) Unwrap() error { return e.err }

// RetryAfter overrides the default backoff, e.g. to honour a Retry-After header.
func RetryAfter(err error, d time.Duration) error {
	if err == nil {
		return nil
	}
	return retryAfterError{err: err, delay: d}
}

func retryDelay(err error) (time.Duration, bool) {
	if r, ok := errors.AsType[retryAfterError](err); ok {
		return r.delay, true
	}
	return 0, false
}
