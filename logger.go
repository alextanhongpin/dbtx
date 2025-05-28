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

func WithLogger(l logger) func(DBTX) DBTX {
	return func(dbtx DBTX) DBTX {
		return NewLogger(dbtx, l)
	}
}

func NewLogger(dbtx DBTX, l logger) *Logger {
	return &Logger{dbtx: dbtx, l: l}
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
