package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/idempotent/internal/postgres"
)

var (
	ErrNotFound        = errors.New("idempotency key not found")
	ErrRequestConflict = errors.New("request conflict")
)

type IdempotencyKey struct {
	Scope     string
	Key       string
	Request   json.RawMessage
	Response  json.RawMessage
	CreatedAt time.Time
}

type Repository struct {
	*dbtx.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{
		dbtx.New(db),
	}
}

func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}

func (r *Repository) Delete(ctx context.Context, scope, key string) (*IdempotencyKey, error) {
	row, err := r.db(ctx).Delete(ctx, postgres.DeleteParams{
		Scope: scope,
		Key:   key,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	return newIdempotencyKey(row), nil
}

// DeleteBefore removes all idempotency keys created before t, and returns the
// number of keys removed.
func (r *Repository) DeleteBefore(ctx context.Context, t time.Time) (int64, error) {
	return r.db(ctx).DeleteBefore(ctx, t)
}

func (r *Repository) Update(ctx context.Context, scope, key string, res json.RawMessage) error {
	return r.db(ctx).Update(ctx, postgres.UpdateParams{
		Response: res,
		Scope:    scope,
		Key:      key,
	})
}

// The row is new if it was inserted by the current transaction. xmin is a
// 32-bit xid, so the 64-bit pg_current_xact_id() must be cast down before
// comparing, otherwise the comparison fails once the xid epoch is non-zero.
//
// The request is compared using jsonb equality, which is exact for numbers and
// ignores key order and whitespace.
const loadOrStoreStmt = `insert into dbtx.idempotency_keys(scope, key, request, response)
values ($1, $2, $3, 'null')
on conflict (scope, key) do select
returning scope, key, request, response, created_at,
  xmin = pg_current_xact_id()::xid as is_new,
  request = $3::jsonb as is_match`

// LoadOrStore inserts the scoped key with the given request, or loads the existing
// row. It returns ErrRequestConflict if the existing row was stored with a
// different request.
func (r *Repository) LoadOrStore(ctx context.Context, scope, key string, req json.RawMessage) (*IdempotencyKey, bool, error) {
	var row postgres.DbtxIdempotencyKey
	var isNew, isMatch bool
	err := r.DBTx(ctx).QueryRowContext(ctx, loadOrStoreStmt, scope, key, req).Scan(&row.Scope, &row.Key, &row.Request, &row.Response, &row.CreatedAt, &isNew, &isMatch)
	if err != nil {
		return nil, false, err
	}
	if !isMatch {
		return nil, false, ErrRequestConflict
	}

	return newIdempotencyKey(&row), !isNew, nil
}

func newIdempotencyKey(row *postgres.DbtxIdempotencyKey) *IdempotencyKey {
	return &IdempotencyKey{
		Scope:     row.Scope,
		Key:       row.Key,
		Request:   row.Request,
		Response:  row.Response,
		CreatedAt: row.CreatedAt,
	}
}
