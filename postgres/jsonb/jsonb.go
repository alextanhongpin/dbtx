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

func (j *JSONB) QueryContext[T any](ctx context.Context, stmt string, args ...any) ([]T, error) {
	rows, err := j.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []T
	for rows.Next() {
		var res json.RawMessage
		err := rows.Scan(&res)
		if err != nil {
			return nil, err
		}

		var v T
		err = json.Unmarshal(res, &v)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

func (j *JSONB) QueryRowContext[T any](ctx context.Context, stmt string, args ...any) (T, error) {
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
