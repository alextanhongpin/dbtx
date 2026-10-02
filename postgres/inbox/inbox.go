package inbox

import (
	_ "embed"

	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/inbox/internal"
)

const (
	StatusDead       = "dead"
	StatusDone       = "done"
	StatusPending    = "pending"
	StatusProcessing = "processing"
)

var (
	ErrDeadLetter = errors.New("inbox: dead letter")
	ErrEOQ        = internal.ErrEOQ
	ErrExists     = internal.ErrExists
	ErrLeaseLost  = internal.ErrLeaseLost
	ErrNotFound   = internal.ErrNotFound
)

//go:embed internal/schema.sql
var Schema string

type PostgresRepository = internal.Repository

var (
	NewRepository = internal.NewRepository
)

// DefaultLease is how long Dequeue leases messages when Inbox.Lease is zero.
const DefaultLease = 30 * time.Second

type (
	AckParams                = internal.AckParams
	ClaimByAggregateIDParams = internal.ClaimByAggregateIDParams
	ClaimParams              = internal.ClaimParams
	CreateParams             = internal.CreateParams
	Message                  = internal.Message
	NackParams               = internal.NackParams
)

type Repository interface {
	Ack(ctx context.Context, params AckParams) error
	Claim(ctx context.Context, params ClaimParams) ([]*Message, error)
	ClaimByAggregateID(ctx context.Context, params ClaimByAggregateIDParams) ([]*Message, error)
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, params CreateParams) (uuid.UUID, error)
	ExpireLeases(ctx context.Context) error
	ListDead(ctx context.Context, limit int32) ([]*Message, error)
	Nack(ctx context.Context, params NackParams) error
	Purge(ctx context.Context, status string, updatedAt time.Time) (int64, error)
	Requeue(ctx context.Context, id uuid.UUID) error
	RunInTx(ctx context.Context, fn func(context.Context) error) error
}

// HandlerFunc processes a message. The ctx carries the transaction that
// acknowledges the message.
type HandlerFunc = func(ctx context.Context, msg *Message) error

type Inbox struct {
	repo Repository

	// Backoff returns how long a failed message stays hidden before it is
	// retried. Defaults to DefaultBackoff.
	Backoff func(msg *Message) time.Duration

	// Lease is how long dequeued messages stay hidden from other workers.
	// All messages in a batch are leased at once, so it must cover handling
	// the whole batch. Defaults to DefaultLease.
	Lease time.Duration

	// Ordered makes Dequeue handle messages of the same AggregateID one at a
	// time, in the order they were enqueued. A batch then holds at most one
	// message per aggregate. Messages without an AggregateID are not ordered.
	Ordered bool

	// OnError, if set, is called with every handler error. Handler errors are
	// not returned by Dequeue, since the message is retried or dead-lettered.
	OnError func(msg *Message, err error)
}

// DefaultBackoff doubles the delay on every attempt, starting from 1s and
// capped at 1h, with jitter.
func DefaultBackoff(msg *Message) time.Duration {
	d := min(time.Second<<min(max(msg.Attempts-1, 0), 12), time.Hour)
	return d/2 + rand.N(d/2+1)
}

func New(repo Repository) *Inbox {
	return &Inbox{
		repo: repo,
	}
}

type EnqueueParams = CreateParams

// Enqueue stores the message. It returns ErrExists if a message with the same
// Source and MessageID was already received.
func (i *Inbox) Enqueue(ctx context.Context, params EnqueueParams) (uuid.UUID, error) {
	return i.repo.Create(ctx, params)
}

// Dequeue leases up to limit messages and handles them one by one with fn. It
// returns ErrEOQ if no message is ready.
func (i *Inbox) Dequeue(ctx context.Context, limit int32, fn HandlerFunc) error {
	err := i.repo.ExpireLeases(ctx)
	if err != nil {
		return err
	}
	lockedBy := uuid.NewV7().String()
	msgs, err := i.claim(ctx, lockedBy, limit)
	if err != nil {
		return err
	}
	var errs []error
	for _, msg := range msgs {
		if err := i.handle(ctx, lockedBy, msg, fn); err != nil {
			errs = append(errs, fmt.Errorf("inbox: message %s: %w", msg.ID, err))
		}
	}

	return errors.Join(errs...)
}

func (i *Inbox) handle(ctx context.Context, lockedBy string, msg *Message, fn HandlerFunc) error {
	var handlerErr error
	// Run in tx
	err := i.repo.RunInTx(ctx, func(ctx context.Context) error {
		if err := fn(ctx, msg); err != nil {
			handlerErr = err
			return err
		}

		// Ack fails with ErrLeaseLost if another worker owns the message
		// now, which rolls back the writes made by fn.
		return i.repo.Ack(ctx, AckParams{ID: msg.ID, LockedBy: lockedBy})
	})
	if handlerErr == nil {
		return err
	}
	if i.OnError != nil {
		i.OnError(msg, handlerErr)
	}

	dead := msg.Attempts >= msg.MaxAttempts || errors.Is(handlerErr, ErrDeadLetter)
	return i.repo.Nack(ctx, NackParams{
		ID:           msg.ID,
		Dead:         dead,
		DelaySeconds: i.backoff(msg).Seconds(),
		LastError:    handlerErr.Error(),
		LockedBy:     lockedBy,
	})
}

func (i *Inbox) claim(ctx context.Context, lockedBy string, limit int32) ([]*Message, error) {
	if i.Ordered {
		return i.repo.ClaimByAggregateID(ctx, ClaimByAggregateIDParams{
			LeaseSeconds: i.lease().Seconds(),
			Limit:        limit,
			LockedBy:     lockedBy,
		})
	}
	return i.repo.Claim(ctx, ClaimParams{
		LeaseSeconds: i.lease().Seconds(),
		Limit:        limit,
		LockedBy:     lockedBy,
	})
}

func (i *Inbox) backoff(msg *Message) time.Duration {
	if i.Backoff != nil {
		return i.Backoff(msg)
	}
	return DefaultBackoff(msg)
}

func (i *Inbox) lease() time.Duration {
	if i.Lease > 0 {
		return i.Lease
	}
	return DefaultLease
}

// Count returns the number of visible messages.
func (i *Inbox) Count(ctx context.Context) (int64, error) {
	return i.repo.Count(ctx)
}

// DeadLetters returns up to limit dead messages, oldest first.
func (i *Inbox) DeadLetters(ctx context.Context, limit int32) ([]*Message, error) {
	return i.repo.ListDead(ctx, limit)
}

// Requeue makes a dead message visible again, with its attempts reset. It
// returns ErrNotFound if the message is not dead.
func (i *Inbox) Requeue(ctx context.Context, id uuid.UUID) error {
	return i.repo.Requeue(ctx, id)
}

// Purge deletes messages with the given status, StatusDone or StatusDead, last
// updated before t, and returns the number of messages deleted. Call it
// periodically to keep the table small.
func (i *Inbox) Purge(ctx context.Context, status string, before time.Time) (int64, error) {
	switch status {
	case StatusDone, StatusDead:
		return i.repo.Purge(ctx, status, before)
	default:
		return 0, fmt.Errorf("inbox: cannot purge %q messages", status)
	}
}
