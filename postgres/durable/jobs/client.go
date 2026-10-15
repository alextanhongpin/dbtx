// Package jobs runs durable executions: submitted once per idempotency key,
// claimed by workers with fenced leases, retried with backoff, resumable from
// checkpoints, and able to call remote APIs through idempotent steps.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// Mode selects how a kind's handler runs (decision D8).
type Mode string

const (
	// ModeTx runs each handler call in a short transaction that holds the
	// execution row; checkpoints and completion commit with its writes.
	ModeTx Mode = "tx"
	// ModeActivity runs the handler outside any transaction with a renewed
	// lease. Use it for remote calls (RunStep) and long work.
	ModeActivity Mode = "activity"
)

// Status of an execution.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Terminal reports whether no further attempts will run.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCancelled
}

// Job is a stored execution.
type Job struct {
	ID              uuid.UUID
	Queue, Kind     string
	Scope, Key      string
	Payload         json.RawMessage
	PayloadVersion  int
	Mode            Mode
	Status          Status
	Attempts        int
	MaxAttempts     int
	RunAt           time.Time
	Token           int64
	CancelRequested bool
	CheckpointName  string
	CheckpointData  json.RawMessage
	LastError       string
	Output          json.RawMessage
	CreatedAt       time.Time
	UpdatedAt       time.Time
	FinishedAt      *time.Time
}

const executionsTable = "durable.executions"

func jobFrom(e postgres.DurableExecution) Job {
	j := Job{
		ID: e.ID, Queue: e.Queue, Kind: e.Kind, Scope: e.Scope.String, Key: e.Key.String,
		Payload: e.Payload.Raw(), PayloadVersion: int(e.PayloadVersion), Mode: Mode(e.Mode.String),
		Status: Status(e.Status), Attempts: int(e.Attempts), MaxAttempts: int(e.MaxAttempts),
		RunAt: e.RunAt, Token: e.Token, CancelRequested: e.CancelRequested,
		CheckpointName: e.CheckpointName, CheckpointData: e.CheckpointData.Raw(),
		LastError: e.LastError.String, Output: e.Output.Raw(),
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
	if e.FinishedAt.Valid {
		j.FinishedAt = &e.FinishedAt.Time
	}
	return j
}

// Config is copied at construction.
type Config struct {
	DB *dbtx.DB
	// Kinds that may be submitted. Submitting any other kind returns
	// ErrUnknownKind, so a typo cannot create work nobody processes (Q4).
	Kinds []string
	// Queue for submissions that do not name one. Default "default".
	Queue string
	// KeyRetention is how long idempotency keys are remembered. Default 7 days.
	KeyRetention time.Duration
	// RequireTx makes Submit fail with ErrNotInTx unless ctx carries a
	// transaction, so the job always commits with the caller's writes.
	RequireTx bool
}

// Client submits and inspects executions. It is safe for concurrent use.
type Client struct{ cfg Config }

func NewClient(cfg Config) (*Client, error) {
	if cfg.DB == nil || cfg.DB.Unwrap() == nil {
		return nil, fmt.Errorf("%w: DB is required", durable.ErrInvalidConfig)
	}
	if len(cfg.Kinds) == 0 || slices.Contains(cfg.Kinds, "") {
		return nil, fmt.Errorf("%w: at least one non-empty kind is required", durable.ErrInvalidConfig)
	}
	if cfg.Queue == "" {
		cfg.Queue = "default"
	}
	if cfg.KeyRetention == 0 {
		cfg.KeyRetention = 7 * 24 * time.Hour
	}
	cfg.Kinds = slices.Clone(cfg.Kinds)
	return &Client{cfg: cfg}, nil
}

// SubmitRequest describes one execution.
type SubmitRequest struct {
	Kind string
	// Payload is JSON-encoded unless it is already json.RawMessage or []byte.
	Payload        any
	PayloadVersion int    // default 1
	Queue          string // default Config.Queue
	// Key makes submission idempotent within Scope: resubmitting the same key
	// and payload returns the existing execution; a different payload returns
	// ErrRequestMismatch. Empty Key means no deduplication.
	Key         string
	Scope       string    // default "jobs"
	MaxAttempts int       // default 10
	RunAt       time.Time // default now (database clock)
}

// SubmitResult describes the execution a submission created or found.
type SubmitResult struct {
	ID      uuid.UUID
	Status  Status
	Created bool // false: an earlier submission with the same key was returned
	Output  json.RawMessage
	Error   string
}

// Submit stores an execution. It joins a dbtx transaction in ctx (same DB ID)
// so the execution commits with the caller's writes.
func (c *Client) Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error) {
	if !slices.Contains(c.cfg.Kinds, req.Kind) {
		return SubmitResult{}, fmt.Errorf("%w: %q", durable.ErrUnknownKind, req.Kind)
	}
	if c.cfg.RequireTx && !c.cfg.DB.IsTx(ctx) {
		return SubmitResult{}, durable.ErrNotInTx
	}
	return submit(ctx, c.cfg.DB, c.cfg.Queue, c.cfg.KeyRetention, req)
}

// submit is shared with compensation and other packages' internal paths.
func submit(ctx context.Context, db *dbtx.DB, defaultQueue string, keyRetention time.Duration, req SubmitRequest) (SubmitResult, error) {
	if req.Kind == "" {
		return SubmitResult{}, fmt.Errorf("%w: kind is required", durable.ErrInvalidArgument)
	}
	payload, err := pg.Marshal(req.Payload)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("%w: payload: %v", durable.ErrInvalidArgument, err)
	}
	if string(payload) == "null" {
		payload = []byte("{}")
	}
	if req.Queue == "" {
		req.Queue = defaultQueue
	}
	if req.Scope == "" {
		req.Scope = "jobs"
	}
	if req.PayloadVersion == 0 {
		req.PayloadVersion = 1
	}
	if req.MaxAttempts == 0 {
		req.MaxAttempts = 10
	}
	if req.MaxAttempts < 0 || req.PayloadVersion < 0 {
		return SubmitResult{}, fmt.Errorf("%w: negative MaxAttempts or PayloadVersion", durable.ErrInvalidArgument)
	}
	id := pg.NewID()
	var res SubmitResult
	err = db.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(db.DBTx(txCtx))
		if req.Key != "" {
			request, _ := json.Marshal(map[string]any{"kind": req.Kind, "payload": json.RawMessage(payload)})
			inserted, err := q.LinkJobKey(txCtx, postgres.LinkJobKeyParams{
				Scope: req.Scope, Key: req.Key, Request: request,
				ExecutionID: sql.Null[uuid.UUID]{V: id, Valid: true}, RetentionUs: pg.Micros(keyRetention),
			})
			if err != nil {
				return err
			}
			if !inserted {
				k, err := q.GetJobKey(txCtx, postgres.GetJobKeyParams{Request: request, Scope: req.Scope, Key: req.Key})
				if err != nil {
					return err
				}
				if !k.SameRequest || k.Status != "linked" || !k.ExecutionID.Valid {
					return durable.ErrRequestMismatch
				}
				e, err := q.GetExecution(txCtx, k.ExecutionID.V)
				if errors.Is(err, sql.ErrNoRows) {
					// The execution was purged while its key is still remembered.
					res = SubmitResult{ID: k.ExecutionID.V, Status: StatusSucceeded}
					return nil
				}
				if err != nil {
					return err
				}
				res = SubmitResult{ID: e.ID, Status: Status(e.Status), Output: e.Output.Raw(), Error: e.LastError.String}
				return nil
			}
		}
		p := postgres.InsertExecutionParams{
			ID: id, Queue: req.Queue, Kind: req.Kind, Payload: payload,
			PayloadVersion: int32(req.PayloadVersion), MaxAttempts: int32(req.MaxAttempts),
		}
		if req.Key != "" {
			p.Scope = sql.NullString{String: req.Scope, Valid: true}
			p.Key = sql.NullString{String: req.Key, Valid: true}
		}
		if !req.RunAt.IsZero() {
			p.RunAt = sql.NullTime{Time: req.RunAt, Valid: true}
		}
		if err := q.InsertExecution(txCtx, p); err != nil {
			return err
		}
		res = SubmitResult{ID: id, Status: StatusPending, Created: true}
		return nil
	})
	return res, err
}

// Get returns an execution by ID.
func (c *Client) Get(ctx context.Context, id uuid.UUID) (Job, error) {
	e, err := postgres.New(c.cfg.DB.DBTx(ctx)).GetExecution(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, durable.ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	return jobFrom(e), nil
}

// GetByKey returns the execution submitted with scope and key.
func (c *Client) GetByKey(ctx context.Context, scope, key string) (Job, error) {
	if scope == "" {
		scope = "jobs"
	}
	e, err := postgres.New(c.cfg.DB.DBTx(ctx)).GetExecutionByKey(ctx, postgres.GetExecutionByKeyParams{Scope: scope, Key: key})
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, durable.ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	return jobFrom(e), nil
}

// Cancel stops an execution. Pending executions become cancelled at once;
// running ones get a cancel request that the worker observes at its next
// heartbeat or step, cancelling the handler's context with ErrCancelled.
// Finished executions are unchanged. It returns the resulting status.
func (c *Client) Cancel(ctx context.Context, id uuid.UUID) (Status, error) {
	status, err := postgres.New(c.cfg.DB.DBTx(ctx)).CancelExecution(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		j, err := c.Get(ctx, id)
		return j.Status, err
	}
	return Status(status), err
}

// Await polls until the execution is terminal or ctx ends. A failed or
// cancelled execution is returned with a nil error; a timeout returns ctx's
// error. Polling interval defaults to 200ms (Q10: no LISTEN dependency).
func (c *Client) Await(ctx context.Context, id uuid.UUID, interval time.Duration) (Job, error) {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	for {
		j, err := c.Get(ctx, id)
		if err != nil {
			return j, err
		}
		if j.Status.Terminal() {
			return j, nil
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return j, context.Cause(ctx)
		case <-t.C:
		}
	}
}
