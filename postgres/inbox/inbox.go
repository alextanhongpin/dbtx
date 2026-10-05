package inbox

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
	"uuid"
)

const (
	StatusDead       = "dead"
	StatusDone       = "done"
	StatusPending    = "pending"
	StatusProcessing = "processing"
)

var (
	ErrDeadLetter  = errors.New("inbox: dead letter")
	ErrEOQ         = errors.New("inbox: end of queue")
	ErrExists      = errors.New("inbox: message exists")
	ErrLeaseLost   = errors.New("inbox: lease lost")
	ErrNotFound    = errors.New("inbox: not found")
	ErrTxInContext = errors.New("inbox: ctx must not carry a transaction")

	ErrInvalidMaxAttempts = errors.New("inbox: max attempts must not be negative")
)

// DefaultLease is how long Dequeue leases messages when Inbox.Lease is zero.
const DefaultLease = 30 * time.Second

// releaseTimeout bounds releasing unhandled messages after ctx is done.
const releaseTimeout = 5 * time.Second

type AckParams struct {
	ID       uuid.UUID
	LockedBy string
}

type ClaimParams struct {
	Limit        int32
	LockedBy     string
	LeaseSeconds float64
}

type ClaimByAggregateIDParams struct {
	Limit        int32
	LockedBy     string
	LeaseSeconds float64
}

type CreateParams struct {
	Source      string
	MessageID   string
	MessageType string
	Payload     jsontext.Value
	AggregateID string

	// AvailableAt delays the first delivery. Zero means now.
	AvailableAt time.Time

	// MaxAttempts is the number of deliveries before the message is dead.
	// Zero means the default of 10.
	MaxAttempts int32
}

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

type LockParams struct {
	ID           uuid.UUID
	LockedBy     string
	LeaseSeconds float64
}

type ReleaseParams struct {
	ID       uuid.UUID
	LockedBy string
}

type NackParams struct {
	ID           uuid.UUID
	Dead         bool
	DelaySeconds float64
	LastError    string
	LockedBy     string
}

type Repository interface {
	Ack(ctx context.Context, params AckParams) error
	Claim(ctx context.Context, params ClaimParams) ([]*Message, error)
	ClaimByAggregateID(ctx context.Context, params ClaimByAggregateIDParams) ([]*Message, error)
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, params CreateParams) (uuid.UUID, error)
	ExpireLeases(ctx context.Context) error
	IsTx(ctx context.Context) bool
	ListDead(ctx context.Context, limit int32) ([]*Message, error)
	Lock(ctx context.Context, params LockParams) error
	LockAggregate(ctx context.Context, aggregateID string) error
	Nack(ctx context.Context, params NackParams) error
	Purge(ctx context.Context, status string, updatedAt time.Time) (int64, error)
	Release(ctx context.Context, params ReleaseParams) error
	Requeue(ctx context.Context, id uuid.UUID) error
	RunInSubTx(ctx context.Context, fn func(context.Context) error) error
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
	// Each message's lease is renewed when its handler starts, and the
	// message cannot be reclaimed while the handler runs. A message whose
	// lease expires while it waits for its turn in the batch may be
	// reclaimed by another worker, and is then skipped. Defaults to
	// DefaultLease.
	Lease time.Duration

	// Ordered makes Dequeue handle messages of the same AggregateID one at a
	// time, in the order they were enqueued. A batch then holds at most one
	// message per aggregate. Messages without an AggregateID are not ordered.
	//
	// Order follows the messages that are visible when one is claimed. A
	// message whose enqueuing transaction commits after a later message of
	// its aggregate was claimed, or a dead message that is requeued, is
	// handled after it, but never at the same time.
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
	if params.MaxAttempts < 0 {
		return uuid.Nil(), ErrInvalidMaxAttempts
	}
	return i.repo.Create(ctx, params)
}

// Dequeue leases up to limit messages and handles them one by one with fn. It
// returns ErrEOQ if no message is ready.
//
// A message that another worker reclaimed before its turn is skipped with
// ErrLeaseLost. If ctx is cancelled, or fn panics, the messages not handled
// yet are released without using up an attempt.
//
// Each message is handled in its own transaction, so Dequeue returns
// ErrTxInContext if ctx already carries a transaction of the repository.
func (i *Inbox) Dequeue(ctx context.Context, limit int32, fn HandlerFunc) (err error) {
	if i.repo.IsTx(ctx) {
		return ErrTxInContext
	}
	err = i.repo.ExpireLeases(ctx)
	if err != nil {
		return err
	}
	lockedBy := uuid.NewV7().String()
	msgs, err := i.claim(ctx, lockedBy, limit)
	if err != nil {
		return err
	}
	var errs []error
	// next is the first message not handled yet. The deferred release also
	// runs when fn panics.
	next := 0
	defer func() {
		if next < len(msgs) {
			if rerr := i.release(ctx, lockedBy, msgs[next:]); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
	}()
	for next < len(msgs) && ctx.Err() == nil {
		msg := msgs[next]
		next++
		if err := i.handle(ctx, lockedBy, msg, fn); err != nil {
			errs = append(errs, fmt.Errorf("inbox: message %s: %w", msg.ID, err))
		}
	}
	if next < len(msgs) {
		errs = append(errs, context.Cause(ctx))
	}

	return errors.Join(errs...)
}

func (i *Inbox) handle(ctx context.Context, lockedBy string, msg *Message, fn HandlerFunc) error {
	var handlerErr error
	err := i.repo.RunInTx(ctx, func(txCtx context.Context) error {
		// Renew the lease and hold the row lock while fn runs, so that the
		// message is not delivered to another worker meanwhile.
		err := i.repo.Lock(txCtx, LockParams{
			ID:           msg.ID,
			LockedBy:     lockedBy,
			LeaseSeconds: i.lease().Seconds(),
		})
		if err != nil {
			return err
		}

		// Claim skips aggregates with a live lease, but a handler can outlive
		// its lease. Wait for it, so that an aggregate is never handled
		// concurrently.
		if i.Ordered && msg.AggregateID != "" {
			if err := i.repo.LockAggregate(txCtx, msg.AggregateID); err != nil {
				return err
			}
		}

		// Run fn in a savepoint, so that its writes roll back on error while
		// the failure is still recorded in this transaction. A Nack after the
		// transaction would race with other workers reclaiming the message.
		err = i.repo.RunInSubTx(txCtx, func(txCtx context.Context) error {
			handlerErr = fn(txCtx, msg)
			return handlerErr
		})
		if handlerErr == nil {
			if err != nil {
				return err
			}
			return i.repo.Ack(txCtx, AckParams{ID: msg.ID, LockedBy: lockedBy})
		}

		dead := msg.Attempts >= msg.MaxAttempts || errors.Is(handlerErr, ErrDeadLetter)
		return i.repo.Nack(txCtx, NackParams{
			ID:           msg.ID,
			Dead:         dead,
			DelaySeconds: i.backoff(msg).Seconds(),
			LastError:    handlerErr.Error(),
			LockedBy:     lockedBy,
		})
	})
	if handlerErr != nil && i.OnError != nil {
		i.OnError(msg, handlerErr)
	}
	return err
}

// release returns messages that were never handled, on a context that
// outlives ctx, since ctx being done is a common reason to get here.
func (i *Inbox) release(ctx context.Context, lockedBy string, msgs []*Message) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	var errs []error
	for _, msg := range msgs {
		err := i.repo.Release(ctx, ReleaseParams{ID: msg.ID, LockedBy: lockedBy})
		if err != nil && !errors.Is(err, ErrLeaseLost) {
			errs = append(errs, fmt.Errorf("inbox: releasing message %s: %w", msg.ID, err))
		}
	}
	return errors.Join(errs...)
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
