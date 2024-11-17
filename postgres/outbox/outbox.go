package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/outbox/internal/postgres"
	"golang.org/x/sync/errgroup"
)

var outboxContextKey contextKey = "outbox"

type Outbox struct {
	*dbtx.Atomic
}

//go:generate sqlc -f internal/sqlc.yaml generate
func New(db *sql.DB, fns ...func(dbtx.DBTX) dbtx.DBTX) *Outbox {
	return &Outbox{
		Atomic: dbtx.New(db, fns...),
	}
}

// RunInTx injects the outbox in the context to allow messages to be enqueued
// and written to the outbox table.
// Use a separate background job to process the outbox messages.
func (o *Outbox) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return o.Atomic.RunInTx(ctx, func(txCtx context.Context) error {
		ob := new(outbox)
		txCtx = outboxContextKey.WithValue(txCtx, ob)
		if err := fn(txCtx); err != nil {
			return err
		}

		// Write events.
		if !ob.IsZero() {
			return o.db(txCtx).Create(txCtx, ob.Params())
		}

		return nil
	})
}

// Count return the number of outbox messages
func (o *Outbox) Count(ctx context.Context) (int64, error) {
	return o.db(ctx).Count(ctx)
}

// Process processes the outbox message sequentially one at a time.
func (o *Outbox) Process(ctx context.Context, fn func(context.Context, Event) error) error {
	return o.Atomic.RunInTx(ctx, func(txCtx context.Context) error {
		e, err := o.db(txCtx).Delete(txCtx)
		if err != nil {
			return err
		}

		return fn(txCtx, Event{
			ID:            e.ID,
			AggregateID:   e.AggregateID,
			AggregateType: e.AggregateType,
			Payload:       e.Payload,
			Type:          e.Type,
			CreatedAt:     e.CreatedAt,
		})
	})
}

type PoolOptions struct {
	BatchSize   int           // The number of rows to process per batch.
	Concurrency int           // The max concurrent message to process at a time.
	Interval    time.Duration // The pool interval.
}

func (o *Outbox) Pool(ctx context.Context, fn func(context.Context, Event) error, opts *PoolOptions) func() {
	done := make(chan struct{})
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		panic("outbox.PoolOptions: Concurrency must be greater than 0")
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		panic("outbox.PoolOptions: BatchSize must be greater than 0")
	}
	interval := opts.Interval

	batch := func() {
		g, ctx := errgroup.WithContext(ctx)
		g.SetLimit(concurrency)
		defer g.Wait()

		for range batchSize {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			default:
				g.Go(func() error {
					err := o.Process(ctx, fn)
					if errors.Is(err, sql.ErrNoRows) {
						return err
					}

					return nil
				})
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()

		for {
			select {
			case <-done:
				return
			case <-t.C:
				batch()
			}
		}
	}()

	return sync.OnceFunc(func() {
		close(done)
		wg.Wait()
	})
}

func (o *Outbox) db(ctx context.Context) postgres.Querier {
	return postgres.New(o.Atomic.DBTx(ctx))
}

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

type outbox struct {
	mu   sync.RWMutex
	msgs []Message
}

func (o *outbox) Enqueue(msgs ...Message) {
	o.mu.Lock()
	o.msgs = append(o.msgs, msgs...)
	o.mu.Unlock()
}

func (o *outbox) IsZero() bool {
	o.mu.RLock()
	isZero := len(o.msgs) == 0
	o.mu.RUnlock()

	return isZero
}

func (o *outbox) Params() (params postgres.CreateParams) {
	o.mu.RLock()
	for _, msg := range o.msgs {
		params.AggregateIds = append(params.AggregateIds, msg.AggregateID)
		params.AggregateTypes = append(params.AggregateTypes, msg.AggregateType)
		params.Payloads = append(params.Payloads, string(msg.Payload))
		params.Types = append(params.Types, msg.Type)
	}
	o.mu.RUnlock()

	return
}

// Enqueue enqueues the events to the outbox.
func Enqueue(ctx context.Context, msgs ...Message) bool {
	o, ok := outboxContextKey.Value(ctx)
	if ok {
		o.Enqueue(msgs...)
	}

	return ok
}

type contextKey string

func (key contextKey) WithValue(ctx context.Context, ob *outbox) context.Context {
	return context.WithValue(ctx, key, ob)
}

func (key contextKey) Value(ctx context.Context) (*outbox, bool) {
	ob, ok := ctx.Value(key).(*outbox)
	return ob, ok
}
