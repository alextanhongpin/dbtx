package outbox

import (
	"context"
	"encoding/json"
)

type ctxKey string

var outboxCtxKey ctxKey = "outbox"

type atomic interface {
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}

// flusher flushes the outbox events that are already written to the persistent
// storage.
// The events can be flushed to a message broker, or a queue.
type flusher interface {
	Flush(ctx context.Context, events []Event) error
}

// writer writes the outbox events to a persistent storage which supports
// transaction.
type writer interface {
	Write(ctx context.Context, events []Event) error
}

type writerFlusher interface {
	writer
	flusher
}

type Outbox struct {
	atomic
	writerFlusher
}

func New(atm atomic, wf writerFlusher) *Outbox {
	return &Outbox{
		atomic:        atm,
		writerFlusher: wf,
	}
}

func (o *Outbox) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	ob := new(outbox)
	err := o.atomic.RunInTx(ctx, func(txCtx context.Context) error {
		txCtx = context.WithValue(txCtx, outboxCtxKey, ob)
		if err := fn(txCtx); err != nil {
			return err
		}

		return o.Write(txCtx, ob.events)
	})
	if err != nil {
		return err
	}

	return o.Flush(ctx, ob.events)
}

type outbox struct {
	events []Event
}

func (o *outbox) Queue(evt ...Event) {
	o.events = append(o.events, evt...)
}

// Enqueue enqueues the events to the outbox.
func Enqueue(ctx context.Context, evt ...Event) bool {
	o, ok := ctx.Value(outboxCtxKey).(*outbox)
	if ok {
		o.Queue(evt...)
	}

	return ok
}

type Message struct {
	ID            string
	AggregateID   string
	AggregateType string
	Typ           string
	Payload       json.RawMessage
}

func (m *Message) AsEvent() Event {
	return &event{
		id:            m.ID,
		aggregateID:   m.AggregateID,
		aggregateType: m.AggregateType,
		typ:           m.Typ,
		payload:       m.Payload,
	}
}

type event struct {
	id            string
	aggregateID   string
	aggregateType string
	typ           string
	payload       json.RawMessage
}

func (e *event) ID() string {
	return e.id
}

func (e *event) AggregateID() string {
	return e.aggregateID
}

func (e *event) AggregateType() string {
	return e.aggregateType
}

func (e *event) Type() string {
	return e.typ
}

func (e *event) Payload() json.RawMessage {
	return e.payload
}

// Event is an outbox event.
// We use interface to allow hiding the actual event implementation.
// The format is based on here:
// https://debezium.io/blog/2019/02/19/reliable-microservices-data-exchange-with-the-outbox-pattern/
type Event interface {
	ID() string
	AggregateID() string
	AggregateType() string
	Type() string
	Payload() json.RawMessage
}
