// package outbox implements outbox pattern using postgres.
// All operations should run inside a transaction, and user has the full
// flexibility to commit or rollback the transaction.
package outbox

import (
	_ "embed"

	_ "github.com/lib/pq"

	"context"
	"errors"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/outbox/internal"
)

//go:embed internal/schema.sql
var Schema string

var (
	ErrNotFound = internal.ErrNotFound
	ErrEOQ      = internal.ErrEOQ

	NewRepository = internal.NewRepository
)

type (
	Repository = internal.Repository

	Message = internal.Message

	CreateParams = internal.CreateParams

	UpdateParams = internal.UpdateParams

	repository interface {
		Count(ctx context.Context) (int64, error)
		Create(ctx context.Context, params CreateParams) (*Message, error)
		Delete(ctx context.Context, id uuid.UUID) (*Message, error)
		Find(ctx context.Context, id uuid.UUID) (*Message, error)
		Load(ctx context.Context) (*Message, error)
		RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error
		Update(ctx context.Context, params UpdateParams) (*Message, error)
	}
)

type Outbox struct {
	repo repository
}

func New(repo repository) *Outbox {
	return &Outbox{
		repo: repo,
	}
}

type EnqueueParams = CreateParams

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

func (o *Outbox) Handle(ctx context.Context, id uuid.UUID, fn func(context.Context, Message) error) error {
	var nack *NackError
	err := o.repo.RunInTx(ctx, func(txCtx context.Context) error {
		row, err := o.repo.Find(txCtx, id)
		if err != nil {
			return err
		}

		err = fn(txCtx, *row)
		if err == nil {
			_, err := o.repo.Delete(txCtx, row.ID)
			return err
		}
		var ok bool
		nack, ok = errors.AsType[*NackError](err)
		if !ok {
			return err
		}

		params := UpdateParams{
			ID:         row.ID,
			LastError:  nack.Error(),
			MaxRetry:   row.MaxRetry,
			RetryCount: row.RetryCount + 1,
			VisibleAt:  time.Now().Add(nack.Timeout),
		}

		// Emulate DLQ by setting max retry to -1 (no longer retryable).
		if nack.Skip {
			params.MaxRetry = -1
		}

		_, err = o.repo.Update(txCtx, params)
		return err
	})
	if err != nil {
		return err
	}

	return nack.Unwrap()
}

func (o *Outbox) Dequeue(ctx context.Context, fn func(context.Context, Message) error) error {
	var nack *NackError
	err := o.repo.RunInTx(ctx, func(txCtx context.Context) error {
		row, err := o.repo.Load(txCtx)
		if err != nil {
			return err
		}

		err = fn(txCtx, *row)
		if err == nil {
			_, err = o.repo.Delete(txCtx, row.ID)
			return err
		}
		var ok bool
		nack, ok = errors.AsType[*NackError](err)
		if !ok {
			return err
		}

		params := UpdateParams{
			ID:         row.ID,
			LastError:  nack.Error(),
			MaxRetry:   row.MaxRetry,
			RetryCount: row.RetryCount + 1,
			VisibleAt:  time.Now().Add(nack.Timeout),
		}

		// Emulate DLQ by setting max retry to -1 (no longer retryable).
		if nack.Skip {
			params.MaxRetry = -1
		}

		_, err = o.repo.Update(txCtx, params)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nack.Unwrap()
}

type NackError struct {
	Cause   error
	Skip    bool
	Timeout time.Duration
}

func Nack(err error) *NackError {
	return &NackError{
		Cause: err,
	}
}

func (n *NackError) Error() string {
	return n.Cause.Error()
}

func (n *NackError) Unwrap() error {
	if n == nil || n.Cause == nil {
		return nil
	}
	return n.Cause
}
