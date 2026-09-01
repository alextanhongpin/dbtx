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

func (j *JSONB) Query[T any](ctx context.Context, stmt string, data any) (T, error) {
	var zero T
	b, err := json.Marshal(data)
	if err != nil {
		return zero, err
	}
	var res json.RawMessage
	err = j.db.QueryRowContext(ctx, stmt, b).Scan(&res)
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
