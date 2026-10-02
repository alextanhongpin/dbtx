package idempotent

import (
	_ "embed"

	"cmp"
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/idempotent/internal"
)

//go:embed internal/schema.sql
var Schema string

const DefaultLease = 30 * time.Second
const DefaultMaxAttempts = 10

// maxClaimRetries bounds how often Do re-claims a key whose state changed
// between Claim and Inspect.
const maxClaimRetries = 3

var (
	ErrClaimed         = internal.ErrClaimed
	ErrNotFound        = internal.ErrNotFound
	ErrRequestInFlight = internal.ErrRequestInFlight
	ErrRequestMismatch = internal.ErrRequestMismatch
	ErrMaxAttempts     = errors.New("max attempts reached")
	ErrInvalidResult   = errors.New("result must set exactly one of Checkpoint or Response")
)

const (
	StatusInProgress = internal.StatusInProgress
	StatusRetryable  = internal.StatusRetryable
	StatusCompleted  = internal.StatusCompleted
	StatusFailed     = internal.StatusFailed
)

var NewRepository = internal.NewRepository

type (
	AckParams          = internal.AckParams
	CheckpointParams   = internal.CheckpointParams
	ClaimParams        = internal.ClaimParams
	ClaimResponse      = internal.ClaimResponse
	FailParams         = internal.FailParams
	InspectParams      = internal.InspectParams
	InspectResponse    = internal.InspectResponse
	LockParams         = internal.LockParams
	NackParams         = internal.NackParams
	PostgresRepository = internal.Repository
	Status             = internal.Status
)

// Repository stores idempotency keys.
type Repository interface {
	Ack(ctx context.Context, params AckParams) error
	Checkpoint(ctx context.Context, params CheckpointParams) error
	Claim(ctx context.Context, params ClaimParams) (*ClaimResponse, error)
	Fail(ctx context.Context, params FailParams) error
	Inspect(ctx context.Context, params InspectParams) (*InspectResponse, error)
	Lock(ctx context.Context, params LockParams) error
	Nack(ctx context.Context, params NackParams) error
	Purge(ctx context.Context) (int64, error)
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error
}

type Idempotent struct {
	repo        Repository
	Lease       time.Duration
	MaxAttempts int
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
func (i *Idempotent) Do(ctx context.Context, key string, fn fun, req Request) (*Response, error) {
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
	maxAttempts := cmp.Or(i.MaxAttempts, DefaultMaxAttempts)
	for range maxClaimRetries {
		claim, err := i.repo.Claim(ctx, ClaimParams{
			IdempotencyKey: key,
			LeaseOwner:     uuid.NewV7().String(),
			LeaseSeconds:   i.lease().Seconds(),
			MaxAttempts:    int32(maxAttempts),
			Request:        req.Data,
		})
		if err == nil {
			return claim, nil, nil
		}
		if !errors.Is(err, ErrClaimed) {
			return nil, nil, err
		}

		inspect, err := i.repo.Inspect(ctx, InspectParams{
			Request:        req.Data,
			IdempotencyKey: key,
		})
		if errors.Is(err, ErrNotFound) {
			// Purged after Claim; try again.
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if inspect.PayloadMismatch {
			return nil, nil, ErrRequestMismatch
		}

		exhausted := int(inspect.Attempts) >= maxAttempts
		switch inspect.Status {
		case StatusCompleted, StatusFailed:
			return nil, &Response{
				Status: string(inspect.Status),
				Data:   inspect.Response,
				Error:  inspect.Error.String,
			}, nil
		case StatusInProgress:
			if !inspect.LeaseExpired {
				return nil, nil, ErrRequestInFlight
			}
			if exhausted {
				return nil, nil, ErrMaxAttempts
			}
			// Lease expired after Claim; try again.
		case StatusRetryable:
			if exhausted {
				return nil, nil, fmt.Errorf("%w: %s", ErrMaxAttempts, inspect.Error.String)
			}
			// Released after Claim; try again.
		default:
			return nil, nil, fmt.Errorf("unknown status: %s", inspect.Status)
		}
	}

	return nil, nil, ErrRequestInFlight
}

func (i *Idempotent) lease() time.Duration {
	return cmp.Or(i.Lease, DefaultLease)
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
	leaseSeconds := i.lease().Seconds()

	var res *Result
	err := i.repo.RunInTx(ctx, func(ctx context.Context) error {
		err := i.repo.Lock(ctx, LockParams{
			IdempotencyKey: key,
			FencingToken:   fencingToken,
			LeaseSeconds:   leaseSeconds,
		})
		if err != nil {
			return err
		}

		res, err = fn(ctx, params)
		if err != nil {
			return err
		}
		if res == nil || (res.Checkpoint == nil) == (res.Response == nil) {
			return ErrInvalidResult
		}

		if res.Checkpoint != nil {
			return i.repo.Checkpoint(ctx, CheckpointParams{
				IdempotencyKey: key,
				FencingToken:   fencingToken,
				Checkpoint:     res.Checkpoint.Name,
				CheckpointData: res.Checkpoint.Data,
				LeaseSeconds:   leaseSeconds,
			})
		}

		switch Status(res.Response.Status) {
		case StatusCompleted:
			return i.repo.Ack(ctx, AckParams{
				IdempotencyKey: key,
				FencingToken:   fencingToken,
				Response:       res.Response.Data,
			})
		case StatusFailed:
			return i.repo.Fail(ctx, FailParams{
				IdempotencyKey: key,
				FencingToken:   fencingToken,
				Response:       res.Response.Data,
				Error:          res.Response.Error,
			})
		default:
			return fmt.Errorf("unknown response status: %s", res.Response.Status)
		}
	})
	if err == nil {
		return res, nil
	}

	// Release so that other processes can retry.
	nackErr := i.repo.Nack(ctx, NackParams{
		IdempotencyKey: key,
		FencingToken:   fencingToken,
		Error:          err.Error(),
	})
	if errors.Is(nackErr, ErrClaimed) {
		// Fenced out; the key belongs to another process.
		return nil, err
	}
	return nil, errors.Join(err, nackErr)
}
