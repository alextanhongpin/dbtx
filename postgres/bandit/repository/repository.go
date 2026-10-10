// Package repository persists bandit state with transaction-scoped run locks.
package repository

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/bandit"
	"github.com/alextanhongpin/dbtx/postgres/bandit/repository/postgres"
	"github.com/lib/pq"
)

// Schema is applied explicitly by applications, never by New.
//
//go:embed schema.sql
var Schema string

type Repository struct{ *dbtx.DB }

var _ bandit.Repository = (*Repository)(nil)

func New(db *sql.DB) *Repository                              { return &Repository{dbtx.New(db)} }
func (r *Repository) q(ctx context.Context) *postgres.Queries { return postgres.New(r.DBTx(ctx)) }
func missing(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return bandit.ErrNotExist
	}
	return err
}
func experiment(row postgres.DbtxBanditExperiment) (bandit.Experiment, error) {
	var e bandit.Experiment
	err := json.Unmarshal(row.Definition, &e)
	e.ID = row.ID
	e.CreatedAt = row.CreatedAt
	return e, err
}
func run(row postgres.DbtxBanditRun) (bandit.Run, error) {
	var v bandit.Run
	err := json.Unmarshal(row.Definition, &v)
	v.ID = row.ID
	v.ExperimentID = row.ExperimentID
	v.Sequence = row.Sequence
	v.Status = row.Status
	v.CreatedAt = row.CreatedAt
	v.ClosedAt = nil
	if row.ClosedAt.Valid {
		v.ClosedAt = &row.ClosedAt.Time
	}
	return v, err
}
func decision(row postgres.DecisionRow) (bandit.Decision, error) {
	var d bandit.Decision
	err := json.Unmarshal(row.Diagnostics, &d)
	d.ID = row.ID
	d.RunID = row.RunID
	d.ArmKey = row.ArmKey
	d.Sequence = row.Sequence
	d.RequestKey = row.RequestKey
	d.CreatedAt = row.CreatedAt
	d.Feedback = nil
	if row.Success.Valid {
		d.Feedback = &bandit.Feedback{Success: row.Success.Bool, ObservedAt: row.ObservedAt.Time}
	}
	return d, err
}
func (r *Repository) CreateExperiment(ctx context.Context, e bandit.Experiment) (out bandit.Experiment, err error) {
	if err = e.Validate(); err != nil {
		return
	}
	e.ID = uuid.NewV7()
	err = r.RunInTx(ctx, func(ctx context.Context) error {
		q := r.q(ctx)
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		row, err := q.CreateExperiment(ctx, postgres.CreateExperimentParams{ID: e.ID, Definition: b})
		if err != nil {
			return err
		}
		for i, a := range e.Arms {
			if err := q.CreateArm(ctx, postgres.CreateArmParams{ExperimentID: e.ID, ArmKey: a.Key, Label: a.Label, Ordinal: int32(i)}); err != nil {
				return err
			}
		}
		out, err = experiment(row)
		return err
	})
	return
}
func (r *Repository) start(ctx context.Context, v bandit.Run) (bandit.Run, error) {
	v.Policy = v.Policy.WithDefaults()
	if err := v.Validate(); err != nil {
		return bandit.Run{}, err
	}
	q := r.q(ctx)
	row, err := q.Experiment(ctx, v.ExperimentID)
	if err != nil {
		return bandit.Run{}, missing(err)
	}
	e, err := experiment(row)
	if err != nil {
		return bandit.Run{}, err
	}
	if v.Mode == "simulation" && len(e.Arms) != len(v.Probabilities) {
		return bandit.Run{}, fmt.Errorf("%w: arm/probability mismatch", bandit.ErrInvalid)
	}
	if v.ID == (uuid.UUID{}) {
		v.ID = uuid.NewV7()
	}
	v.Version = bandit.Version
	v.Sequence = 0
	v.Status = "open"
	v.ClosedAt = nil
	b, err := json.Marshal(v)
	if err != nil {
		return bandit.Run{}, err
	}
	rr, err := q.CreateRun(ctx, postgres.CreateRunParams{ID: v.ID, ExperimentID: v.ExperimentID, Definition: b})
	if err != nil {
		return bandit.Run{}, err
	}
	for _, a := range e.Arms {
		if err := q.CreateStatistics(ctx, postgres.CreateStatisticsParams{RunID: v.ID, ExperimentID: v.ExperimentID, ArmKey: a.Key}); err != nil {
			return bandit.Run{}, err
		}
	}
	return run(rr)
}
func (r *Repository) StartRun(ctx context.Context, v bandit.Run) (out bandit.Run, err error) {
	if v.Mode != "live" {
		return out, fmt.Errorf("%w: simulations must be persisted as complete repetitions", bandit.ErrInvalid)
	}
	err = r.RunInTx(ctx, func(ctx context.Context) error { var err error; out, err = r.start(ctx, v); return err })
	return
}
func (r *Repository) statistics(ctx context.Context, id uuid.UUID) ([]bandit.ArmStats, error) {
	rows, err := r.q(ctx).Statistics(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]bandit.ArmStats, len(rows))
	for i, s := range rows {
		out[i] = bandit.ArmStats{Arm: bandit.Arm{Key: s.ArmKey, Label: s.Label}, Selections: s.Selections, Observations: s.Observations, Successes: s.Successes}
	}
	return out, nil
}
func (r *Repository) Select(ctx context.Context, id uuid.UUID, key string) (out bandit.Decision, err error) {
	err = r.RunInTx(ctx, func(ctx context.Context) error {
		q := r.q(ctx)
		row, err := q.LockRun(ctx, id)
		if err != nil {
			return missing(err)
		}
		existing, err := q.DecisionByKey(ctx, postgres.DecisionByKeyParams{RunID: id, RequestKey: key})
		if err == nil {
			out, err = decision(postgres.DecisionRow(existing))
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		v, err := run(row)
		if err != nil {
			return err
		}
		if v.Status == "closed" {
			return bandit.ErrClosed
		}
		if v.Sequence >= 1000000 {
			return fmt.Errorf("%w: run reached one million decisions; start a new run", bandit.ErrInvalid)
		}
		if v.Mode != "live" || v.Version != bandit.Version {
			return fmt.Errorf("%w: unsupported run", bandit.ErrInvalid)
		}
		stats, err := r.statistics(ctx, id)
		if err != nil {
			return err
		}
		p, err := bandit.NewPolicy(v.Policy)
		if err != nil {
			return err
		}
		choice, err := p.Choose(stats, bandit.DecisionRandom(v.Seed, v.Sequence+1, 0))
		if err != nil {
			return err
		}
		out = bandit.Decision{ID: uuid.NewV7(), RunID: id, Sequence: v.Sequence + 1, RequestKey: key, ArmKey: stats[choice.Index].Arm.Key, Statistics: stats, Scores: choice.Scores}
		b, err := json.Marshal(out)
		if err != nil {
			return err
		}
		d, err := q.CreateDecision(ctx, postgres.CreateDecisionParams{ID: out.ID, RunID: id, ArmKey: out.ArmKey, Sequence: out.Sequence, RequestKey: key, Diagnostics: b})
		if err != nil {
			return err
		}
		out.CreatedAt = d.CreatedAt
		if err := q.SelectArm(ctx, postgres.SelectArmParams{RunID: id, ArmKey: out.ArmKey}); err != nil {
			return err
		}
		return q.Advance(ctx, id)
	})
	return
}
func (r *Repository) Observe(ctx context.Context, id uuid.UUID, success bool) error {
	return r.RunInTx(ctx, func(ctx context.Context) error {
		q := r.q(ctx)
		d, err := q.Decision(ctx, id)
		if err != nil {
			return missing(err)
		}
		if _, err := q.LockRun(ctx, d.RunID); err != nil {
			return missing(err)
		}
		// Re-read after obtaining the run lock: another observer may have committed.
		d, err = q.Decision(ctx, id)
		if err != nil {
			return missing(err)
		}
		if d.Success.Valid {
			if d.Success.Bool != success {
				return bandit.ErrConflict
			}
			return nil
		}
		if err := q.CreateFeedback(ctx, postgres.CreateFeedbackParams{DecisionID: id, Success: success}); err != nil {
			return err
		}
		reward := int64(0)
		if success {
			reward = 1
		}
		return q.ObserveArm(ctx, postgres.ObserveArmParams{RunID: d.RunID, ArmKey: d.ArmKey, Reward: reward})
	})
}
func (r *Repository) CloseRun(ctx context.Context, id uuid.UUID) error {
	return r.RunInTx(ctx, func(ctx context.Context) error {
		q := r.q(ctx)
		if _, err := q.LockRun(ctx, id); err != nil {
			return missing(err)
		}
		return q.CloseRun(ctx, id)
	})
}
func (r *Repository) ListExperiments(ctx context.Context, p bandit.Page) ([]bandit.Experiment, error) {
	rows, err := r.q(ctx).Experiments(ctx, postgres.ExperimentsParams{Limit: int32(p.Limit), Offset: int32(p.Offset)})
	if err != nil {
		return nil, err
	}
	out := make([]bandit.Experiment, len(rows))
	for i, row := range rows {
		out[i], err = experiment(row)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (r *Repository) ListRuns(ctx context.Context, id uuid.UUID, p bandit.Page) ([]bandit.Run, error) {
	q := r.q(ctx)
	if _, err := q.Experiment(ctx, id); err != nil {
		return nil, missing(err)
	}
	rows, err := q.Runs(ctx, postgres.RunsParams{ExperimentID: id, Limit: int32(p.Limit), Offset: int32(p.Offset)})
	if err != nil {
		return nil, err
	}
	out := make([]bandit.Run, len(rows))
	for i, row := range rows {
		out[i], err = run(row)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (r *Repository) Summary(ctx context.Context, id uuid.UUID) (out bandit.Summary, err error) {
	// The run lock makes the run and its counters a consistent snapshot.
	err = r.RunInTx(ctx, func(ctx context.Context) error {
		q := r.q(ctx)
		row, err := q.LockRun(ctx, id)
		if err != nil {
			return missing(err)
		}
		out.Run, err = run(row)
		if err != nil {
			return err
		}
		e, err := q.Experiment(ctx, row.ExperimentID)
		if err != nil {
			return missing(err)
		}
		out.Experiment, err = experiment(e)
		if err != nil {
			return err
		}
		out.Arms, err = r.statistics(ctx, id)
		return err
	})
	return
}
func (r *Repository) History(ctx context.Context, id uuid.UUID, p bandit.Page) ([]bandit.Decision, error) {
	q := r.q(ctx)
	if _, err := q.Run(ctx, id); err != nil {
		return nil, missing(err)
	}
	rows, err := q.History(ctx, postgres.HistoryParams{RunID: id, Limit: int32(p.Limit), Offset: int32(p.Offset)})
	if err != nil {
		return nil, err
	}
	out := make([]bandit.Decision, len(rows))
	for i, row := range rows {
		out[i], err = decision(postgres.DecisionRow(row))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *Repository) Trajectory(ctx context.Context, id uuid.UUID) (out []bandit.Decision, err error) {
	err = r.RunInTx(ctx, func(ctx context.Context) error {
		q := r.q(ctx)
		if _, err := q.LockRun(ctx, id); err != nil {
			return missing(err)
		}
		rows, err := q.Trajectory(ctx, id)
		if err != nil {
			return err
		}
		out = make([]bandit.Decision, len(rows))
		for i, row := range rows {
			out[i] = bandit.Decision{ID: row.ID, RunID: row.RunID, ArmKey: row.ArmKey, Sequence: row.Sequence, RequestKey: row.RequestKey, CreatedAt: row.CreatedAt}
			if row.Success.Valid {
				out[i].Feedback = &bandit.Feedback{Success: row.Success.Bool, ObservedAt: row.ObservedAt.Time}
			}
		}
		return nil
	})
	return
}

func (r *Repository) SaveSimulation(ctx context.Context, v bandit.Run, decisions []bandit.Decision, stats []bandit.ArmStats) (out bandit.Run, err error) {
	if v.Mode != "simulation" || len(decisions) != v.Horizon || v.Version != bandit.Version {
		return out, fmt.Errorf("%w: complete simulation required", bandit.ErrInvalid)
	}
	err = r.RunInTx(ctx, func(ctx context.Context) error {
		var err error
		out, err = r.start(ctx, v)
		if err != nil {
			return err
		}
		q := r.q(ctx)
		actual, err := r.statistics(ctx, out.ID)
		if err != nil {
			return err
		}
		if len(actual) != len(stats) {
			return fmt.Errorf("%w: statistics mismatch", bandit.ErrInvalid)
		}
		counts := map[string]bandit.ArmStats{}
		for i, a := range actual {
			if stats[i].Arm != a.Arm {
				return fmt.Errorf("%w: arm order mismatch", bandit.ErrInvalid)
			}
			counts[a.Arm.Key] = a
		}
		for i, d := range decisions {
			s, ok := counts[d.ArmKey]
			if !ok || d.RunID != out.ID || d.Sequence != int64(i+1) || d.Feedback == nil {
				return fmt.Errorf("%w: invalid simulation ledger", bandit.ErrInvalid)
			}
			s.Selections++
			s.Observations++
			if d.Feedback.Success {
				s.Successes++
			}
			counts[d.ArmKey] = s
		}
		for _, s := range stats {
			if counts[s.Arm.Key] != s {
				return fmt.Errorf("%w: ledger/statistics mismatch", bandit.ErrInvalid)
			}
		}
		// COPY keeps default 10,000-step repetitions practical without giving up
		// atomicity. It uses the same caller-owned transaction as generated queries.
		stmt, err := r.DBTx(ctx).PrepareContext(ctx, pq.CopyInSchema("dbtx", "bandit_decisions", "id", "run_id", "arm_key", "sequence", "request_key", "diagnostics", "created_at"))
		if err != nil {
			return err
		}
		for _, d := range decisions {
			b, err := json.Marshal(d)
			if err != nil {
				stmt.Close()
				return err
			}
			if _, err := stmt.ExecContext(ctx, d.ID, d.RunID, d.ArmKey, d.Sequence, d.RequestKey, string(b), d.CreatedAt); err != nil {
				stmt.Close()
				return err
			}
		}
		if _, err := stmt.ExecContext(ctx); err != nil {
			stmt.Close()
			return err
		}
		if err := stmt.Close(); err != nil {
			return err
		}
		stmt, err = r.DBTx(ctx).PrepareContext(ctx, pq.CopyInSchema("dbtx", "bandit_feedback", "decision_id", "success", "observed_at"))
		if err != nil {
			return err
		}
		for _, d := range decisions {
			if _, err := stmt.ExecContext(ctx, d.ID, d.Feedback.Success, d.Feedback.ObservedAt); err != nil {
				stmt.Close()
				return err
			}
		}
		if _, err := stmt.ExecContext(ctx); err != nil {
			stmt.Close()
			return err
		}
		if err := stmt.Close(); err != nil {
			return err
		}
		for _, s := range stats {
			if err := q.SaveStatistics(ctx, postgres.SaveStatisticsParams{RunID: out.ID, ArmKey: s.Arm.Key, Selections: s.Selections, Observations: s.Observations, Successes: s.Successes}); err != nil {
				return err
			}
		}
		if err := q.FinishSimulation(ctx, postgres.FinishSimulationParams{ID: out.ID, Sequence: int64(v.Horizon)}); err != nil {
			return err
		}
		row, err := q.Run(ctx, out.ID)
		if err != nil {
			return err
		}
		out, err = run(row)
		return err
	})
	return
}
