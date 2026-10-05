package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"uuid"
)

// SubmitConfig tunes input validation and defaults.
type SubmitConfig struct {
	DefaultMaxAttempts int32 // used when SubmitInput.MaxAttempts == 0 (default 10)
	MaxAttemptsLimit   int32 // upper bound a caller may request (default 100)
	MaxKeyLength       int   // default 255
	MaxRequestBytes    int   // default 1 MiB
}

func (c *SubmitConfig) applyDefaults() {
	if c.DefaultMaxAttempts <= 0 {
		c.DefaultMaxAttempts = 10
	}
	if c.MaxAttemptsLimit <= 0 {
		c.MaxAttemptsLimit = 100
	}
	if c.MaxKeyLength <= 0 {
		c.MaxKeyLength = 255
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = 1 << 20
	}
}

// Submitter is the producer-side use case: idempotently enqueue a job.
type Submitter struct {
	repo Repository
	cfg  SubmitConfig
}

func NewSubmitter(repo Repository, cfg SubmitConfig) *Submitter {
	cfg.applyDefaults()
	return &Submitter{repo: repo, cfg: cfg}
}

type SubmitInput struct {
	IdempotencyKey string          // supplied by the client; same key => same job
	Request        json.RawMessage // job payload; empty => {}
	Delay          time.Duration   // optional: do not run before now+Delay
	MaxAttempts    int32           // optional: 0 => default
}

type SubmitResult struct {
	JobID    uuid.UUID
	Status   Status
	Response json.RawMessage // populated once the job has succeeded
	Error    string          // last error, if any
	Created  bool            // false => this was a replay of an earlier submit
}

// Submit enqueues a job, or returns the existing one for a repeated key.
//
// Outcomes:
//   - new key                   -> Created=true, status 'idle'
//   - same key, same payload    -> Created=false, current status/response
//     (safe for clients to retry and to poll with)
//   - same key, other payload   -> ErrIdempotencyKeyReuse (the result still
//     carries the original job's id/status)
func (s *Submitter) Submit(ctx context.Context, in SubmitInput) (SubmitResult, error) {
	req, maxAttempts, err := s.validate(in)
	if err != nil {
		return SubmitResult{}, err
	}

	res, err := s.repo.Create(ctx, CreateParams{
		IdempotencyKey: in.IdempotencyKey,
		Request:        req,
		DelaySeconds:   in.Delay.Seconds(),
		MaxAttempts:    maxAttempts,
	})
	if err != nil {
		return SubmitResult{}, fmt.Errorf("create job: %w", err)
	}

	out := SubmitResult{
		JobID:    res.ID,
		Status:   res.Status,
		Response: res.Response,
		Created:  res.Created,
	}
	if res.Error != nil {
		out.Error = *res.Error
	}

	if !res.PayloadMatches {
		return out, fmt.Errorf("%w: key %q belongs to job %s", ErrIdempotencyKeyReuse, in.IdempotencyKey, res.ID)
	}
	return out, nil
}

func (s *Submitter) validate(in SubmitInput) (json.RawMessage, int32, error) {
	if in.IdempotencyKey == "" {
		return nil, 0, fmt.Errorf("%w: idempotency key is required", ErrInvalidInput)
	}
	if len(in.IdempotencyKey) > s.cfg.MaxKeyLength {
		return nil, 0, fmt.Errorf("%w: idempotency key longer than %d bytes", ErrInvalidInput, s.cfg.MaxKeyLength)
	}

	req := in.Request
	if len(req) == 0 {
		req = json.RawMessage(`{}`)
	}
	if len(req) > s.cfg.MaxRequestBytes {
		return nil, 0, fmt.Errorf("%w: request larger than %d bytes", ErrInvalidInput, s.cfg.MaxRequestBytes)
	}
	if err := validJSONB(req); err != nil {
		return nil, 0, fmt.Errorf("%w: request: %w", ErrInvalidInput, err)
	}

	if in.Delay < 0 {
		return nil, 0, fmt.Errorf("%w: delay must not be negative", ErrInvalidInput)
	}

	maxAttempts := in.MaxAttempts
	switch {
	case maxAttempts == 0:
		maxAttempts = s.cfg.DefaultMaxAttempts
	case maxAttempts < 0 || maxAttempts > s.cfg.MaxAttemptsLimit:
		return nil, 0, fmt.Errorf("%w: max attempts must be between 1 and %d", ErrInvalidInput, s.cfg.MaxAttemptsLimit)
	}
	return req, maxAttempts, nil
}
