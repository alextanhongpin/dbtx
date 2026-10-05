package idempotent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"time"
	"uuid"
)

// nackTimeout bounds recording a failure after ctx is done.
const nackTimeout = 5 * time.Second

const (
	DefaultLease       = 30 * time.Second
	DefaultMaxAttempts = 10
	DefaultTTL         = 24 * time.Hour
)

var (
	ErrBackoff         = errors.New("retry backoff")
	ErrClaimed         = errors.New("lease expired or claimed by another process")
	ErrInvalidConfig   = errors.New("idempotent: invalid config")
	ErrInvalidResult   = errors.New("result must set exactly one of Checkpoint or Response")
	ErrMaxAttempts     = errors.New("max attempts reached")
	ErrNotFound        = errors.New("idempotency key not found")
	ErrRequestInFlight = errors.New("request in flight")
	ErrRequestMismatch = errors.New("request mismatch")
	ErrTxInContext     = errors.New("idempotent: ctx must not carry a transaction")
)

type Status string

const (
	StatusInProgress Status = "in_progress"
	StatusRetryable  Status = "retryable"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// Outcomes returned by Claim.
const (
	OutcomeBackoff         = "backoff"
	OutcomeClaimed         = "claimed"
	OutcomeCompleted       = "completed"
	OutcomeExhausted       = "exhausted"
	OutcomeFailed          = "failed"
	OutcomeInProgress      = "in_progress"
	OutcomePayloadMismatch = "payload_mismatch"
	OutcomeResumed         = "resumed"
)

type AckParams struct {
	IdempotencyKey string
	FencingToken   int64
	Response       []byte
	TtlSeconds     float64
}

type CheckpointParams struct {
	IdempotencyKey string
	FencingToken   int64
	Checkpoint     string
	CheckpointData []byte
	LeaseSeconds   float64
}

type ClaimParams struct {
	IdempotencyKey string
	Request        []byte
	LeaseOwner     string
	LeaseSeconds   float64
	MaxAttempts    int32
	TtlSeconds     float64
}

// ClaimResponse fields that do not apply to the Outcome are zero.
type ClaimResponse struct {
	Outcome        string
	FencingToken   int64
	Attempts       int32
	Checkpoint     string
	CheckpointData []byte
	Response       []byte
	Error          string
	LeaseExpiresAt time.Time
	RetryAfter     time.Time
}

type FailParams struct {
	IdempotencyKey string
	FencingToken   int64
	Response       []byte
	Error          string
	TtlSeconds     float64
}

type LockParams struct {
	IdempotencyKey string
	FencingToken   int64
	LeaseSeconds   float64
}

type NackParams struct {
	IdempotencyKey     string
	FencingToken       int64
	Error              string
	MaxAttempts        int32
	BaseBackoffSeconds float64
	MaxBackoffSeconds  float64
	TtlSeconds         float64
}

// Repository stores idempotency keys.
type Repository interface {
	Ack(ctx context.Context, params AckParams) error
	Checkpoint(ctx context.Context, params CheckpointParams) error
	Claim(ctx context.Context, params ClaimParams) (*ClaimResponse, error)
	Fail(ctx context.Context, params FailParams) error
	IsTx(ctx context.Context) bool
	Lock(ctx context.Context, params LockParams) error
	Nack(ctx context.Context, params NackParams) error
	Purge(ctx context.Context) (int64, error)
	RunInSubTx(ctx context.Context, fn func(txCtx context.Context) error) error
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error
}

type Idempotent struct {
	repo  Repository
	Lease time.Duration
	// MaxAttempts is the number of attempts before the key fails. Zero means
	// DefaultMaxAttempts, and values outside int32 make Do fail with
	// ErrInvalidConfig.
	MaxAttempts int
	// TTL is how long a key is kept after its last write before Purge
	// removes it.
	TTL time.Duration
	// After a handler error, the key cannot be claimed again for
	// BaseBackoff * 2^(attempts-1), capped at MaxBackoff. Zero MaxBackoff
	// disables the backoff.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

func New(repo Repository) *Idempotent {
	return &Idempotent{
		repo: repo,
	}
}

type fun = func(context.Context, Params) (*Result, error)

// Do runs fn at most once to completion for the given key.
//
// Each call to fn runs in its own transaction that holds the row lock and
// extends the lease, so the key cannot be taken over while fn is running.
// fn returns either a Checkpoint, which is saved and passed to the next call,
// or a Response, which ends the run.
//
// Do returns ErrTxInContext if ctx carries a transaction of the repository:
// each step must commit on its own, so it cannot join the caller's.
func (i *Idempotent) Do(ctx context.Context, key string, fn fun, req Request) (*Response, error) {
	if i.repo.IsTx(ctx) {
		return nil, ErrTxInContext
	}
	// maxAttempts converts to int32 for the database.
	if i.MaxAttempts < 0 || i.MaxAttempts > math.MaxInt32 {
		return nil, fmt.Errorf("%w: MaxAttempts must be between 0 (default) and %d", ErrInvalidConfig, math.MaxInt32)
	}
	claim, cached, err := i.claim(ctx, key, req)
	if err != nil {
		return nil, err
	}
	if cached != nil {
		return cached, nil
	}

	params := Params{
		Request: &req,
		Checkpoint: &Checkpoint{
			Name: claim.Checkpoint,
			Data: claim.CheckpointData,
		},
	}
	for {
		res, err := i.atomic(ctx, key, claim.FencingToken, params, fn)
		if err != nil {
			return nil, err
		}
		if res.Response != nil {
			return res.Response, nil
		}
		params.Checkpoint = res.Checkpoint
	}
}

// claim acquires the lease for key. If the key is already finished, the cached
// response is returned instead.
func (i *Idempotent) claim(ctx context.Context, key string, req Request) (*ClaimResponse, *Response, error) {
	claim, err := i.repo.Claim(ctx, ClaimParams{
		IdempotencyKey: key,
		LeaseOwner:     uuid.NewV7().String(),
		LeaseSeconds:   i.lease().Seconds(),
		MaxAttempts:    i.maxAttempts(),
		TtlSeconds:     i.ttl().Seconds(),
		Request:        req.Data,
	})
	if err != nil {
		return nil, nil, err
	}
	switch claim.Outcome {
	case OutcomeClaimed, OutcomeResumed:
		return claim, nil, nil
	case OutcomeCompleted, OutcomeFailed:
		return nil, &Response{
			Status: claim.Outcome,
			Data:   claim.Response,
			Error:  claim.Error,
		}, nil
	case OutcomeInProgress:
		return nil, nil, ErrRequestInFlight
	case OutcomeBackoff:
		return nil, nil, fmt.Errorf("%w: retry after %s", ErrBackoff, claim.RetryAfter)
	case OutcomeExhausted:
		return nil, nil, fmt.Errorf("%w: %s", ErrMaxAttempts, claim.Error)
	case OutcomePayloadMismatch:
		return nil, nil, ErrRequestMismatch
	default:
		return nil, nil, fmt.Errorf("unhandled outcome: %s", claim.Outcome)
	}
}

func (i *Idempotent) lease() time.Duration {
	return cmp.Or(i.Lease, DefaultLease)
}

func (i *Idempotent) ttl() time.Duration {
	return cmp.Or(i.TTL, DefaultTTL)
}

func (i *Idempotent) maxAttempts() int32 {
	return int32(cmp.Or(i.MaxAttempts, DefaultMaxAttempts))
}

type Checkpoint struct {
	Name string
	Data []byte
}

type Params struct {
	Request    *Request
	Checkpoint *Checkpoint
}

type Request struct {
	Data []byte
}

type Response struct {
	Status string
	Data   []byte
	Error  string
}

// Result must set exactly one of Checkpoint or Response.
type Result struct {
	Checkpoint *Checkpoint
	Response   *Response
}

func (i *Idempotent) atomic(ctx context.Context, key string, fencingToken int64, params Params, fn fun) (*Result, error) {
	// A panic in fn rolls back the step and propagates. Record it first, so
	// that the key is retried after the backoff, instead of when the lease
	// expires without an error.
	defer func() {
		if p := recover(); p != nil {
			_ = i.nack(ctx, key, fencingToken, fmt.Errorf("panic: %v", p))
			panic(p)
		}
	}()

	var res *Result
	var stepErr error
	err := i.repo.RunInTx(ctx, func(ctx context.Context) error {
		err := i.repo.Lock(ctx, LockParams{
			IdempotencyKey: key,
			FencingToken:   fencingToken,
			LeaseSeconds:   i.lease().Seconds(),
		})
		if err != nil {
			return err
		}

		// Run the step in a savepoint, so that its writes roll back on error
		// while the row stays locked. Recording the failure here, before the
		// lock is released, makes a Claim that waits on the lock see the
		// backoff. After the transaction, the lease may have expired already,
		// and the Claim would take over first.
		stepErr = i.repo.RunInSubTx(ctx, func(ctx context.Context) error {
			var err error
			res, err = i.step(ctx, key, fencingToken, params, fn)
			return err
		})
		if stepErr == nil {
			return nil
		}
		return i.nack(ctx, key, fencingToken, stepErr)
	})
	switch {
	case err == nil && stepErr == nil:
		return res, nil
	case err == nil:
		return nil, stepErr
	}

	// The transaction failed, so release the key without it. The key fails
	// once the attempts are exhausted.
	if stepErr != nil {
		err = errors.Join(stepErr, err)
	}
	nackErr := i.nack(ctx, key, fencingToken, err)
	if errors.Is(nackErr, ErrClaimed) {
		// Fenced out; the key belongs to another process.
		return nil, err
	}
	return nil, errors.Join(err, nackErr)
}

// step runs fn and saves its result: a checkpoint, or the final response.
func (i *Idempotent) step(ctx context.Context, key string, fencingToken int64, params Params, fn fun) (*Result, error) {
	res, err := fn(ctx, params)
	if err != nil {
		return nil, err
	}
	if res == nil || (res.Checkpoint == nil) == (res.Response == nil) {
		return nil, ErrInvalidResult
	}

	if res.Checkpoint != nil {
		return res, i.repo.Checkpoint(ctx, CheckpointParams{
			IdempotencyKey: key,
			FencingToken:   fencingToken,
			Checkpoint:     res.Checkpoint.Name,
			CheckpointData: res.Checkpoint.Data,
			LeaseSeconds:   i.lease().Seconds(),
		})
	}

	switch Status(res.Response.Status) {
	case StatusCompleted:
		return res, i.repo.Ack(ctx, AckParams{
			IdempotencyKey: key,
			FencingToken:   fencingToken,
			Response:       res.Response.Data,
			TtlSeconds:     i.ttl().Seconds(),
		})
	case StatusFailed:
		return res, i.repo.Fail(ctx, FailParams{
			IdempotencyKey: key,
			FencingToken:   fencingToken,
			Response:       res.Response.Data,
			Error:          res.Response.Error,
			TtlSeconds:     i.ttl().Seconds(),
		})
	default:
		return nil, fmt.Errorf("unknown response status: %s", res.Response.Status)
	}
}

// nack releases the key so that other processes can retry it after the
// backoff. The key fails once the attempts are exhausted.
//
// Outside the step transaction, ctx is detached from its cancellation: a
// cancelled ctx is a common reason for the step to fail, and the failure must
// still be recorded.
func (i *Idempotent) nack(ctx context.Context, key string, fencingToken int64, cause error) error {
	if !i.repo.IsTx(ctx) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), nackTimeout)
		defer cancel()
	}
	return i.repo.Nack(ctx, NackParams{
		IdempotencyKey:     key,
		FencingToken:       fencingToken,
		Error:              cause.Error(),
		MaxAttempts:        i.maxAttempts(),
		BaseBackoffSeconds: i.BaseBackoff.Seconds(),
		MaxBackoffSeconds:  i.MaxBackoff.Seconds(),
		TtlSeconds:         i.ttl().Seconds(),
	})
}
