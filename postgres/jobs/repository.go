package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"uuid"
)

// Status mirrors the dbtx.job_status enum.
type Status string

const (
	StatusQueued  Status = "queued"  // queued, never claimed
	StatusRunning Status = "running" // leased by a worker
	StatusError   Status = "error"   // failed attempt, waiting for backoff
	StatusSuccess Status = "success" // terminal
	StatusFailed  Status = "failed"  // terminal
)

// Terminal reports whether no further processing will happen.
func (s Status) Terminal() bool { return s == StatusSuccess || s == StatusFailed }

// ErrNoRows is what the repository MUST return when a guarded UPDATE
// (Heartbeat / Complete / Fail / Retry) matches zero rows, i.e. the lease was
// lost. The sqlc adapter should translate pgx.ErrNoRows (or sql.ErrNoRows)
// into this sentinel so the use case does not depend on a driver.
var ErrNoRows = errors.New("jobs: no rows matched")

// Job is a row of dbtx.jobs.
type Job struct {
	ID             uuid.UUID
	IdempotencyKey string
	Request        json.RawMessage
	Response       json.RawMessage
	Status         Status
	WorkerID       *string
	FencingToken   int64
	Attempts       int32
	MaxAttempts    int32
	Error          *string
	VisibleAt      time.Time
	ProcessedAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// --- query parameters / results (1:1 with the sqlc queries) ------------------

type CreateParams struct {
	IdempotencyKey string
	Request        json.RawMessage
	DelaySeconds   float64
	MaxAttempts    int32
}

// CreateResult is the row returned by dbtx.create_job().
type CreateResult struct {
	ID             uuid.UUID
	Status         Status
	Response       json.RawMessage
	Error          *string
	PayloadMatches bool // false => same key, different request payload
	Created        bool // true => this call inserted the job
}

type FindAvailableJobsParams struct {
	WorkerID     string
	LeaseSeconds float64
	Limit        int32
}

type HeartbeatParams struct {
	ID           uuid.UUID
	FencingToken int64
	WorkerID     string
	LeaseSeconds float64
}

type CompleteParams struct {
	ID           uuid.UUID
	FencingToken int64
	WorkerID     string
	Response     json.RawMessage
}

type FailParams struct {
	ID           uuid.UUID
	FencingToken int64
	WorkerID     string
	Error        string
}

type RetryParams struct {
	ID             uuid.UUID
	FencingToken   int64
	WorkerID       string
	Error          string
	BackoffSeconds float64
}

type ArchiveOldJobsParams struct {
	RetentionSeconds float64
	BatchSize        int32
}

type PurgeOldJobKeysParams struct {
	KeyTTLSeconds float64
	BatchSize     int32
}

// Repository is the persistence port the use cases depend on. The sqlc-backed
// implementation lives elsewhere; each method maps to one named query.
//
// Contract: Heartbeat, Complete, Fail and Retry return ErrNoRows when the
// fencing guard matches nothing.
type Repository interface {
	Create(ctx context.Context, p CreateParams) (CreateResult, error)
	FindAvailableJobs(ctx context.Context, p FindAvailableJobsParams) ([]Job, error)
	Heartbeat(ctx context.Context, p HeartbeatParams) (Job, error)
	Complete(ctx context.Context, p CompleteParams) (Job, error)
	Fail(ctx context.Context, p FailParams) (Job, error)
	Retry(ctx context.Context, p RetryParams) (Job, error)
	ReapExhausted(ctx context.Context) (int64, error)
	ArchiveOldJobs(ctx context.Context, p ArchiveOldJobsParams) (int64, error)
	PurgeOldJobKeys(ctx context.Context, p PurgeOldJobKeysParams) (int64, error)

	// IsTx reports whether ctx carries a transaction that the other methods
	// would join.
	IsTx(ctx context.Context) bool
}
