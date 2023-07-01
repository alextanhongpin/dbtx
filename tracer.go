package dbtx

import (
	"context"
	"database/sql"
	"time"
)

type Event struct {
	Method  string
	Query   string
	Args    []any
	Err     error
	StartAt time.Time
	EndAt   time.Time
}

type tracer interface {
	Trace(ctx context.Context, event Event)
}

var _ DBTX = (*Tracer)(nil)

// Tracer logs the query, args as well as the execution time and error.
type Tracer struct {
	dbtx DBTX
	t    tracer
}

func WithTracer(t tracer) Middleware {
	return func(dbtx DBTX) DBTX {
		return NewTracer(dbtx, t)
	}
}

func NewTracer(dbtx DBTX, t tracer) *Tracer {
	return &Tracer{dbtx: dbtx, t: t}
}

func (r *Tracer) Exec(query string, args ...any) (res sql.Result, err error) {
	defer func(start time.Time) {
		r.t.Trace(context.Background(), Event{
			Method:  "Exec",
			Query:   query,
			Args:    args,
			StartAt: start,
			EndAt:   time.Now(),
			Err:     err,
		})
	}(time.Now())

	return r.dbtx.Exec(query, args...)
}

func (r *Tracer) Prepare(query string) (stmt *sql.Stmt, err error) {
	defer func(start time.Time) {
		r.t.Trace(context.Background(), Event{
			Method:  "Prepare",
			Query:   query,
			StartAt: start,
			EndAt:   time.Now(),
			Err:     err,
		})
	}(time.Now())

	return r.dbtx.Prepare(query)
}

func (r *Tracer) Query(query string, args ...any) (rows *sql.Rows, err error) {
	defer func(start time.Time) {
		r.t.Trace(context.Background(), Event{
			Method:  "Query",
			Query:   query,
			Args:    args,
			StartAt: start,
			EndAt:   time.Now(),
			Err:     err,
		})
	}(time.Now())

	return r.dbtx.Query(query, args...)
}

func (r *Tracer) QueryRow(query string, args ...any) *sql.Row {
	defer func(start time.Time) {
		r.t.Trace(context.Background(), Event{
			Method:  "QueryRow",
			Query:   query,
			Args:    args,
			StartAt: start,
			EndAt:   time.Now(),
		})
	}(time.Now())

	return r.dbtx.QueryRow(query, args...)
}

func (r *Tracer) ExecContext(ctx context.Context, query string, args ...any) (res sql.Result, err error) {
	defer func(start time.Time) {
		r.t.Trace(ctx, Event{
			Method:  "ExecContext",
			Query:   query,
			Args:    args,
			StartAt: start,
			EndAt:   time.Now(),
			Err:     err,
		})
	}(time.Now())

	return r.dbtx.ExecContext(ctx, query, args...)
}

func (r *Tracer) PrepareContext(ctx context.Context, query string) (stmt *sql.Stmt, err error) {
	defer func(start time.Time) {
		r.t.Trace(ctx, Event{
			Method:  "PrepareContext",
			Query:   query,
			StartAt: start,
			EndAt:   time.Now(),
			Err:     err,
		})
	}(time.Now())

	return r.dbtx.PrepareContext(ctx, query)
}

func (r *Tracer) QueryContext(ctx context.Context, query string, args ...any) (rows *sql.Rows, err error) {
	defer func(start time.Time) {
		r.t.Trace(ctx, Event{
			Method:  "QueryContext",
			Query:   query,
			Args:    args,
			StartAt: start,
			EndAt:   time.Now(),
			Err:     err,
		})
	}(time.Now())

	return r.dbtx.QueryContext(ctx, query, args...)
}

func (r *Tracer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	defer func(start time.Time) {
		r.t.Trace(ctx, Event{
			Method:  "QueryRowContext",
			Query:   query,
			Args:    args,
			StartAt: start,
			EndAt:   time.Now(),
		})
	}(time.Now())

	return r.dbtx.QueryRowContext(ctx, query, args...)
}
