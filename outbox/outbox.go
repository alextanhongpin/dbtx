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

// flusher flush the messages after it has been written.
type flusher interface {
	Flush(ctx context.Context, messages []Message) error
}

// write writes the outbox messages to a persistent
// storage which supports transaction.
type writer interface {
	Write(ctx context.Context, messages []Message) error
}

type queuer interface {
	Queue(msgs ...Message)
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
		if err := fn(withValue(txCtx, ob)); err != nil {
			return err
		}

		return o.Write(txCtx, ob.messages)
	})
	if err != nil {
		return err
	}

	return o.Flush(ctx, ob.messages)
}

type outbox struct {
	messages []Message
}

func (o *outbox) Queue(msg ...Message) {
	o.messages = append(o.messages, msg...)
}

func withValue(ctx context.Context, o *outbox) context.Context {
	return context.WithValue(ctx, outboxCtxKey, o)
}

func Value(ctx context.Context) (queuer, bool) {
	o, ok := ctx.Value(outboxCtxKey).(*outbox)
	return o, ok
}

// https://debezium.io/blog/2019/02/19/reliable-microservices-data-exchange-with-the-outbox-pattern/
type Message struct {
	ID            string
	AggregateID   string
	AggregateType string
	Type          string
	Payload       json.RawMessage
}
