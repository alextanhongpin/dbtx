package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox/internal/postgres"
)

var (
	ErrNotFound = errors.New("outbox: not found")
	ErrLocked   = errors.New("outbox: locked")
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

type CreateParams struct {
	AggregateID   string
	AggregateType string
	Type          string
	Payload       json.RawMessage
	// MaxRetry is the number of attempts before the message is dead. Zero
	// means unlimited.
	MaxRetry int32
	// VisibleAt delays the message until the given time. Nil means now.
	VisibleAt *time.Time
}

type NackParams struct {
	ID        uuid.UUID
	LastError string
	// Delay is how long until the message is visible again.
	Delay time.Duration
	// Dead stops the message from being retried.
	Dead bool
}

// Count returns the number of visible messages.
func (r *Repository) Count(ctx context.Context) (int64, error) {
	return r.db(ctx).Count(ctx)
}

func (r *Repository) Create(ctx context.Context, params CreateParams) (*Message, error) {
	var visibleAt sql.NullTime
	if params.VisibleAt != nil {
		visibleAt = sql.NullTime{Time: *params.VisibleAt, Valid: true}
	}
	return r.db(ctx).Create(ctx, postgres.CreateParams{
		AggregateID:   params.AggregateID,
		AggregateType: params.AggregateType,
		Type:          params.Type,
		Payload:       params.Payload,
		MaxRetry:      params.MaxRetry,
		VisibleAt:     visibleAt,
	})
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID) (*Message, error) {
	return notFound(r.db(ctx).Delete(ctx, id))
}

// Find locks the message with the given id. It returns ErrLocked if another
// transaction holds the lock, and ErrNotFound if the message does not exist.
func (r *Repository) Find(ctx context.Context, id uuid.UUID) (*Message, error) {
	q := r.db(ctx)
	row, err := q.Find(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		// SKIP LOCKED returns no rows for locked messages too, so check
		// whether the message exists at all.
		ok, err := q.Exists(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			return nil, ErrLocked
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

func (r *Repository) Load(ctx context.Context) (*Message, error) {
	row, err := r.db(ctx).Load(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEOQ
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

func (r *Repository) Nack(ctx context.Context, params NackParams) (*Message, error) {
	return notFound(r.db(ctx).Nack(ctx, postgres.NackParams{
		ID:        params.ID,
		LastError: params.LastError,
		DelayUs:   params.Delay.Microseconds(),
		Dead:      params.Dead,
	}))
}

// ListDead returns up to limit messages that are no longer retryable, oldest
// first.
func (r *Repository) ListDead(ctx context.Context, limit int32) ([]*Message, error) {
	return r.db(ctx).ListDead(ctx, limit)
}

// Requeue makes a dead message visible again with a fresh retry budget. It
// returns ErrNotFound if the message does not exist or is not dead.
func (r *Repository) Requeue(ctx context.Context, id uuid.UUID, maxRetry int32) (*Message, error) {
	return notFound(r.db(ctx).Requeue(ctx, postgres.RequeueParams{
		ID:       id,
		MaxRetry: maxRetry,
	}))
}

// PurgeDead deletes dead messages last updated before t, and returns the
// number of messages deleted.
func (r *Repository) PurgeDead(ctx context.Context, before time.Time) (int64, error) {
	return r.db(ctx).PurgeDead(ctx, before)
}

func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}

func notFound(row *Message, err error) (*Message, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}
