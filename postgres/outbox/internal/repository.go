package internal

import (
	"context"
	"database/sql"
	"errors"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox/internal/postgres"
)

var (
	ErrNotFound = errors.New("outbox: not found")
	ErrEOQ      = errors.New("outbox: end of queue")
)

type Repository struct {
	*dbtx.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{
		DB: dbtx.New(db),
	}
}

type Message = postgres.DbtxOutbox

type CreateParams = postgres.CreateParams

type UpdateParams = postgres.UpdateParams

func (r *Repository) Count(ctx context.Context) (int64, error) {
	return r.db(ctx).Count(ctx)
}

func (r *Repository) Create(ctx context.Context, params postgres.CreateParams) (*postgres.DbtxOutbox, error) {
	return r.db(ctx).Create(ctx, params)
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID) (*postgres.DbtxOutbox, error) {
	return r.db(ctx).Delete(ctx, id)
}

func (r *Repository) Find(ctx context.Context, id uuid.UUID) (*postgres.DbtxOutbox, error) {
	row, err := r.db(ctx).Find(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

func (r *Repository) Load(ctx context.Context) (*postgres.DbtxOutbox, error) {
	row, err := r.db(ctx).Load(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEOQ
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

func (r *Repository) Update(ctx context.Context, params postgres.UpdateParams) (*postgres.DbtxOutbox, error) {
	return r.db(ctx).Update(ctx, params)
}

// Count returns the number of visible messages.
func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}
