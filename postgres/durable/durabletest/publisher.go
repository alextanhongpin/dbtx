package durabletest

import (
	"context"
	"slices"
	"sync"

	"github.com/alextanhongpin/dbtx/postgres/durable/outbox"
)

// Recorder is an in-memory outbox.Publisher. It records every accepted
// message, in order, including duplicates. Fail, when set, is consulted per
// message: a non-nil error rejects that message and the rest of the batch.
type Recorder struct {
	mu   sync.Mutex
	msgs []outbox.Message
	Fail func(m outbox.Message) error
}

func (r *Recorder) Publish(ctx context.Context, msgs []outbox.Message) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, m := range msgs {
		if r.Fail != nil {
			if err := r.Fail(m); err != nil {
				return i, err
			}
		}
		if err := ctx.Err(); err != nil {
			return i, err
		}
		r.msgs = append(r.msgs, m)
	}
	return len(msgs), nil
}

// Messages returns a copy of everything accepted so far.
func (r *Recorder) Messages() []outbox.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.msgs)
}
