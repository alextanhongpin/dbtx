// Package outbox stores events in the same transaction as business writes and
// relays them to a broker at least once. Topics are registered up front and
// are either unordered (events published independently) or ordered (events
// of one aggregate get a gapless sequence and are published one stream at a
// time, in order).
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// HeadPolicy decides what an ordered stream does when its head event keeps
// failing to publish.
type HeadPolicy string

const (
	// HeadBlock stops the stream (status blocked) and alerts; other streams
	// keep flowing. Resume it after fixing the cause.
	HeadBlock HeadPolicy = "block"
	// HeadPark turns the head event into a gap marker, publishes the marker
	// in its place, and continues. Consumers see the gap explicitly.
	HeadPark HeadPolicy = "park"
	// HeadStop stops the stream until an operator resumes it.
	HeadStop HeadPolicy = "stop"
)

// Advance decides how often an ordered stream records progress (Q3).
type Advance string

const (
	AdvancePerEvent Advance = "event" // fewer duplicates after a crash
	AdvancePerBatch Advance = "batch" // fewer writes
)

// TopicConfig describes one topic.
type TopicConfig struct {
	Ordered       bool
	OnHeadFailure HeadPolicy      // ordered only; default HeadBlock
	Advance       Advance         // ordered only; default AdvancePerEvent
	MaxAttempts   int             // per event (unordered) or per head (ordered); default 10
	BatchSize     int             // default 100
	Backoff       durable.Backoff // default durable.DefaultBackoff
}

// Config is copied at construction.
type Config struct {
	DB     *dbtx.DB
	Topics map[string]TopicConfig
	// AllowNoTx lets Publish commit on its own when ctx has no transaction.
	// By default Publish requires one, because an event committed separately
	// from its business change loses the outbox guarantee.
	AllowNoTx bool
}

// Outbox publishes events and administers streams.
type Outbox struct{ cfg Config }

func New(cfg Config) (*Outbox, error) {
	if cfg.DB == nil || cfg.DB.Unwrap() == nil {
		return nil, fmt.Errorf("%w: DB is required", durable.ErrInvalidConfig)
	}
	if len(cfg.Topics) == 0 {
		return nil, fmt.Errorf("%w: register at least one topic", durable.ErrInvalidConfig)
	}
	topics := maps.Clone(cfg.Topics)
	for name, t := range topics {
		if name == "" {
			return nil, fmt.Errorf("%w: empty topic name", durable.ErrInvalidConfig)
		}
		if t.OnHeadFailure == "" {
			t.OnHeadFailure = HeadBlock
		}
		if t.Advance == "" {
			t.Advance = AdvancePerEvent
		}
		if t.MaxAttempts <= 0 {
			t.MaxAttempts = 10
		}
		if t.BatchSize <= 0 {
			t.BatchSize = 100
		}
		if t.Backoff == nil {
			t.Backoff = durable.DefaultBackoff
		}
		topics[name] = t
	}
	cfg.Topics = topics
	return &Outbox{cfg: cfg}, nil
}

// Event is one domain event.
type Event struct {
	Topic string
	Type  string
	// AggregateKey identifies the stream on an ordered topic (required there).
	// On an unordered topic it is only passed to the publisher as a
	// partition hint.
	AggregateKey string
	Headers      map[string]string
	// Payload is JSON-encoded unless it is json.RawMessage or []byte.
	Payload any
	// RunAt delays publication of unordered events. Ordered events are
	// published in sequence and cannot be delayed individually.
	RunAt time.Time
}

// Published identifies a stored event.
type Published struct {
	ID  uuid.UUID
	Seq int64 // 0 on unordered topics
}

// Publish stores e. On ordered topics it takes the next sequence number for
// its aggregate; producers of one aggregate are serialized until commit, so
// sequence order equals commit order.
func (o *Outbox) Publish(ctx context.Context, e Event) (Published, error) {
	t, ok := o.cfg.Topics[e.Topic]
	if !ok {
		return Published{}, fmt.Errorf("%w: %q", durable.ErrUnknownTopic, e.Topic)
	}
	if t.Ordered && e.AggregateKey == "" {
		return Published{}, fmt.Errorf("%w: topic %q", durable.ErrAggregateRequired, e.Topic)
	}
	if !o.cfg.AllowNoTx && !o.cfg.DB.IsTx(ctx) {
		return Published{}, durable.ErrNotInTx
	}
	if e.Type == "" {
		return Published{}, fmt.Errorf("%w: event type is required", durable.ErrInvalidArgument)
	}
	payload, err := pg.Marshal(e.Payload)
	if err != nil {
		return Published{}, fmt.Errorf("%w: payload: %v", durable.ErrInvalidArgument, err)
	}
	headers, err := json.Marshal(e.Headers)
	if err != nil {
		return Published{}, err
	}
	if e.Headers == nil {
		headers = []byte("{}")
	}
	id := pg.NewID()
	var res Published
	err = o.cfg.DB.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(o.cfg.DB.DBTx(txCtx))
		if t.Ordered {
			key := sql.NullString{String: e.AggregateKey, Valid: true}
			seq, err := q.NextHeadSeq(txCtx, postgres.NextHeadSeqParams{Topic: e.Topic, AggregateKey: e.AggregateKey})
			if err != nil {
				return err
			}
			res.Seq = seq
			return q.InsertOrderedEvent(txCtx, postgres.InsertOrderedEventParams{
				ID: id, Topic: e.Topic, Type: e.Type, AggregateKey: key,
				Seq: sql.NullInt64{Int64: seq, Valid: true}, Headers: headers, Payload: payload,
				MaxAttempts: int32(t.MaxAttempts),
			})
		}
		p := postgres.InsertUnorderedEventParams{
			ID: id, Topic: e.Topic, Type: e.Type, Headers: headers, Payload: payload,
			MaxAttempts: int32(t.MaxAttempts),
		}
		if e.AggregateKey != "" {
			p.PartitionKey = sql.NullString{String: e.AggregateKey, Valid: true}
		}
		if !e.RunAt.IsZero() {
			p.RunAt = sql.NullTime{Time: e.RunAt, Valid: true}
		}
		return q.InsertUnorderedEvent(txCtx, p)
	})
	res.ID = id
	return res, err
}

// Message is what a Publisher receives. ID is stable across republishing;
// consumers deduplicate on it, or on (AggregateKey, Seq) for ordered topics.
type Message struct {
	ID           string
	Topic        string
	Type         string // GapType for gap markers
	AggregateKey string
	Seq          int64
	Headers      map[string]string
	Payload      json.RawMessage // null for gap markers
	IsGap        bool
	Attempt      int
}

// GapType is the Type of a gap marker: the event at Seq was skipped.
const GapType = "durable.gap"

// Publisher sends messages of one topic to a broker. It returns how many
// messages, from the start of msgs, were accepted; with an error, the rest are
// retried later. Accepting a message may happen more than once.
type Publisher interface {
	Publish(ctx context.Context, msgs []Message) (published int, err error)
}

// PublisherFunc adapts a function to Publisher.
type PublisherFunc func(ctx context.Context, msgs []Message) (int, error)

func (f PublisherFunc) Publish(ctx context.Context, msgs []Message) (int, error) { return f(ctx, msgs) }
