// Package outbox implements the transactional outbox pattern on PostgreSQL.
//
// Messages are enqueued in the same transaction as the business writes that
// produce them, and later dispatched by workers that call Dequeue.
//
// Enqueue joins the caller's transaction when ctx carries one created by a
// dbtx.DB with the same ID as the repository (dbtx.ID by default). Otherwise
// the message is committed on its own.
//
// Dequeue leases messages to the worker instead of holding row locks, so no
// transaction stays open while messages wait to be handled. A message whose
// lease expires, for example because the worker crashed, is delivered again.
// Delivery is at least once, so handlers must be idempotent.
package outbox

import (
	_ "embed"

	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/outbox/internal"
)

//go:embed internal/schema.sql
var Schema string

var (
	ErrNotFound     = internal.ErrNotFound
	ErrEOQ          = internal.ErrEOQ
	ErrLeaseExpired = internal.ErrLeaseExpired
	ErrDeadLetter   = errors.New("outbox: dead letter")
)

// Statuses of a message.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusDone       = "done"
	StatusDead       = "dead"
)

// DefaultLease is how long Dequeue leases messages when Outbox.Lease is zero.
const DefaultLease = 30 * time.Second

type (
	Message = internal.Message

	EnqueueParams = internal.CreateParams

	// PostgresRepository is the PostgreSQL implementation of Repository.
	PostgresRepository = internal.Repository
)

var NewRepository = internal.NewRepository

var _ Repository = (*PostgresRepository)(nil)

// Repository stores outbox messages.
type Repository interface {
	Ack(ctx context.Context, id uuid.UUID, lockedBy string) error
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, params EnqueueParams) (uuid.UUID, error)
	Dead(ctx context.Context) (int64, error)
	ListDead(ctx context.Context, limit int32) ([]*Message, error)
	Nack(ctx context.Context, id uuid.UUID, lockedBy string, dead bool, lastError string, delay time.Duration) error
	Poll(ctx context.Context, lockedBy string, limit int32, lease time.Duration) ([]*Message, error)
	Purge(ctx context.Context, status string, before time.Time) (int64, error)
	Requeue(ctx context.Context, id uuid.UUID) error
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error
}

// HandlerFunc processes a message. The ctx carries the transaction that
// acknowledges the message.
type HandlerFunc = func(ctx context.Context, msg *Message) error

type Outbox struct {
	repo Repository

	// Backoff returns how long a failed message stays hidden before it is
	// retried. Defaults to DefaultBackoff.
	Backoff func(msg *Message) time.Duration

	// Lease is how long dequeued messages stay hidden from other workers.
	// All messages in a batch are leased at once, so it must cover handling
	// the whole batch. Defaults to DefaultLease.
	Lease time.Duration
}

// DefaultBackoff doubles the delay on every attempt, starting from 1s and
// capped at 1h, with jitter.
func DefaultBackoff(msg *Message) time.Duration {
	d := min(time.Second<<min(max(msg.Attempts-1, 0), 12), time.Hour)
	return d/2 + rand.N(d/2+1)
}

func New(repo Repository) *Outbox {
	return &Outbox{
		repo: repo,
	}
}

// Enqueue enqueues a new message to outbox.
func (o *Outbox) Enqueue(ctx context.Context, params EnqueueParams) (uuid.UUID, error) {
	return o.repo.Create(ctx, params)
}

// Dequeue leases up to limit visible messages and passes each to fn, in its
// own transaction.
//
// If fn returns nil, the message is marked as done in the same transaction as
// the writes made by fn. If fn returns an error, the writes made by fn are
// rolled back, and the message is hidden until its backoff expires. The
// message is dead once it runs out of attempts, or when the error wraps
// ErrDeadLetter.
//
// Errors from fn are recorded on the message, and not returned. Any error
// returned means the affected messages are retried once their lease expires.
// It returns ErrEOQ if there are no visible messages.
func (o *Outbox) Dequeue(ctx context.Context, limit int32, fn HandlerFunc) error {
	lockedBy := uuid.NewV7().String()

	// Messages that ran out of attempts are skipped by Poll, so the ones
	// whose final lease expired must be marked as dead here.
	if _, err := o.repo.Dead(ctx); err != nil {
		return err
	}

	msgs, err := o.repo.Poll(ctx, lockedBy, max(limit, 1), o.lease())
	if err != nil {
		return err
	}

	var errs []error
	for _, msg := range msgs {
		if err := o.handle(ctx, lockedBy, msg, fn); err != nil {
			errs = append(errs, fmt.Errorf("outbox: message %s: %w", msg.ID, err))
		}
	}

	return errors.Join(errs...)
}

func (o *Outbox) handle(ctx context.Context, lockedBy string, msg *Message, fn HandlerFunc) error {
	var handlerErr error
	err := o.repo.RunInTx(ctx, func(txCtx context.Context) error {
		if err := fn(txCtx, msg); err != nil {
			handlerErr = err
			return err
		}

		// Roll back the writes made by fn if another worker owns the
		// message now.
		return o.repo.Ack(txCtx, msg.ID, lockedBy)
	})
	if handlerErr == nil {
		return err
	}

	dead := msg.Attempts >= msg.MaxAttempts || errors.Is(handlerErr, ErrDeadLetter)
	return o.repo.Nack(ctx, msg.ID, lockedBy, dead, handlerErr.Error(), o.backoff(msg))
}

func (o *Outbox) backoff(msg *Message) time.Duration {
	if o.Backoff != nil {
		return o.Backoff(msg)
	}
	return DefaultBackoff(msg)
}

func (o *Outbox) lease() time.Duration {
	if o.Lease > 0 {
		return o.Lease
	}
	return DefaultLease
}

// Count returns the number of visible messages.
func (o *Outbox) Count(ctx context.Context) (int64, error) {
	return o.repo.Count(ctx)
}

// DeadLetters returns up to limit dead messages, oldest first.
func (o *Outbox) DeadLetters(ctx context.Context, limit int32) ([]*Message, error) {
	return o.repo.ListDead(ctx, limit)
}

// Requeue makes a dead message visible again, with its attempts reset. It
// returns ErrNotFound if the message is not dead.
func (o *Outbox) Requeue(ctx context.Context, id uuid.UUID) error {
	return o.repo.Requeue(ctx, id)
}

// Purge deletes messages with the given status, StatusDone or StatusDead, last
// updated before t, and returns the number of messages deleted. Call it
// periodically to keep the table small.
func (o *Outbox) Purge(ctx context.Context, status string, before time.Time) (int64, error) {
	switch status {
	case StatusDone, StatusDead:
		return o.repo.Purge(ctx, status, before)
	default:
		return 0, fmt.Errorf("outbox: cannot purge %q messages", status)
	}
}
