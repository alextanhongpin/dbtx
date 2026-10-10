// Package repository implements ab persistence with database/sql and dbtx.
package repository

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/ab"
	"github.com/alextanhongpin/dbtx/postgres/ab/internal/validate"
)

// Schema must be applied by the application's migrations before use.
//
//go:embed schema.sql
var Schema string

// Repository joins transactions using DBTx(ctx). Configure its dbtx ID before use.
// Assignment and recording retain experiment locks until the transaction ends.
// Close becomes visible on commit. Database deadlock and serialization errors are
// returned without retries; callers may need to retry their whole transaction.
type Repository struct{ *dbtx.DB }

var _ ab.Repository = (*Repository)(nil)

func New(db *sql.DB) *Repository { return &Repository{DB: dbtx.New(db)} }

func (r *Repository) Create(ctx context.Context, e ab.Experiment) (err error) {
	defer func() { err = wrap("create", err) }()
	if err := e.Validate(); err != nil {
		return err
	}
	res, err := r.DBTx(ctx).ExecContext(ctx, `INSERT INTO dbtx.ab_experiments(id,control,treatment) VALUES($1,$2,$3) ON CONFLICT(id) DO NOTHING`, e.ID, e.Control, e.Treatment)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ab.ErrExists
	}
	return nil
}
func (r *Repository) Load(ctx context.Context, id string) (e ab.Experiment, err error) {
	defer func() { err = wrap("load", err) }()
	if err := validate.ID("experiment", id); err != nil {
		return e, err
	}
	err = r.DBTx(ctx).QueryRowContext(ctx, `SELECT id,control,treatment,created_at,closed_at FROM dbtx.ab_experiments WHERE id=$1`, id).Scan(&e.ID, &e.Control, &e.Treatment, &e.CreatedAt, &e.ClosedAt)
	return e, notExist(err)
}

// lock holds a shared experiment lock through the mutation transaction, allowing
// concurrent recordings while excluding Close's UPDATE lock.
func (r *Repository) lock(ctx context.Context, id string) (ab.Experiment, error) {
	var e ab.Experiment
	err := r.DBTx(ctx).QueryRowContext(ctx, `SELECT id,control,treatment,created_at,closed_at FROM dbtx.ab_experiments WHERE id=$1 FOR SHARE`, id).Scan(&e.ID, &e.Control, &e.Treatment, &e.CreatedAt, &e.ClosedAt)
	return e, notExist(err)
}
func (r *Repository) assignment(ctx context.Context, id, subject string) (ab.Assignment, error) {
	var a ab.Assignment
	err := r.DBTx(ctx).QueryRowContext(ctx, `SELECT experiment_id,subject_id,variant,assigned_at,converted_at FROM dbtx.ab_assignments WHERE experiment_id=$1 AND subject_id=$2`, id, subject).Scan(&a.ExperimentID, &a.SubjectID, &a.Variant, &a.AssignedAt, &a.ConvertedAt)
	return a, err
}
func (r *Repository) Assign(ctx context.Context, id, subject, variant string) (a ab.Assignment, err error) {
	defer func() { err = wrap("assign", err) }()
	if err := validate.Subject(id, subject); err != nil {
		return a, err
	}
	err = r.RunInTx(ctx, func(ctx context.Context) error {
		e, err := r.lock(ctx, id)
		if err != nil {
			return err
		}
		a, err = r.assignment(ctx, id, subject)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if e.ClosedAt != nil {
			return ab.ErrClosed
		}
		if variant != e.Control && variant != e.Treatment {
			return fmt.Errorf("ab: unknown variant")
		}
		_, err = r.DBTx(ctx).ExecContext(ctx, `INSERT INTO dbtx.ab_assignments(experiment_id,subject_id,variant) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, subject, variant)
		if err != nil {
			return err
		}
		a, err = r.assignment(ctx, id, subject)
		return err
	})
	return a, err
}
func (r *Repository) Record(ctx context.Context, id, subject string) (err error) {
	defer func() { err = wrap("record", err) }()
	if err := validate.Subject(id, subject); err != nil {
		return err
	}
	return r.RunInTx(ctx, func(ctx context.Context) error {
		e, err := r.lock(ctx, id)
		if err != nil {
			return err
		}
		a, err := r.assignment(ctx, id, subject)
		if errors.Is(err, sql.ErrNoRows) {
			return ab.ErrNotAssigned
		}
		if err != nil {
			return err
		}
		if a.ConvertedAt != nil {
			return nil
		}
		if e.ClosedAt != nil {
			return ab.ErrClosed
		}
		_, err = r.DBTx(ctx).ExecContext(ctx, `UPDATE dbtx.ab_assignments SET converted_at=clock_timestamp() WHERE experiment_id=$1 AND subject_id=$2 AND converted_at IS NULL`, id, subject)
		return err
	})
}
func (r *Repository) Close(ctx context.Context, id string) (err error) {
	defer func() { err = wrap("close", err) }()
	if err := validate.ID("experiment", id); err != nil {
		return err
	}
	res, err := r.DBTx(ctx).ExecContext(ctx, `UPDATE dbtx.ab_experiments SET closed_at=COALESCE(closed_at,clock_timestamp()) WHERE id=$1`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ab.ErrNotExist
	}
	return nil
}

// Results uses one statement so both arms share a single MVCC snapshot.
func (r *Repository) Results(ctx context.Context, id string) (out ab.Results, err error) {
	defer func() { err = wrap("results", err) }()
	if err := validate.ID("experiment", id); err != nil {
		return out, err
	}
	err = r.DBTx(ctx).QueryRowContext(ctx, `SELECT e.id,e.control,e.treatment,e.created_at,e.closed_at,
 count(a.subject_id) FILTER(WHERE a.variant=e.control),count(a.converted_at) FILTER(WHERE a.variant=e.control),
 count(a.subject_id) FILTER(WHERE a.variant=e.treatment),count(a.converted_at) FILTER(WHERE a.variant=e.treatment)
 FROM dbtx.ab_experiments e LEFT JOIN dbtx.ab_assignments a ON a.experiment_id=e.id
 WHERE e.id=$1 GROUP BY e.id`, id).Scan(&out.Experiment.ID, &out.Experiment.Control, &out.Experiment.Treatment, &out.Experiment.CreatedAt, &out.Experiment.ClosedAt, &out.Control.Subjects, &out.Control.Conversions, &out.Treatment.Subjects, &out.Treatment.Conversions)
	return out, notExist(err)
}
func notExist(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ab.ErrNotExist
	}
	return err
}

func wrap(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("ab repository %s: %w", operation, err)
}
