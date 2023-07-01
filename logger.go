package dbtx

import (
	"context"
	"database/sql"
)

type logger interface {
	Log(ctx context.Context, method, query string, args ...any)
}

var _ DBTX = (*Logger)(nil)

// Logger logs the query and args.
type Logger struct {
	dbtx DBTX
	l    logger
}

func WithLogger(l logger) Middleware {
	return func(dbtx DBTX) DBTX {
		return NewLogger(dbtx, l)
	}
}

func NewLogger(dbtx DBTX, l logger) *Logger {
	return &Logger{dbtx: dbtx, l: l}
}

func (r *Logger) Exec(query string, args ...any) (sql.Result, error) {
	r.l.Log(context.Background(), "Exec", query, args...)

	return r.dbtx.Exec(query, args...)
}

func (r *Logger) Prepare(query string) (*sql.Stmt, error) {
	r.l.Log(context.Background(), "Prepare", query)

	return r.dbtx.Prepare(query)
}

func (r *Logger) Query(query string, args ...any) (*sql.Rows, error) {
	r.l.Log(context.Background(), "Query", query, args...)

	return r.dbtx.Query(query, args...)
}

func (r *Logger) QueryRow(query string, args ...any) *sql.Row {
	r.l.Log(context.Background(), "QueryRow", query, args...)

	return r.dbtx.QueryRow(query, args...)
}

func (r *Logger) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	r.l.Log(ctx, "ExecContext", query, args...)

	return r.dbtx.ExecContext(ctx, query, args...)
}

func (r *Logger) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	r.l.Log(ctx, "PrepareContext", query)

	return r.dbtx.PrepareContext(ctx, query)
}

func (r *Logger) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	r.l.Log(ctx, "QueryContext", query, args...)

	return r.dbtx.QueryContext(ctx, query, args...)
}

func (r *Logger) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	r.l.Log(ctx, "QueryRowContext", query, args...)

	return r.dbtx.QueryRowContext(ctx, query, args...)
}
