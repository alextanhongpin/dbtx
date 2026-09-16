package jsonb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"github.com/alextanhongpin/dbtx/postgres/jsonb/internal"
)

type JSONB struct {
	*sql.DB
	valid sync.Map
}

func New(db *sql.DB) *JSONB {
	return &JSONB{
		DB: db,
	}
}

func (j *JSONB) QueryContext[T any](ctx context.Context, stmt string, args ...any) ([]T, error) {
	rows, err := j.DB.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var once bool
	var result []T
	for rows.Next() {
		var res JSON[T]
		err := rows.Scan(&res)
		if err != nil {
			return nil, err
		}

		if !once {
			once = true

			typ := reflect.TypeFor[T]()
			_, loaded := j.valid.LoadOrStore(validateKey{
				typ:  typ,
				stmt: stmt,
			}, true)
			res.validated = loaded

			if res.difference.Different() {
				slog.WarnContext(ctx, "dbtx/postgres/jsonb: columns mismatch", "missing", res.difference.ExtraInMap, "extra", res.difference.MissingInMap, "type", typ)
			}
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
	var res JSON[T]

	typ := reflect.TypeFor[T]()
	_, loaded := j.valid.LoadOrStore(validateKey{
		typ:  typ,
		stmt: stmt,
	}, true)
	res.validated = loaded

	err := j.DB.QueryRowContext(ctx, stmt, args...).Scan(&res)
	if err != nil {
		return zero, err
	}
	if res.difference.Different() {
		slog.WarnContext(ctx, "dbtx/postgres/jsonb: columns mismatch", "missing", res.difference.ExtraInMap, "extra", res.difference.MissingInMap, "type", typ)
	}

	return res.value, nil
}

var _ sql.Scanner = (*JSON[any])(nil)

type JSON[T any] struct {
	difference internal.Difference
	tagName    string
	validated  bool
	value      T
}

func (j *JSON[T]) Scan(src any) error {
	if src == nil {
		return nil
	}
	b, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("cannot scan type %T into Status", src)
	}

	if !j.validated {
		var a any
		err := json.Unmarshal(b, &a)
		if err != nil {
			return err
		}
		var zero T
		j.difference = internal.CompareStructAndMap(zero, a)
	}

	return json.Unmarshal(b, &j.value)
}

type validateKey struct {
	typ  reflect.Type
	stmt string
}
