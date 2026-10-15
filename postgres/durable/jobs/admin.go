package jobs

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// Filter selects failed executions.
type Filter struct {
	Kind          string    // exact kind; empty matches all
	ErrorContains string    // substring of last_error; empty matches all
	Limit         int       // default 100
	After         uuid.UUID // keyset pagination: IDs greater than this
}

// ListFailed returns failed executions, oldest ID first.
func (c *Client) ListFailed(ctx context.Context, f Filter) ([]Job, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	rows, err := postgres.New(c.cfg.DB.DBTx(ctx)).ListFailedExecutions(ctx, postgres.ListFailedExecutionsParams{
		Kind: f.Kind, ErrorContains: f.ErrorContains, After: f.After, Lim: int32(f.Limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Job, len(rows))
	for i, r := range rows {
		out[i] = jobFrom(r)
	}
	return out, nil
}

// Requeue makes a failed or cancelled execution pending again and grants it
// as many extra attempts as it originally had (Q2). The attempt history is
// kept in execution_attempts.
func (c *Client) Requeue(ctx context.Context, id uuid.UUID) error {
	n, err := postgres.New(c.cfg.DB.DBTx(ctx)).RequeueExecution(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: no failed or cancelled execution %s", durable.ErrNotFound, id)
	}
	return nil
}

// RequeueFailed requeues every failed execution matching f (Limit ignored).
func (c *Client) RequeueFailed(ctx context.Context, f Filter) (int64, error) {
	return postgres.New(c.cfg.DB.DBTx(ctx)).RequeueFailedExecutions(ctx, postgres.RequeueFailedExecutionsParams{
		Kind: f.Kind, ErrorContains: f.ErrorContains,
	})
}

// OldestPayloadVersions reports, per kind, the lowest payload version among
// unfinished executions, so handlers know which versions they must still
// accept (Q8).
func (c *Client) OldestPayloadVersions(ctx context.Context) (map[string]int, error) {
	rows, err := postgres.New(c.cfg.DB.DBTx(ctx)).OldestPayloadVersions(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.Kind] = int(r.Version)
	}
	return out, nil
}

// Purge deletes up to limit executions that finished before cutoff and whose
// idempotency key (if any) has expired, so a remembered key never points at a
// missing execution. Steps and attempts are deleted with them.
func (c *Client) Purge(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	return postgres.New(c.cfg.DB.DB()).PurgeExecutions(ctx, postgres.PurgeExecutionsParams{Cutoff: cutoff, Lim: int32(limit)})
}

// PurgeKeys deletes up to limit expired idempotency keys that are not in
// flight, including keys of synchronous requests (package keys).
func (c *Client) PurgeKeys(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	return postgres.New(c.cfg.DB.DB()).PurgeKeys(ctx, int32(limit))
}

// Janitor periodically purges finished executions and expired keys.
type Janitor struct {
	Client    *Client
	Interval  time.Duration // default 1 minute
	Retention time.Duration // finished executions kept this long; default 24h
	Observer  durable.Observer
}

// Run purges until ctx ends. Each pass repeats a purge while it fills a batch.
func (j *Janitor) Run(ctx context.Context) error {
	if j.Client == nil {
		return fmt.Errorf("%w: Client is required", durable.ErrInvalidConfig)
	}
	interval, retention := j.Interval, j.Retention
	if interval <= 0 {
		interval = time.Minute
	}
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	for {
		for {
			n, err := j.Client.Purge(ctx, time.Now().Add(-retention), 1000)
			if err != nil || n < 1000 {
				break
			}
		}
		for {
			n, err := j.Client.PurgeKeys(ctx, 1000)
			if err != nil || n < 1000 {
				break
			}
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}
