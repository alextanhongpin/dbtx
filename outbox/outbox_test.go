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

	msg := &message{
		id:            "fake-id",
		aggregateID:   "aggregate-id",
		aggregateType: "aggregate-type",
		typ:           "event-type",
		payload:       json.RawMessage(`{}`),
	}

	assert := assert.New(t)
	err := o.RunInTx(ctx, func(txCtx context.Context) error {
		ob, ok := outbox.Value(txCtx)
		if !ok {
			return errors.New("outbox not found")
		}
		ob.Queue(msg)

		return nil
	})
	assert.Nil(err)

	msgs := []outbox.Message{msg}
	assert.ElementsMatch(wf.write, msgs)
	assert.ElementsMatch(wf.flush, msgs)
}

type mockAtomic struct{}

func (m *mockAtomic) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

type mockWriterFlusher struct {
	write []outbox.Message
	flush []outbox.Message
}

func (m *mockWriterFlusher) Write(ctx context.Context, msgs []outbox.Message) error {
	m.write = msgs
	return nil
}

func (m *mockWriterFlusher) Flush(ctx context.Context, msgs []outbox.Message) error {
	m.flush = msgs
	return nil
}

type message struct {
	id            string
	aggregateID   string
	aggregateType string
	typ           string
	payload       json.RawMessage
}

func (m *message) ID() string {
	return m.id
}

func (m *message) AggregateID() string {
	return m.aggregateID
}

func (m *message) AggregateType() string {
	return m.aggregateType
}

func (m *message) Type() string {
	return m.typ
}

func (m *message) Payload() json.RawMessage {
	return m.payload
}
