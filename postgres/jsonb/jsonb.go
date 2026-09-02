package jsonb

import (
	"context"
	"database/sql"
	"encoding/json"
)

type JSONB struct {
	db *sql.DB
}

func New(db *sql.DB) *JSONB {
	return &JSONB{
		db: db,
	}
}

func (j *JSONB) QueryJSONContext[T any](ctx context.Context, stmt string, args ...any) (T, error) {
	var zero T
	var res json.RawMessage
	err := j.db.QueryRowContext(ctx, stmt, args...).Scan(&res)
	if err != nil {
		return zero, err
	}
	var v T
	err = json.Unmarshal(res, &v)
	if err != nil {
		return zero, err
	}
	return v, nil
}
