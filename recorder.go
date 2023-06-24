package dbtx

import (
	"context"
	"database/sql"
)

type logger interface {
	Log(method, query string, args ...any)
}

var _ DBTX = (*Recorder)(nil)

type Recorder struct {
	dbtx DBTX
	l    logger
}

func NewRecorder(dbtx DBTX, l logger) *Recorder {
	return &Recorder{dbtx: dbtx, l: l}
}

func (r *Recorder) Exec(query string, args ...any) (sql.Result, error) {
	r.l.Log("Exec", query, args...)

	return r.dbtx.Exec(query, args...)
}

func (r *Recorder) Prepare(query string) (*sql.Stmt, error) {
	r.l.Log("Prepare", query)

	return r.dbtx.Prepare(query)
}

func (r *Recorder) Query(query string, args ...any) (*sql.Rows, error) {
	r.l.Log("Query", query, args...)

	return r.dbtx.Query(query, args...)
}

func (r *Recorder) QueryRow(query string, args ...any) *sql.Row {
	r.l.Log("QueryRow", query, args...)

	return r.dbtx.QueryRow(query, args...)
}

func (r *Recorder) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	r.l.Log("ExecContext", query, args...)

	return r.dbtx.ExecContext(ctx, query, args...)
}

func (r *Recorder) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	r.l.Log("PrepareContext", query)

	return r.dbtx.PrepareContext(ctx, query)
}

func (r *Recorder) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	r.l.Log("QueryContext", query, args...)

	return r.dbtx.QueryContext(ctx, query, args...)
}

func (r *Recorder) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	r.l.Log("QueryRowContext", query, args...)

	return r.dbtx.QueryRowContext(ctx, query, args...)
}
