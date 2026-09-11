// package outbox implements outbox pattern using postgres.
// All operations should run inside a transaction, and user has the full
// flexibility to commit or rollback the transaction.
package outbox

import (
	_ "embed"

	_ "github.com/lib/pq"

	"context"
	"database/sql"
	"errors"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox/internal/postgres"
)

//go:embed internal/schema.sql
var schema string

var (
	ErrNotFound = errors.New("outbox: not found")
	ErrEOQ      = errors.New("outbox: end of queue")
)

type Message = postgres.DbtxOutbox

type Outbox struct {
	*dbtx.DB
}

func New(db *sql.DB) *Outbox {
	return &Outbox{
		DB: dbtx.New(db),
	}
}

type EnqueueParams = postgres.EnqueueParams

// Enqueue enqueues a new message to outbox.
func (o *Outbox) Enqueue(ctx context.Context, params EnqueueParams) (uuid.UUID, error) {
	return o.db(ctx).Enqueue(ctx, params)
}

// Count returns the number of visible messages.
func (o *Outbox) Count(ctx context.Context) (int64, error) {
	return o.db(ctx).Count(ctx)
}

type Nack struct {
	Error   string
	Skip    bool
	Timeout time.Duration
}

func (o *Outbox) Handle(ctx context.Context, id uuid.UUID, fn func(context.Context, Message) (*Nack, error)) error {
	return o.RunInTx(ctx, func(txCtx context.Context) error {
		row, err := o.db(txCtx).Find(txCtx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		nack, err := fn(txCtx, *row)
		if err != nil {
			return err
		}
		if nack == nil {
			_, err = o.db(txCtx).Delete(txCtx, row.ID)
			return err
		}

		params := postgres.RequeueParams{
			ID:        row.ID,
			LastError: nack.Error,
			VisibleAt: time.Now().Add(nack.Timeout),
		}

		// Emulate DLQ by setting max retry to -1 (no longer retryable).
		if nack.Skip {
			params.MaxRetry = -1
		}

		_, err = o.db(txCtx).Requeue(txCtx, params)
		return err
	})
}

func (o *Outbox) Dequeue(ctx context.Context, fn func(context.Context, Message) (*Nack, error)) error {
	return o.RunInTx(ctx, func(txCtx context.Context) error {
		row, err := o.db(txCtx).Peek(txCtx)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEOQ
		}

		nack, err := fn(txCtx, *row)
		if err != nil {
			return err
		}
		if nack == nil {
			_, err = o.db(txCtx).Delete(txCtx, row.ID)
			return err
		}

		params := postgres.RequeueParams{
			ID:        row.ID,
			LastError: nack.Error,
			VisibleAt: time.Now().Add(nack.Timeout),
		}

		// Emulate DLQ by setting max retry to -1 (no longer retryable).
		if nack.Skip {
			params.MaxRetry = -1
		}

		_, err = o.db(txCtx).Requeue(txCtx, params)
		if err != nil {
			return err
		}

		return nil
	})
}

func (o *Outbox) Migrate(ctx context.Context) error {
	_, err := o.DBTx(ctx).ExecContext(ctx, schema)
	return err
}

func (o *Outbox) db(ctx context.Context) postgres.Querier {
	return postgres.New(o.DBTx(ctx))
}
