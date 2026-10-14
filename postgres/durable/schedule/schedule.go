// Package schedule submits jobs on cron or interval schedules. Any number of
// schedulers can run: each tick becomes one job, keyed by schedule name and
// tick time, so duplicates are impossible.
package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/jobs"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// Missed decides what happens to ticks that passed while no scheduler ran.
type Missed string

const (
	MissedAll    Missed = "all"    // submit every missed tick (up to MaxCatchUp)
	MissedLatest Missed = "latest" // submit only the most recent tick (default, Q5)
	MissedNone   Missed = "none"   // submit nothing older than MissedGrace
)

// Overlap decides what happens when the previous tick's job is unfinished.
type Overlap string

const (
	OverlapSkip  Overlap = "skip"  // drop the tick (default, Q5)
	OverlapQueue Overlap = "queue" // wait until the previous job finishes
	OverlapAllow Overlap = "allow" // submit anyway
)

// Schedule defines one recurring job.
type Schedule struct {
	Name     string
	Spec     string // see Parse
	TimeZone string // IANA name; default UTC
	Kind     string
	Queue    string // default: the jobs client's queue
	Payload  any
	Missed   Missed
	Overlap  Overlap
}

// Config is copied at construction.
type Config struct {
	DB           *dbtx.DB
	Jobs         *jobs.Client
	PollInterval time.Duration // default 1s
	MaxCatchUp   int           // ticks submitted per schedule per pass with MissedAll; default 100
	MissedGrace  time.Duration // how late a tick may run under MissedNone; default 1m
	Observer     durable.Observer
}

type Scheduler struct{ cfg Config }

func New(cfg Config) (*Scheduler, error) {
	if cfg.DB == nil || cfg.Jobs == nil {
		return nil, fmt.Errorf("%w: DB and Jobs are required", durable.ErrInvalidConfig)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.MaxCatchUp <= 0 {
		cfg.MaxCatchUp = 100
	}
	if cfg.MissedGrace <= 0 {
		cfg.MissedGrace = time.Minute
	}
	return &Scheduler{cfg: cfg}, nil
}

func parse(spec, tz string) (Spec, error) {
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("schedule: time zone %q: %w", tz, err)
	}
	return Parse(spec, loc)
}

// Register creates or updates a schedule. The next tick is recomputed when
// the schedule is new or its spec or time zone changed.
func (s *Scheduler) Register(ctx context.Context, sch Schedule) error {
	if sch.Name == "" || sch.Kind == "" {
		return fmt.Errorf("%w: name and kind are required", durable.ErrInvalidArgument)
	}
	if sch.TimeZone == "" {
		sch.TimeZone = "UTC"
	}
	if sch.Missed == "" {
		sch.Missed = MissedLatest
	}
	if sch.Overlap == "" {
		sch.Overlap = OverlapSkip
	}
	spec, err := parse(sch.Spec, sch.TimeZone)
	if err != nil {
		return fmt.Errorf("%w: %v", durable.ErrInvalidArgument, err)
	}
	payload, err := pg.Marshal(sch.Payload)
	if err != nil {
		return fmt.Errorf("%w: payload: %v", durable.ErrInvalidArgument, err)
	}
	if string(payload) == "null" {
		payload = []byte("{}")
	}
	q := postgres.New(s.cfg.DB.DBTx(ctx))
	now, err := q.ClockTimestamp(ctx)
	if err != nil {
		return err
	}
	next := spec.Next(now)
	if next.IsZero() {
		return fmt.Errorf("%w: spec %q never fires", durable.ErrInvalidArgument, sch.Spec)
	}
	return q.UpsertSchedule(ctx, postgres.UpsertScheduleParams{
		Name: sch.Name, Spec: sch.Spec, TimeZone: sch.TimeZone, Queue: sch.Queue, Kind: sch.Kind,
		Payload: payload, Missed: string(sch.Missed), Overlap: string(sch.Overlap), NextRunAt: next,
	})
}

// Pause stops a schedule from firing; Resume restarts it from the next tick
// after now (ticks during the pause are not caught up).
func (s *Scheduler) Pause(ctx context.Context, name string) error {
	return s.setPaused(ctx, name, true)
}

func (s *Scheduler) Resume(ctx context.Context, name string) error {
	return s.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(s.cfg.DB.DBTx(txCtx))
		row, err := q.LockSchedule(txCtx, name)
		if errors.Is(err, sql.ErrNoRows) {
			return durable.ErrNotFound
		}
		if err != nil {
			return err
		}
		sp, err := parse(row.Spec, row.TimeZone)
		if err != nil {
			return err
		}
		return q.ResumeSchedule(txCtx, postgres.ResumeScheduleParams{NextRunAt: sp.Next(row.Now), Name: name})
	})
}

func (s *Scheduler) setPaused(ctx context.Context, name string, paused bool) error {
	n, err := postgres.New(s.cfg.DB.DBTx(ctx)).SetSchedulePaused(ctx, postgres.SetSchedulePausedParams{Paused: paused, Name: name})
	if err != nil {
		return err
	}
	if n == 0 {
		return durable.ErrNotFound
	}
	return nil
}

// Delete removes a schedule. Jobs it already submitted are unaffected.
func (s *Scheduler) Delete(ctx context.Context, name string) error {
	return postgres.New(s.cfg.DB.DBTx(ctx)).DeleteSchedule(ctx, name)
}

// TickKey is the idempotency key (scope "schedule") of a tick's job.
func TickKey(name string, tick time.Time) string {
	return name + "@" + tick.UTC().Format(time.RFC3339Nano)
}

// Run fires due schedules until ctx ends.
func (s *Scheduler) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		n, err := s.RunOnce(ctx)
		if err == nil && n > 0 {
			continue
		}
		t := time.NewTimer(s.cfg.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	return nil
}

// RunOnce handles every due schedule once and returns how many jobs it
// submitted.
func (s *Scheduler) RunOnce(ctx context.Context) (int, error) {
	total := 0
	seen := map[string]bool{}
	for {
		name, n, err := s.fireOne(ctx, seen)
		if err != nil || name == "" {
			return total, err
		}
		seen[name] = true
		total += n
	}
}

func (s *Scheduler) fireOne(ctx context.Context, seen map[string]bool) (string, int, error) {
	var (
		name      string
		submitted int
	)
	skip, _ := json.Marshal(mapKeys(seen))
	err := s.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(s.cfg.DB.DBTx(txCtx))
		due, err := q.NextDueSchedule(txCtx, skip)
		if errors.Is(err, sql.ErrNoRows) {
			name = ""
			return nil
		}
		if err != nil {
			return err
		}
		name = due.Name
		spec, tz, kind, queue, missed, overlap := due.Spec, due.TimeZone, due.Kind, due.Queue, due.Missed, due.Overlap
		payload, next, now, lastExec := due.Payload, due.NextRunAt, due.Now, due.LastExecutionID
		sp, err := parse(spec, tz)
		if err != nil {
			return err
		}
		var (
			fire   []time.Time
			latest time.Time
		)
		switch Missed(missed) {
		case MissedAll:
			// Oldest first, at most MaxCatchUp per pass; next_run_at moves
			// to the tick after the last one fired, so later passes continue.
			for t := next; !t.IsZero() && !t.After(now) && len(fire) < s.cfg.MaxCatchUp; t = sp.Next(t) {
				fire = append(fire, t)
			}
			latest = fire[len(fire)-1]
		case MissedNone:
			latest = latestTick(sp, next, now)
			if now.Sub(latest) <= s.cfg.MissedGrace {
				fire = []time.Time{latest}
			}
		default:
			latest = latestTick(sp, next, now)
			fire = []time.Time{latest}
		}
		if len(fire) > 0 && lastExec.Valid && Overlap(overlap) != OverlapAllow {
			prev, err := s.cfg.Jobs.Get(txCtx, lastExec.V)
			if err == nil && !prev.Status.Terminal() {
				if Overlap(overlap) == OverlapQueue {
					return nil // try again on the next pass; next_run_at unchanged
				}
				fire = nil // OverlapSkip
			}
		}
		lastID := lastExec.V // zero when there is none
		for _, tick := range fire {
			res, err := s.cfg.Jobs.Submit(txCtx, jobs.SubmitRequest{
				Kind: kind, Queue: queue, Payload: json.RawMessage(payload),
				Scope: "schedule", Key: TickKey(name, tick),
			})
			if err != nil {
				return err
			}
			if res.Created {
				submitted++
			}
			lastID = res.ID
		}
		following := sp.Next(latest)
		if following.IsZero() {
			following = latest.AddDate(100, 0, 0)
		}
		return q.AdvanceSchedule(txCtx, postgres.AdvanceScheduleParams{
			NextRunAt: following, Fired: len(fire) > 0, LastTick: latest, LastExecutionID: lastID, Name: name,
		})
	})
	return name, submitted, err
}

// latestTick returns the last tick at or before now, starting from a due tick
// next. Intervals are computed directly, so a long outage with "@every 1s"
// does not iterate millions of ticks; cron specs step at most a million times.
func latestTick(sp Spec, next, now time.Time) time.Time {
	if e, ok := sp.(every); ok {
		d := time.Duration(e)
		return next.Add(now.Sub(next) / d * d)
	}
	latest := next
	for range 1_000_000 {
		t := sp.Next(latest)
		if t.IsZero() || t.After(now) {
			break
		}
		latest = t
	}
	return latest
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
