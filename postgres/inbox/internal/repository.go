package internal

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/inbox/internal/postgres"
)

type Message struct {
	ID          uuid.UUID
	Source      string
	MessageID   string
	MessageType string
	AggregateID string
	Payload     jsontext.Value
	Status      string
	Attempts    int32
	MaxAttempts int32
	LastError   string
	LockedBy    string
	AvailableAt time.Time
	ReceivedAt  time.Time
	ProcessedAt time.Time
	UpdatedAt   time.Time
}

var (
	ErrEOQ       = errors.New("inbox: end of queue")
	ErrExists    = errors.New("inbox: message exists")
	ErrLeaseLost = errors.New("inbox: lease lost")
	ErrNotFound  = errors.New("inbox: not found")
)

type Repository struct {
	*dbtx.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{
		DB: dbtx.New(db),
	}
}

type AckParams = postgres.AckParams

// Ack marks the message as done. It returns ErrLeaseLost if the message is no
// longer leased by params.LockedBy.
func (r *Repository) Ack(ctx context.Context, params AckParams) error {
	n, err := r.db(ctx).Ack(ctx, params)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

type ClaimParams = postgres.ClaimParams

func (r *Repository) Count(ctx context.Context) (int64, error) {
	return r.db(ctx).Count(ctx)
}

func (r *Repository) Claim(ctx context.Context, params ClaimParams) ([]*Message, error) {
	res, err := r.db(ctx).Claim(ctx, params)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, ErrEOQ
	}
	return newMessages(res), nil
}

type ClaimByAggregateIDParams = postgres.ClaimByAggregateIDParams

func (r *Repository) ClaimByAggregateID(ctx context.Context, params ClaimByAggregateIDParams) ([]*Message, error) {
	res, err := r.db(ctx).ClaimByAggregateID(ctx, params)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, ErrEOQ
	}
	return newMessages(res), nil
}

type CreateParams struct {
	Source      string
	MessageID   string
	MessageType string
	Payload     jsontext.Value
	AggregateID string
	AvailableAt time.Time
	MaxAttempts int32
}

func (r *Repository) Create(ctx context.Context, params CreateParams) (uuid.UUID, error) {
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
		return uuid.Nil(), ErrExists
	}
	return id, err
}

// ExpireLeases marks messages whose lease expired on their final attempt as
// dead.
func (r *Repository) ExpireLeases(ctx context.Context) error {
	return r.db(ctx).ExpireLeases(ctx)
}

func (r *Repository) Find(ctx context.Context, id uuid.UUID) (*Message, error) {
	row, err := r.db(ctx).Find(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return newMessage(row), nil
}

type NackParams = postgres.NackParams

func (r *Repository) Nack(ctx context.Context, params NackParams) error {
	return r.db(ctx).Nack(ctx, params)
}

func (r *Repository) ListDead(ctx context.Context, limit int32) ([]*Message, error) {
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
		return ErrNotFound
	}
	return nil
}

func (r *Repository) db(ctx context.Context) postgres.Querier {
	return postgres.New(r.DBTx(ctx))
}

func newMessages(msgs []*postgres.DbtxInbox) []*Message {
	res := make([]*Message, len(msgs))
	for i, msg := range msgs {
		res[i] = newMessage(msg)
	}
	return res
}

func newMessage(msg *postgres.DbtxInbox) *Message {
	return &Message{
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
