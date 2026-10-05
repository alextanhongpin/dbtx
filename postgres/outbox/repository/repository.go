package repository

import (
	_ "embed"

	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox"
	"github.com/alextanhongpin/dbtx/postgres/outbox/repository/postgres"
)

//go:embed schema.sql
var Schema string

var _ outbox.Repository = (*Repository)(nil)

type Repository struct {
	*dbtx.DB
}

func New(db *sql.DB) *Repository {
	return &Repository{
		DB: dbtx.New(db),
	}
}

// Ack marks a leased message as done. It returns ErrLeaseExpired if the
// message is no longer leased by lockedBy.
func (r *Repository) Ack(ctx context.Context, id uuid.UUID, lockedBy string) error {
	n, err := r.db(ctx).Ack(ctx, postgres.AckParams{
		ID:       id,
		LockedBy: newNullString(lockedBy),
	})
	return leased(n, err)
}

// Lock renews the lease of a message leased to lockedBy, and locks its row
// until the transaction in ctx ends. It returns ErrLeaseExpired if the message
// is no longer leased by lockedBy.
func (r *Repository) Lock(ctx context.Context, id uuid.UUID, lockedBy string, lease time.Duration) error {
	n, err := r.db(ctx).Lock(ctx, postgres.LockParams{
		ID:           id,
		LockedBy:     lockedBy,
		LeaseSeconds: lease.Seconds(),
	})
	return leased(n, err)
}

// Release returns a leased message that was never handled, and refunds its
// attempt. It returns ErrLeaseExpired if the message is no longer leased by
// lockedBy.
func (r *Repository) Release(ctx context.Context, id uuid.UUID, lockedBy string) error {
	n, err := r.db(ctx).Release(ctx, postgres.ReleaseParams{
		ID:       id,
		LockedBy: lockedBy,
	})
	return leased(n, err)
}

// Count returns the number of visible messages.
func (r *Repository) Count(ctx context.Context) (int64, error) {
	return r.db(ctx).Count(ctx)
}

func (r *Repository) Create(ctx context.Context, params outbox.EnqueueParams) (uuid.UUID, error) {
	return r.db(ctx).Create(ctx, postgres.CreateParams{
		AggregateID:   params.AggregateID,
		AggregateType: params.AggregateType,
		EventType:     params.EventType,
		Payload:       []byte(params.Payload),
		MaxAttempts: sql.NullInt32{
			Int32: params.MaxAttempts,
			Valid: params.MaxAttempts != 0,
		},
		AvailableAt: sql.NullTime{
			Time:  params.AvailableAt,
			Valid: !params.AvailableAt.IsZero(),
		},
	})
}

// Dead marks messages whose lease expired on their final attempt as dead, and
// returns the number of messages marked.
func (r *Repository) Dead(ctx context.Context) (int64, error) {
	return r.db(ctx).Dead(ctx)
}

func (r *Repository) Find(ctx context.Context, id uuid.UUID) (*outbox.Message, error) {
	row, err := r.db(ctx).Find(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, outbox.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	return newMessage(row), nil
}

func (r *Repository) ListDead(ctx context.Context, limit int32) ([]*outbox.Message, error) {
	rows, err := r.db(ctx).ListDead(ctx, limit)
	if err != nil {
		return nil, err
	}
	return newMessages(rows), nil
}

// Nack releases a leased message, and hides it for delay. If dead is true, the
// message is not retried. It returns ErrLeaseExpired if the message is no
// longer leased by lockedBy.
func (r *Repository) Nack(ctx context.Context, id uuid.UUID, lockedBy string, dead bool, lastError string, delay time.Duration) error {
	n, err := r.db(ctx).Nack(ctx, postgres.NackParams{
		ID:           id,
		LockedBy:     newNullString(lockedBy),
		Dead:         dead,
		LastError:    newNullString(lastError),
		DelaySeconds: delay.Seconds(),
	})
	return leased(n, err)
}

// Poll leases up to limit visible messages to lockedBy for the lease duration.
// It returns ErrEOQ if there are no visible messages.
func (r *Repository) Poll(ctx context.Context, lockedBy string, limit int32, lease time.Duration) ([]*outbox.Message, error) {
	rows, err := r.db(ctx).Poll(ctx, postgres.PollParams{
		LockedBy:     newNullString(lockedBy),
		LeaseSeconds: lease.Seconds(),
		BatchSize:    limit,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, outbox.ErrEOQ
	}
	return newMessages(rows), nil
}

func (r *Repository) Purge(ctx context.Context, status string, before time.Time) (int64, error) {
	return r.db(ctx).Purge(ctx, postgres.PurgeParams{
		Status:    postgres.DbtxOutboxStatus(status),
		UpdatedAt: before,
	})
}

func (r *Repository) Requeue(ctx context.Context, id uuid.UUID) error {
	n, err := r.db(ctx).Requeue(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return outbox.ErrNotFound
	}
	return nil
}

func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}

func leased(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return outbox.ErrLeaseExpired
	}
	return nil
}

func newNullString(s string) sql.NullString {
	return sql.NullString{
		String: s,
		Valid:  s != "",
	}
}

func newMessages(rows []*postgres.DbtxOutbox) []*outbox.Message {
	res := make([]*outbox.Message, len(rows))
	for i, row := range rows {
		res[i] = newMessage(row)
	}
	return res
}

func newMessage(row *postgres.DbtxOutbox) *outbox.Message {
	return &outbox.Message{
		ID:            row.ID,
		AggregateType: row.AggregateType,
		AggregateID:   row.AggregateID,
		EventType:     row.EventType,
		Payload:       jsontext.Value(row.Payload),
		Status:        string(row.Status),
		Attempts:      row.Attempts,
		MaxAttempts:   row.MaxAttempts,
		LastError:     row.LastError.String,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
}
