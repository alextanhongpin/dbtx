package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// JanitorConfig controls background maintenance.
type JanitorConfig struct {
	Interval           time.Duration // how often to run a cycle (default 1m)
	Retention          time.Duration // keep finished jobs in the hot table this long (default 24h)
	KeyTTL             time.Duration // idempotency window; MUST exceed Retention (default 7d)
	BatchSize          int32         // rows per statement (default 1000)
	MaxBatchesPerCycle int           // cap per cycle so one cycle cannot run forever (default 50)
}

func (c *JanitorConfig) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = 24 * time.Hour
	}
	if c.KeyTTL <= 0 {
		c.KeyTTL = 7 * 24 * time.Hour
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 1000
	}
	if c.MaxBatchesPerCycle <= 0 {
		c.MaxBatchesPerCycle = 50
	}
}

// Janitor reaps exhausted jobs, archives finished ones and expires idempotency
// keys. Every statement is safe to run concurrently from several instances
// (FOR UPDATE SKIP LOCKED / idempotent updates), so no leader election is needed.
type Janitor struct {
	repo Repository
	cfg  JanitorConfig
	log  *slog.Logger
}

func NewJanitor(repo Repository, cfg JanitorConfig, log *slog.Logger) (*Janitor, error) {
	cfg.applyDefaults()
	if cfg.KeyTTL <= cfg.Retention {
		// Otherwise a client retrying a key could land after the job was
		// archived but still get a brand-new job once the key is gone.
		return nil, fmt.Errorf("%w: KeyTTL (%s) must be longer than Retention (%s)",
			ErrInvalidInput, cfg.KeyTTL, cfg.Retention)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Janitor{repo: repo, cfg: cfg, log: log}, nil
}

// Run blocks until ctx is cancelled. It returns ErrTxInContext if ctx carries a
// transaction of the repository.
func (j *Janitor) Run(ctx context.Context) error {
	if j.repo.IsTx(ctx) {
		return ErrTxInContext
	}
	j.cycle(ctx)
	t := time.NewTicker(j.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			j.cycle(ctx)
		}
	}
}

func (j *Janitor) cycle(ctx context.Context) {
	// 1. Jobs stuck on an expired lease with no attempts left -> 'failed'.
	//    Must run before archiving so they become archivable.
	if n, err := j.repo.ReapExhausted(ctx); err != nil {
		j.log.Error("reap exhausted failed", "err", err)
	} else if n > 0 {
		j.log.Warn("reaped jobs that exhausted their attempts", "count", n)
	}

	// 2. Move finished jobs to the archive, batch by batch.
	j.drain(ctx, "archive", func(ctx context.Context) (int64, error) {
		return j.repo.ArchiveOldJobs(ctx, ArchiveOldJobsParams{
			RetentionSeconds: j.cfg.Retention.Seconds(),
			BatchSize:        j.cfg.BatchSize,
		})
	})

	// 3. Expire idempotency keys last (they outlive the archived jobs).
	j.drain(ctx, "purge keys", func(ctx context.Context) (int64, error) {
		return j.repo.PurgeOldJobKeys(ctx, PurgeOldJobKeysParams{
			KeyTTLSeconds: j.cfg.KeyTTL.Seconds(),
			BatchSize:     j.cfg.BatchSize,
		})
	})
}

// drain repeats fn until a short batch signals there is nothing left.
func (j *Janitor) drain(ctx context.Context, what string, fn func(context.Context) (int64, error)) {
	var total int64
	for i := 0; i < j.cfg.MaxBatchesPerCycle && ctx.Err() == nil; i++ {
		n, err := fn(ctx)
		if err != nil {
			j.log.Error("janitor step failed", "step", what, "err", err)
			return
		}
		total += n
		if n < int64(j.cfg.BatchSize) {
			break
		}
	}
	if total > 0 {
		j.log.Info("janitor step done", "step", what, "rows", total)
	}
}
