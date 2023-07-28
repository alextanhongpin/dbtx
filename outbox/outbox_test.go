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

	msg := outbox.Message{
		ID:            "fake-id",
		AggregateID:   "aggregate-id",
		AggregateType: "aggregate-type",
		Type:          "event-type",
		Payload:       json.RawMessage(`{}`),
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

	assert.ElementsMatch(wf.flush, []outbox.Message{msg})
	assert.ElementsMatch(wf.write, []outbox.Message{msg})
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
