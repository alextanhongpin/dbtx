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

type IdempotencyKey struct {
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

func (r *Repository) Delete(ctx context.Context, key string) (*IdempotencyKey, error) {
	row, err := r.db(ctx).Delete(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return newIdempotencyKey(row), nil
}

func (r *Repository) Update(ctx context.Context, key string, req, res json.RawMessage) error {
	return r.db(ctx).Update(ctx, postgres.UpdateParams{
		Request:  req,
		Response: res,
		Key:      key,
	})
}

const insertStmt = `insert into dbtx.idempotency_keys(key, request, response)
values ($1, '{}', '{}')
on conflict (key) do select
returning key, request, response, created_at, (xmin::text = txid_current()::text) AS is_new`

func (r *Repository) LoadOrStore(ctx context.Context, key string) (*IdempotencyKey, bool, error) {
	var row postgres.DbtxIdempotencyKey
	var stored bool
	err := r.DBTx(ctx).QueryRowContext(ctx, insertStmt, key).Scan(&row.Key, &row.Request, &row.Response, &row.CreatedAt, &stored)
	if err != nil {
		return nil, false, err
	}

	return newIdempotencyKey(&row), !stored, nil
}

func newIdempotencyKey(row *postgres.DbtxIdempotencyKey) *IdempotencyKey {
	return &IdempotencyKey{
		Key:       row.Key,
		Request:   row.Request,
		Response:  row.Response,
		CreatedAt: row.CreatedAt,
	}
}
