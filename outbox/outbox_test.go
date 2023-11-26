package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/alextanhongpin/dbtx/outbox"
	"github.com/stretchr/testify/assert"
)

var ctx = context.Background()

func TestOutbox(t *testing.T) {
	atm := &mockAtomic{}
	wf := &mockWriterFlusher{}
	o := outbox.New(atm, wf)

	msg := &outbox.Message{
		ID:            "fake-id",
		AggregateID:   "aggregate-id",
		AggregateType: "aggregate-type",
		Typ:           "event-type",
		Payload:       json.RawMessage(`{}`),
	}

	is := assert.New(t)
	err := o.RunInTx(ctx, func(txCtx context.Context) error {
		if ok := outbox.Enqueue(txCtx, msg.AsEvent()); !ok {
			return errors.New("outbox not found")
		}

		return nil
	})
	is.Nil(err)

	events := []outbox.Event{msg.AsEvent()}
	is.ElementsMatch(wf.write, events)
	is.ElementsMatch(wf.flush, events)
}

type mockAtomic struct{}

func (m *mockAtomic) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

type mockWriterFlusher struct {
	write []outbox.Event
	flush []outbox.Event
}

func (m *mockWriterFlusher) Write(ctx context.Context, events []outbox.Event) error {
	m.write = events
	return nil
}

func (m *mockWriterFlusher) Flush(ctx context.Context, events []outbox.Event) error {
	m.flush = events
	return nil
}
