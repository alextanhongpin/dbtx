package jsonb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
		var res Raw[T]
		err := rows.Scan(&res)
		if err != nil {
			return nil, err
		}

		result = append(result, res.value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

func (j *JSONB) QueryRowContext[T any](ctx context.Context, stmt string, args ...any) (T, error) {
	var zero T
	var res Raw[T]
	err := j.db.QueryRowContext(ctx, stmt, args...).Scan(&res)
	if err != nil {
		return zero, err
	}
	return res.value, nil
}

var _ sql.Scanner = (*Raw[any])(nil)

type Raw[T any] struct {
	value T
}

func (r *Raw[T]) Scan(src any) error {
	if src == nil {
		return nil
	}
	b, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("cannot scan type %T into Status", src)
	}

	return json.Unmarshal(b, &r.value)
}
