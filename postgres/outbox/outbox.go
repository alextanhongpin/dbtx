package outbox

import (
	_ "embed"

	_ "github.com/lib/pq"

	"cmp"
	"fmt"
	"time"

	"context"
	"database/sql"
	"errors"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox/internal/postgres"
)

//go:embed internal/schema.sql
var schema string

var (
	ErrNotInTx  = errors.New("outbox: not in transaction")
	ErrNotFound = errors.New("outbox: not found")
	ErrEOQ      = errors.New("outbox: end of queue")
	ErrDLQ      = errors.New("outbox: dlq")
)

type Config struct {
	RetryAfter func(attempts int) time.Duration
	MaxRetry   int
}

func DefaultConfig() *Config {
	return &Config{
		// Constant.
		RetryAfter: func(attempts int) time.Duration {
			return 0
		},
	}
}

type Outbox struct {
	*Config
	*dbtx.DB
}

func New(db *sql.DB) *Outbox {
	return &Outbox{
		Config: DefaultConfig(),
		DB:     dbtx.New(db),
	}
}

func (o *Outbox) Create(ctx context.Context, msg Message) (uuid.UUID, error) {
	return o.db(ctx).Enqueue(ctx, postgres.EnqueueParams{
		AggregateID:   msg.AggregateID,
		AggregateType: msg.AggregateType,
		Type:          msg.Type,
		Payload:       msg.Payload,
	})
}

func (o *Outbox) Count(ctx context.Context) (int64, error) {
	return o.db(ctx).Count(ctx)
}

func (o *Outbox) CountDLQ(ctx context.Context) (int64, error) {
	return o.db(ctx).CountDLQ(ctx)
}

func (o *Outbox) process(ctx context.Context, msg *Message, cause error) (error, error) {
	// Nack. requeue=false
	if errors.Is(cause, ErrDLQ) {
		_, err := o.db(ctx).EnqueueDLQ(ctx, msg.ID)
		if err != nil {
			return nil, fmt.Errorf("enqueuing dlq: %w", err)
		}
		return cause, nil
	}
	// Nack.
	if cause != nil {
		timeout := o.RetryAfter(msg.RetryCount)
		if o.MaxRetry != 0 && msg.RetryCount >= o.MaxRetry {
			_, err := o.db(ctx).EnqueueDLQ(ctx, msg.ID)
			if err != nil {
				return nil, fmt.Errorf("enqueuing dlq after exceeded max retry: %w", err)
			}
			return cause, err
		}
		_, err := o.db(ctx).Requeue(ctx, postgres.RequeueParams{
			ID: msg.ID,
			RetryAt: sql.NullTime{
				Time:  time.Now().Add(timeout),
				Valid: timeout > 0,
			},
			FailureReason: cause.Error(),
		})
		if err != nil {
			return nil, fmt.Errorf("requeing: %w", err)
		}
		return cause, nil
	}
	// Ack.
	_, err := o.db(ctx).Delete(ctx, msg.ID)
	if err != nil {
		return nil, err
	}
	return cause, nil
}

func (o *Outbox) Delete(ctx context.Context, id uuid.UUID, fn func(context.Context, *Message) error) error {
	cause, err := o.RunInTx2(ctx, func(ctx context.Context) (error, error) {
		res, err := o.db(ctx).Dequeue(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		if err != nil {
			return nil, err
		}
		msg := newMessage(res)
		err = fn(ctx, msg)
		return o.process(ctx, msg, err)
	})
	return cmp.Or(cause, err)
}

// Poll polls the message by FIFO order.
func (o *Outbox) Poll(ctx context.Context, fn func(context.Context, *Message) error) error {
	cause, err := o.RunInTx2(ctx, func(ctx context.Context) (error, error) {
		res, err := o.db(ctx).DequeueFIFO(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrEOQ
		}
		if err != nil {
			return nil, err
		}
		msg := newMessage(res)
		err = fn(ctx, msg)
		return o.process(ctx, msg, err)
	})
	return cmp.Or(cause, err)
}

func (o *Outbox) Migrate(ctx context.Context) error {
	_, err := o.DBTx(ctx).ExecContext(ctx, schema)
	return err
}

func (o *Outbox) db(ctx context.Context) postgres.Querier {
	return postgres.New(o.DBTx(ctx))
}
