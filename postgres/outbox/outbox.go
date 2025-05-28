package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox/internal/postgres"
)

var ErrNotInTx = errors.New("outbox: not in transaction")

// Message is the outbox message to enqueue.
type Message struct {
	AggregateID   string
	AggregateType string
	Payload       json.RawMessage
	Type          string
}

// Event is the enqueued message.
type Event struct {
	ID            int64
	AggregateID   string
	AggregateType string
	Payload       json.RawMessage
	Type          string
	CreatedAt     time.Time
}

type OutBox struct {
	dbtx.UnitOfWork
}

func New(uow dbtx.UnitOfWork) *OutBox {
	return &OutBox{
		UnitOfWork: uow,
	}
}

func (o *OutBox) db(ctx context.Context) postgres.Querier {
	return postgres.New(o.DBTx(ctx))
}

func (o *OutBox) Create(ctx context.Context, messages ...Message) error {
	var params postgres.CreateParams
	for _, msg := range messages {
		params.AggregateIds = append(params.AggregateIds, msg.AggregateID)
		params.AggregateTypes = append(params.AggregateTypes, msg.AggregateType)
		params.Payloads = append(params.Payloads, string(msg.Payload))
		params.Types = append(params.Types, msg.Type)
	}

	return o.db(ctx).Create(ctx, params)
}

func (o *OutBox) Count(ctx context.Context, messages ...Message) (int64, error) {
	return o.db(ctx).Count(ctx)
}

func (o *OutBox) LoadAndDelete(ctx context.Context) (*Event, error) {
	evt, err := o.db(ctx).Delete(ctx)
	if err != nil {
		return nil, err
	}

	return &Event{
		ID:            evt.ID,
		AggregateID:   evt.AggregateID,
		AggregateType: evt.AggregateType,
		Payload:       evt.Payload,
		Type:          evt.Type,
		CreatedAt:     evt.CreatedAt,
	}, nil
}
