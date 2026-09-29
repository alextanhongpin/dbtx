// Package outbox implements the transactional outbox pattern on PostgreSQL.
//
// Messages are enqueued in the same transaction as the business writes that
// produce them, and later dispatched by workers that call Dequeue or Handle.
//
// Enqueue joins the caller's transaction when ctx carries one created by a
// dbtx.DB with the same ID as the repository (dbtx.ID by default). Otherwise
// the message is committed on its own.
//
// Dequeue and Handle hold a row lock on the message, inside a transaction, for
// the whole handler call. Keep handlers short: a slow broker means a
// long-running transaction and a held connection.
package outbox

import (
	_ "embed"

	"context"
	"errors"
	"math/rand/v2"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/outbox/internal"
)

//go:embed internal/schema.sql
var Schema string

var (
	ErrNotFound = internal.ErrNotFound
	ErrLocked   = internal.ErrLocked
	ErrEOQ      = internal.ErrEOQ
)

type (
	Message = internal.Message

	EnqueueParams = internal.CreateParams

	NackParams = internal.NackParams

	// PostgresRepository is the PostgreSQL implementation of Repository.
	PostgresRepository = internal.Repository
)

var NewRepository = internal.NewRepository

var _ Repository = (*PostgresRepository)(nil)

// Repository stores outbox messages.
type Repository interface {
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, params EnqueueParams) (*Message, error)
	Delete(ctx context.Context, id uuid.UUID) (*Message, error)
	Find(ctx context.Context, id uuid.UUID) (*Message, error)
	ListDead(ctx context.Context, limit int32) ([]*Message, error)
	Load(ctx context.Context) (*Message, error)
	Nack(ctx context.Context, params NackParams) (*Message, error)
	PurgeDead(ctx context.Context, before time.Time) (int64, error)
	Requeue(ctx context.Context, id uuid.UUID, maxRetry int32) (*Message, error)
	RunInSubTx(ctx context.Context, fn func(txCtx context.Context) error) error
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error
}

// HandlerFunc processes a message. The ctx carries the transaction that holds
// the message lock.
type HandlerFunc func(ctx context.Context, msg Message) error

type Outbox struct {
	repo Repository

	// Backoff returns how long a failed message stays hidden before it is
	// retried, unless the NackError sets Timeout.
	Backoff func(msg Message) time.Duration
}

func New(repo Repository) *Outbox {
	return &Outbox{
		repo:    repo,
		Backoff: DefaultBackoff,
	}
}

// DefaultBackoff doubles the delay on every retry, starting from 1s and capped
// at 1h, with jitter.
func DefaultBackoff(msg Message) time.Duration {
	d := min(time.Second<<min(max(msg.RetryCount, 0), 12), time.Hour)
	return d/2 + rand.N(d/2+1)
}

// Enqueue enqueues a new message to outbox.
func (o *Outbox) Enqueue(ctx context.Context, params EnqueueParams) (uuid.UUID, error) {
	msg, err := o.repo.Create(ctx, params)
	if err != nil {
		return uuid.Nil(), err
	}
	return msg.ID, nil
}

// Count returns the number of visible messages.
func (o *Outbox) Count(ctx context.Context) (int64, error) {
	return o.repo.Count(ctx)
}

// Dequeue locks the next visible message and passes it to fn.
//
// If fn returns nil, the message is deleted. If fn returns an error, writes
// made by fn are rolled back, the retry is recorded and the message is hidden
// until its backoff expires. Wrap the error with Nack to control the timeout,
// or to stop further retries.
//
// Errors from fn are returned as a *NackError. Any other error means the
// message was left untouched. It returns ErrEOQ if there are no visible
// messages.
func (o *Outbox) Dequeue(ctx context.Context, fn HandlerFunc) error {
	return o.process(ctx, o.repo.Load, fn)
}

// Handle is like Dequeue, but for the message with the given id. It ignores
// visibility and retry limits, so it can replay dead messages. It returns
// ErrNotFound if the message does not exist, and ErrLocked if another worker
// is processing it.
func (o *Outbox) Handle(ctx context.Context, id uuid.UUID, fn HandlerFunc) error {
	return o.process(ctx, func(ctx context.Context) (*Message, error) {
		return o.repo.Find(ctx, id)
	}, fn)
}

func (o *Outbox) process(ctx context.Context, load func(context.Context) (*Message, error), fn HandlerFunc) error {
	var fnErr error
	err := o.repo.RunInTx(ctx, func(txCtx context.Context) error {
		msg, err := load(txCtx)
		if err != nil {
			return err
		}

		// Run fn in a savepoint, so that its writes are discarded on failure
		// while the retry is still recorded.
		err = o.repo.RunInSubTx(txCtx, func(txCtx context.Context) error {
			fnErr = fn(txCtx, *msg)
			return fnErr
		})
		if fnErr == nil {
			if err != nil {
				return err
			}
			_, err = o.repo.Delete(txCtx, msg.ID)
			return err
		}

		nack, ok := errors.AsType[*NackError](fnErr)
		if !ok {
			nack = Nack(fnErr)
			fnErr = nack
		}

		_, err = o.repo.Nack(txCtx, NackParams{
			ID:        msg.ID,
			LastError: fnErr.Error(),
			Delay:     o.delay(nack, *msg),
			Dead:      nack.Skip,
		})
		return err
	})
	if err != nil {
		return err
	}

	return fnErr
}

func (o *Outbox) delay(nack *NackError, msg Message) time.Duration {
	if nack.Timeout > 0 {
		return nack.Timeout
	}
	if o.Backoff != nil {
		return o.Backoff(msg)
	}
	return DefaultBackoff(msg)
}

// DeadLetters returns up to limit messages that are no longer retryable,
// either because they ran out of retries or were nacked with Skip.
func (o *Outbox) DeadLetters(ctx context.Context, limit int32) ([]*Message, error) {
	return o.repo.ListDead(ctx, limit)
}

// Requeue makes a dead message visible again, with retry count reset and the
// given max retry. It returns ErrNotFound if the message is not dead.
func (o *Outbox) Requeue(ctx context.Context, id uuid.UUID, maxRetry int32) (*Message, error) {
	return o.repo.Requeue(ctx, id, maxRetry)
}

// PurgeDead deletes dead messages last updated before t, and returns the
// number of messages deleted. Call it periodically to keep the table small.
func (o *Outbox) PurgeDead(ctx context.Context, before time.Time) (int64, error) {
	return o.repo.PurgeDead(ctx, before)
}

// NackError is returned by a handler to control how a failed message is
// retried.
type NackError struct {
	Cause error
	// Skip stops further retries, emulating a dead letter queue.
	Skip bool
	// Timeout is how long until the message is visible again. Zero means
	// Outbox.Backoff is used.
	Timeout time.Duration
}

func Nack(err error) *NackError {
	return &NackError{
		Cause: err,
	}
}

func (n *NackError) Error() string {
	if n.Cause == nil {
		return "outbox: nack"
	}
	return n.Cause.Error()
}

func (n *NackError) Unwrap() error {
	return n.Cause
}
