package repository

import (
	_ "embed"

	"context"
	"database/sql"
	"errors"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/inbox"
	"github.com/alextanhongpin/dbtx/postgres/inbox/repository/postgres"
)

//go:embed schema.sql
var Schema string

var _ inbox.Repository = (*Repository)(nil)

type Repository struct {
	*dbtx.DB
}

func New(db *sql.DB) *Repository {
	return &Repository{
		DB: dbtx.New(db),
	}
}

// Ack marks the message as done. It returns inbox.ErrLeaseLost if the message
// is no longer leased by params.LockedBy.
func (r *Repository) Ack(ctx context.Context, params inbox.AckParams) error {
	n, err := r.db(ctx).Ack(ctx, postgres.AckParams{
		ID:       params.ID,
		LockedBy: params.LockedBy,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return inbox.ErrLeaseLost
	}
	return nil
}

func (r *Repository) Count(ctx context.Context) (int64, error) {
	return r.db(ctx).Count(ctx)
}

func (r *Repository) Claim(ctx context.Context, params inbox.ClaimParams) ([]*inbox.Message, error) {
	res, err := r.db(ctx).Claim(ctx, postgres.ClaimParams{
		Limit:        params.Limit,
		LockedBy:     params.LockedBy,
		LeaseSeconds: params.LeaseSeconds,
	})
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, inbox.ErrEOQ
	}
	return newMessages(res), nil
}

func (r *Repository) ClaimByAggregateID(ctx context.Context, params inbox.ClaimByAggregateIDParams) ([]*inbox.Message, error) {
	res, err := r.db(ctx).ClaimByAggregateID(ctx, postgres.ClaimByAggregateIDParams{
		Limit:        params.Limit,
		LockedBy:     params.LockedBy,
		LeaseSeconds: params.LeaseSeconds,
	})
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, inbox.ErrEOQ
	}
	return newMessages(res), nil
}

func (r *Repository) Create(ctx context.Context, params inbox.CreateParams) (uuid.UUID, error) {
	id, err := r.db(ctx).Create(ctx, postgres.CreateParams{
		Source:      params.Source,
		MessageID:   params.MessageID,
		MessageType: params.MessageType,
		Payload:     params.Payload,
		AggregateID: params.AggregateID,
		AvailableAt: sql.NullTime{
			Time:  params.AvailableAt,
			Valid: !params.AvailableAt.IsZero(),
		},
		MaxAttempts: sql.NullInt32{
			Int32: params.MaxAttempts,
			Valid: params.MaxAttempts != 0,
		},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil(), inbox.ErrExists
	}
	return id, err
}

// ExpireLeases marks messages whose lease expired on their final attempt as
// dead.
func (r *Repository) ExpireLeases(ctx context.Context) error {
	return r.db(ctx).ExpireLeases(ctx)
}

func (r *Repository) Find(ctx context.Context, id uuid.UUID) (*inbox.Message, error) {
	row, err := r.db(ctx).Find(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, inbox.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return newMessage(row), nil
}

// Nack releases the message, and hides it for params.DelaySeconds. It returns
// inbox.ErrLeaseLost if the message is no longer leased by params.LockedBy.
func (r *Repository) Nack(ctx context.Context, params inbox.NackParams) error {
	n, err := r.db(ctx).Nack(ctx, postgres.NackParams{
		ID:           params.ID,
		Dead:         params.Dead,
		DelaySeconds: params.DelaySeconds,
		LastError:    params.LastError,
		LockedBy:     params.LockedBy,
	})
	return leased(n, err)
}

// Lock renews the lease of a message leased to params.LockedBy, and locks its
// row until the transaction in ctx ends. It returns inbox.ErrLeaseLost if the
// message is no longer leased by params.LockedBy.
func (r *Repository) Lock(ctx context.Context, params inbox.LockParams) error {
	n, err := r.db(ctx).Lock(ctx, postgres.LockParams{
		ID:           params.ID,
		LockedBy:     params.LockedBy,
		LeaseSeconds: params.LeaseSeconds,
	})
	return leased(n, err)
}

// LockAggregate serialises the handlers of an aggregate until the transaction
// in ctx ends.
func (r *Repository) LockAggregate(ctx context.Context, aggregateID string) error {
	return r.db(ctx).LockAggregate(ctx, aggregateID)
}

// Release returns a leased message that was never handled, and refunds its
// attempt. It returns inbox.ErrLeaseLost if the message is no longer leased by
// params.LockedBy.
func (r *Repository) Release(ctx context.Context, params inbox.ReleaseParams) error {
	n, err := r.db(ctx).Release(ctx, postgres.ReleaseParams{
		ID:       params.ID,
		LockedBy: params.LockedBy,
	})
	return leased(n, err)
}

func leased(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return inbox.ErrLeaseLost
	}
	return nil
}

func (r *Repository) ListDead(ctx context.Context, limit int32) ([]*inbox.Message, error) {
	rows, err := r.db(ctx).ListDead(ctx, limit)
	if err != nil {
		return nil, err
	}
	return newMessages(rows), nil
}

func (r *Repository) Purge(ctx context.Context, status string, updatedAt time.Time) (int64, error) {
	return r.db(ctx).Purge(ctx, postgres.PurgeParams{
		Status:    postgres.DbtxInboxStatus(status),
		UpdatedAt: updatedAt,
	})
}

func (r *Repository) Requeue(ctx context.Context, id uuid.UUID) error {
	n, err := r.db(ctx).Requeue(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return inbox.ErrNotFound
	}
	return nil
}

func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}

func newMessages(msgs []*postgres.DbtxInbox) []*inbox.Message {
	res := make([]*inbox.Message, len(msgs))
	for i, msg := range msgs {
		res[i] = newMessage(msg)
	}
	return res
}

func newMessage(msg *postgres.DbtxInbox) *inbox.Message {
	return &inbox.Message{
		ID:          msg.ID,
		Source:      msg.Source,
		MessageID:   msg.MessageID,
		MessageType: msg.MessageType,
		AggregateID: msg.AggregateID.String,
		Payload:     msg.Payload,
		Status:      string(msg.Status),
		Attempts:    msg.Attempts,
		MaxAttempts: msg.MaxAttempts,
		LastError:   msg.LastError.String,
		LockedBy:    msg.LockedBy.String,
		AvailableAt: msg.AvailableAt,
		ReceivedAt:  msg.ReceivedAt,
		ProcessedAt: msg.ProcessedAt.Time,
		UpdatedAt:   msg.UpdatedAt,
	}
}
