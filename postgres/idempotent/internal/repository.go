package internal

import (
	"context"
	"database/sql"
	"errors"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/idempotent/internal/postgres"
)

var (
	ErrClaimed         = errors.New("lease expired or claimed by another process")
	ErrNotFound        = errors.New("idempotency key not found")
	ErrRequestInFlight = errors.New("request in flight")
	ErrRequestMismatch = errors.New("request mismatch")
)

type Status = postgres.DbtxIdempotencyKeyStatus

const (
	StatusInProgress = postgres.DbtxIdempotencyKeyStatusInProgress
	StatusRetryable  = postgres.DbtxIdempotencyKeyStatusRetryable
	StatusCompleted  = postgres.DbtxIdempotencyKeyStatusCompleted
	StatusFailed     = postgres.DbtxIdempotencyKeyStatusFailed
)

type Repository struct {
	*dbtx.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{
		dbtx.New(db),
	}
}

type AckParams = postgres.AckParams

func (r *Repository) Ack(ctx context.Context, params AckParams) error {
	_, err := r.db(ctx).Ack(ctx, params)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClaimed
	}
	return err
}

type CheckpointParams = postgres.CheckpointParams

func (r *Repository) Checkpoint(ctx context.Context, params CheckpointParams) error {
	_, err := r.db(ctx).Checkpoint(ctx, params)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClaimed
	}
	return err
}

type ClaimParams = postgres.ClaimParams
type ClaimResponse = postgres.ClaimRow

func (r *Repository) Claim(ctx context.Context, params ClaimParams) (*ClaimResponse, error) {
	res, err := r.db(ctx).Claim(ctx, params)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClaimed
	}
	return res, err
}

type FailParams = postgres.FailParams

func (r *Repository) Fail(ctx context.Context, params FailParams) error {
	_, err := r.db(ctx).Fail(ctx, params)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClaimed
	}
	return err
}

type LockParams = postgres.LockParams

func (r *Repository) Lock(ctx context.Context, params LockParams) error {
	_, err := r.db(ctx).Lock(ctx, params)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClaimed
	}
	return err
}

type NackParams = postgres.NackParams

func (r *Repository) Nack(ctx context.Context, params NackParams) error {
	_, err := r.db(ctx).Nack(ctx, params)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClaimed
	}
	return err
}

func (r *Repository) Purge(ctx context.Context) (int64, error) {
	return r.db(ctx).Purge(ctx)
}

func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}
