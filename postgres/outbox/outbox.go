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
//
// Delivery order is not guaranteed, not even for messages of the same
// aggregate: workers publish concurrently, failed messages are retried behind
// later ones, and poll order follows available_at, which defaults to the start
// of the enqueuing transaction rather than its commit. Consumers must tolerate
// reordering, for example by versioning the events of an aggregate.
package outbox

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
	"uuid"
)

var (
	ErrDeadLetter         = errors.New("outbox: dead letter")
	ErrEOQ                = errors.New("outbox: end of queue")
	ErrInvalidMaxAttempts = errors.New("outbox: max attempts must not be negative")
	ErrLeaseExpired       = errors.New("outbox: lease expired")
	ErrNotFound           = errors.New("outbox: not found")
	ErrTxInContext        = errors.New("outbox: ctx must not carry a transaction")
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

// releaseTimeout bounds releasing unhandled messages after ctx is done.
const releaseTimeout = 5 * time.Second

type Message struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       jsontext.Value
	Status        string
	Attempts      int32
	MaxAttempts   int32
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type EnqueueParams struct {
	AggregateID   string
	AggregateType string
	EventType     string
	Payload       jsontext.Value

	// MaxAttempts is the number of deliveries before the message is dead.
	// Zero means the default of 10.
	MaxAttempts int32

	// AvailableAt delays the first delivery. Zero means now.
	AvailableAt time.Time
}

// Repository stores outbox messages.
type Repository interface {
	Ack(ctx context.Context, id uuid.UUID, lockedBy string) error
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, params EnqueueParams) (uuid.UUID, error)
	Dead(ctx context.Context) (int64, error)
	IsTx(ctx context.Context) bool
	ListDead(ctx context.Context, limit int32) ([]*Message, error)
	Lock(ctx context.Context, id uuid.UUID, lockedBy string, lease time.Duration) error
	Nack(ctx context.Context, id uuid.UUID, lockedBy string, dead bool, lastError string, delay time.Duration) error
	Poll(ctx context.Context, lockedBy string, limit int32, lease time.Duration) ([]*Message, error)
	Purge(ctx context.Context, status string, before time.Time) (int64, error)
	Release(ctx context.Context, id uuid.UUID, lockedBy string) error
	Requeue(ctx context.Context, id uuid.UUID) error
	RunInSubTx(ctx context.Context, fn func(txCtx context.Context) error) error
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
	// Each message's lease is renewed when its handler starts, and the
	// message cannot be reclaimed while the handler runs. A message whose
	// lease expires while it waits for its turn in the batch may be
	// reclaimed by another worker, and is then skipped. Defaults to
	// DefaultLease.
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
	if params.MaxAttempts < 0 {
		return uuid.Nil(), ErrInvalidMaxAttempts
	}
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
// Errors from fn are recorded on the message, and not returned. A message that
// another worker reclaimed before its turn is skipped with ErrLeaseExpired. If
// ctx is cancelled, or fn panics, the messages not handled yet are released
// without using up an attempt.
// It returns ErrEOQ if there are no visible messages, and ErrTxInContext if ctx
// already carries a transaction of the repository.
func (o *Outbox) Dequeue(ctx context.Context, limit int32, fn HandlerFunc) (err error) {
	if o.repo.IsTx(ctx) {
		return ErrTxInContext
	}
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
	// next is the first message not handled yet. The deferred release also
	// runs when fn panics.
	next := 0
	defer func() {
		if next < len(msgs) {
			if rerr := o.release(ctx, lockedBy, msgs[next:]); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
	}()
	for next < len(msgs) && ctx.Err() == nil {
		msg := msgs[next]
		next++
		if err := o.handle(ctx, lockedBy, msg, fn); err != nil {
			errs = append(errs, fmt.Errorf("outbox: message %s: %w", msg.ID, err))
		}
	}
	if next < len(msgs) {
		errs = append(errs, context.Cause(ctx))
	}

	return errors.Join(errs...)
}

func (o *Outbox) handle(ctx context.Context, lockedBy string, msg *Message, fn HandlerFunc) error {
	return o.repo.RunInTx(ctx, func(txCtx context.Context) error {
		// Renew the lease and hold the row lock while fn runs, so that the
		// message is not delivered to another worker meanwhile.
		if err := o.repo.Lock(txCtx, msg.ID, lockedBy, o.lease()); err != nil {
			return err
		}

		// Run fn in a savepoint, so that its writes roll back on error while
		// the failure is still recorded in this transaction. A Nack after the
		// transaction would race with other workers reclaiming the message.
		var handlerErr error
		err := o.repo.RunInSubTx(txCtx, func(txCtx context.Context) error {
			handlerErr = fn(txCtx, msg)
			return handlerErr
		})
		if handlerErr == nil {
			if err != nil {
				return err
			}
			return o.repo.Ack(txCtx, msg.ID, lockedBy)
		}

		dead := msg.Attempts >= msg.MaxAttempts || errors.Is(handlerErr, ErrDeadLetter)
		return o.repo.Nack(txCtx, msg.ID, lockedBy, dead, handlerErr.Error(), o.backoff(msg))
	})
}

// release returns messages that were never handled, on a context that
// outlives ctx, since ctx being done is a common reason to get here.
func (o *Outbox) release(ctx context.Context, lockedBy string, msgs []*Message) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	var errs []error
	for _, msg := range msgs {
		err := o.repo.Release(ctx, msg.ID, lockedBy)
		if err != nil && !errors.Is(err, ErrLeaseExpired) {
			errs = append(errs, fmt.Errorf("outbox: releasing message %s: %w", msg.ID, err))
		}
	}
	return errors.Join(errs...)
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
