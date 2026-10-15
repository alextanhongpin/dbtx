package repository

import (
	_ "embed"

	"context"
	"database/sql"
	"errors"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/idempotent"
	"github.com/alextanhongpin/dbtx/postgres/idempotent/repository/postgres"
)

//go:embed schema.sql
var Schema string

var _ idempotent.Repository = (*Repository)(nil)

type Repository struct {
	*dbtx.DB
}

func New(db *sql.DB) *Repository {
	return &Repository{
		dbtx.New(db),
	}
}

func (r *Repository) Ack(ctx context.Context, params idempotent.AckParams) error {
	_, err := r.db(ctx).Ack(ctx, postgres.AckParams{
		IdempotencyKey: params.IdempotencyKey,
		FencingToken:   params.FencingToken,
		Response:       jsonOrNull(params.Response),
		TtlSeconds:     params.TtlSeconds,
	})
	return claimed(err)
}

func (r *Repository) Checkpoint(ctx context.Context, params idempotent.CheckpointParams) error {
	_, err := r.db(ctx).Checkpoint(ctx, postgres.CheckpointParams{
		IdempotencyKey: params.IdempotencyKey,
		FencingToken:   params.FencingToken,
		Checkpoint:     params.Checkpoint,
		CheckpointData: jsonOrNull(params.CheckpointData),
		LeaseSeconds:   params.LeaseSeconds,
	})
	return claimed(err)
}

func (r *Repository) Claim(ctx context.Context, params idempotent.ClaimParams) (*idempotent.ClaimResponse, error) {
	res, err := r.db(ctx).Claim(ctx, postgres.ClaimParams{
		IdempotencyKey: params.IdempotencyKey,
		Request:        jsonOrNull(params.Request),
		LeaseOwner:     params.LeaseOwner,
		LeaseSeconds:   params.LeaseSeconds,
		MaxAttempts:    params.MaxAttempts,
		TtlSeconds:     params.TtlSeconds,
	})
	if err != nil {
		return nil, claimed(err)
	}
	return &idempotent.ClaimResponse{
		Outcome:        res.Outcome,
		FencingToken:   res.FencingToken,
		Attempts:       res.Attempts,
		Checkpoint:     res.Checkpoint,
		CheckpointData: nullToNil(res.CheckpointData),
		Response:       nullToNil(res.Response),
		Error:          res.Error,
		LeaseExpiresAt: res.LeaseExpiresAt,
		RetryAfter:     res.RetryAfter,
	}, nil
}

func (r *Repository) Fail(ctx context.Context, params idempotent.FailParams) error {
	_, err := r.db(ctx).Fail(ctx, postgres.FailParams{
		IdempotencyKey: params.IdempotencyKey,
		FencingToken:   params.FencingToken,
		Response:       jsonOrNull(params.Response),
		Error:          params.Error,
		TtlSeconds:     params.TtlSeconds,
	})
	return claimed(err)
}

func (r *Repository) Lock(ctx context.Context, params idempotent.LockParams) error {
	_, err := r.db(ctx).Lock(ctx, postgres.LockParams{
		IdempotencyKey: params.IdempotencyKey,
		FencingToken:   params.FencingToken,
		LeaseSeconds:   params.LeaseSeconds,
	})
	return claimed(err)
}

func (r *Repository) Nack(ctx context.Context, params idempotent.NackParams) error {
	_, err := r.db(ctx).Nack(ctx, postgres.NackParams{
		IdempotencyKey:     params.IdempotencyKey,
		FencingToken:       params.FencingToken,
		Error:              params.Error,
		MaxAttempts:        params.MaxAttempts,
		BaseBackoffSeconds: params.BaseBackoffSeconds,
		MaxBackoffSeconds:  params.MaxBackoffSeconds,
		TtlSeconds:         params.TtlSeconds,
	})
	return claimed(err)
}

func (r *Repository) Purge(ctx context.Context) (int64, error) {
	return r.db(ctx).Purge(ctx)
}

func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}

// jsonOrNull maps empty data to JSON null. Otherwise a nil []byte is sent as
// SQL NULL, and an empty one as an empty string, which the not null jsonb
// columns reject.
func jsonOrNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}

// nullToNil reverses jsonOrNull, so that data saved empty is read back as
// nil.
func nullToNil(b []byte) []byte {
	if string(b) == "null" {
		return nil
	}
	return b
}

// claimed maps a missing row to ErrClaimed: the update matched no row because
// the lease was lost or the fencing token is stale.
func claimed(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return idempotent.ErrClaimed
	}
	return err
}
