package repository

import (
	_ "embed"

	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/jobs"
	"github.com/alextanhongpin/dbtx/postgres/jobs/repository/postgres"
)

//go:embed schema.sql
var Schema string

var _ jobs.Repository = (*Repository)(nil)

type Repository struct {
	*dbtx.DB
}

func New(db *sql.DB) *Repository {
	return &Repository{
		DB: dbtx.New(db),
	}
}

func (r *Repository) Create(ctx context.Context, params jobs.CreateParams) (jobs.CreateResult, error) {
	res, err := r.db(ctx).Create(ctx, postgres.CreateParams{
		IdempotencyKey: params.IdempotencyKey,
		Request:        params.Request,
		DelaySeconds:   params.DelaySeconds,
		MaxAttempts:    params.MaxAttempts,
	})
	if err != nil {
		return jobs.CreateResult{}, dataError(err)
	}
	var errText *string
	if res.Error != "" {
		errText = &res.Error
	}
	return jobs.CreateResult{
		ID:             res.ID,
		Status:         jobs.Status(res.Status),
		Response:       json.RawMessage(res.Response),
		Error:          errText,
		PayloadMatches: res.PayloadMatches,
		Created:        res.Created,
	}, nil
}

func (r *Repository) FindAvailableJobs(ctx context.Context, params jobs.FindAvailableJobsParams) ([]jobs.Job, error) {
	rows, err := r.db(ctx).FindAvailableJobs(ctx, postgres.FindAvailableJobsParams{
		WorkerID:     params.WorkerID,
		LeaseSeconds: params.LeaseSeconds,
		Limit:        params.Limit,
	})
	if err != nil {
		return nil, err
	}
	res := make([]jobs.Job, len(rows))
	for i, row := range rows {
		res[i] = newJob(row)
	}
	return res, nil
}

// Heartbeat returns jobs.ErrNoRows if the lease is lost.
func (r *Repository) Heartbeat(ctx context.Context, params jobs.HeartbeatParams) (jobs.Job, error) {
	return guarded(r.db(ctx).Heartbeat(ctx, postgres.HeartbeatParams{
		ID:           params.ID,
		FencingToken: params.FencingToken,
		WorkerID:     params.WorkerID,
		LeaseSeconds: params.LeaseSeconds,
	}))
}

// Complete returns jobs.ErrNoRows if the lease is lost.
func (r *Repository) Complete(ctx context.Context, params jobs.CompleteParams) (jobs.Job, error) {
	return guarded(r.db(ctx).Complete(ctx, postgres.CompleteParams{
		ID:           params.ID,
		FencingToken: params.FencingToken,
		WorkerID:     params.WorkerID,
		Response:     params.Response,
	}))
}

// Fail returns jobs.ErrNoRows if the lease is lost.
func (r *Repository) Fail(ctx context.Context, params jobs.FailParams) (jobs.Job, error) {
	return guarded(r.db(ctx).Fail(ctx, postgres.FailParams{
		ID:           params.ID,
		FencingToken: params.FencingToken,
		WorkerID:     params.WorkerID,
		Error:        params.Error,
	}))
}

// Retry returns jobs.ErrNoRows if the lease is lost.
func (r *Repository) Retry(ctx context.Context, params jobs.RetryParams) (jobs.Job, error) {
	return guarded(r.db(ctx).Retry(ctx, postgres.RetryParams{
		ID:             params.ID,
		FencingToken:   params.FencingToken,
		WorkerID:       params.WorkerID,
		Error:          params.Error,
		BackoffSeconds: params.BackoffSeconds,
	}))
}

func (r *Repository) ReapExhausted(ctx context.Context, params jobs.ReapExhaustedParams) (int64, error) {
	return r.db(ctx).ReapExhausted(ctx, params.GraceSeconds)
}

func (r *Repository) ArchiveOldJobs(ctx context.Context, params jobs.ArchiveOldJobsParams) (int64, error) {
	return r.db(ctx).ArchiveOldJobs(ctx, postgres.ArchiveOldJobsParams{
		RetentionSeconds: params.RetentionSeconds,
		BatchSize:        params.BatchSize,
	})
}

func (r *Repository) PurgeOldJobKeys(ctx context.Context, params jobs.PurgeOldJobKeysParams) (int64, error) {
	return r.db(ctx).PurgeOldJobKeys(ctx, postgres.PurgeOldJobKeysParams{
		KeyTtlSeconds: params.KeyTTLSeconds,
		BatchSize:     params.BatchSize,
	})
}

func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}

// guarded maps a fenced update that matched no row to jobs.ErrNoRows.
func guarded(row *postgres.DbtxJob, err error) (jobs.Job, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNoRows
	}
	if err != nil {
		return jobs.Job{}, dataError(err)
	}
	return newJob(row), nil
}

// dataError marks errors where the database rejected a value (SQLSTATE class
// 22, data exception) with jobs.ErrInvalidData, since retrying cannot fix
// them. SQLState is implemented by both lib/pq and pgx errors.
func dataError(err error) error {
	var state interface{ SQLState() string }
	if errors.As(err, &state) && strings.HasPrefix(state.SQLState(), "22") {
		return fmt.Errorf("%w: %w", jobs.ErrInvalidData, err)
	}
	return err
}

func newJob(row *postgres.DbtxJob) jobs.Job {
	return jobs.Job{
		ID:             row.ID,
		IdempotencyKey: row.IdempotencyKey,
		Request:        json.RawMessage(row.Request),
		Response:       json.RawMessage(row.Response),
		Status:         jobs.Status(row.Status),
		WorkerID:       nullString(row.WorkerID),
		FencingToken:   row.FencingToken,
		Attempts:       row.Attempts,
		MaxAttempts:    row.MaxAttempts,
		Error:          nullString(row.Error),
		VisibleAt:      row.VisibleAt,
		ProcessedAt:    nullTime(row.ProcessedAt),
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func nullString(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}
