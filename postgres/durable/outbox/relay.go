package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/engine"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/pg"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// RelayConfig is copied at construction. Zero values select defaults.
type RelayConfig struct {
	Publisher      Publisher
	Owner          string        // identifies this relay in lease rows; default random
	Concurrency    int           // ordered streams in parallel; default 8
	PollInterval   time.Duration // default 500ms
	LeaseDuration  time.Duration // default 30s
	RequestTimeout time.Duration // default 5s
	PublishTimeout time.Duration // per Publisher call; default 10s
	Observer       durable.Observer
	Hooks          *durable.Hooks
}

// Relay publishes stored events. Run any number of relays; they coordinate
// through row leases.
type Relay struct {
	o      *Outbox
	cfg    RelayConfig
	topics string // JSON array of topic names
}

func NewRelay(o *Outbox, cfg RelayConfig) (*Relay, error) {
	if o == nil || cfg.Publisher == nil {
		return nil, fmt.Errorf("%w: outbox and publisher are required", durable.ErrInvalidConfig)
	}
	if cfg.Owner == "" {
		cfg.Owner = "relay-" + rand.Text()[:10]
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 30 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if cfg.PublishTimeout <= 0 {
		cfg.PublishTimeout = 10 * time.Second
	}
	if cfg.LeaseDuration < 6*time.Millisecond {
		return nil, fmt.Errorf("%w: LeaseDuration >= 6ms", durable.ErrInvalidConfig)
	}
	// Unordered events are not heartbeated while the publisher runs, so a
	// publish must end well before their lease does.
	cfg.PublishTimeout = min(cfg.PublishTimeout, cfg.LeaseDuration/2)
	names := make([]string, 0, len(o.cfg.Topics))
	for n := range o.cfg.Topics {
		names = append(names, n)
	}
	slices.Sort(names)
	b, _ := json.Marshal(names)
	return &Relay{o: o, cfg: cfg, topics: string(b)}, nil
}

// Run publishes until ctx ends.
func (r *Relay) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		n, err := r.RunOnce(ctx)
		if err == nil && n > 0 {
			continue
		}
		t := time.NewTimer(r.cfg.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	return nil
}

// RunOnce publishes one batch of unordered events per topic and one pass over
// up to Concurrency ordered streams. It returns how many messages were
// accepted by the publisher.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	n1, err1 := r.runUnordered(ctx)
	n2, err2 := r.runStreams(ctx)
	return n1 + n2, errors.Join(err1, err2)
}

const (
	eventsTable  = "durable.outbox_events"
	streamsTable = "durable.outbox_streams"
)

func (r *Relay) q() *postgres.Queries { return postgres.New(r.o.cfg.DB.DB()) }

func (r *Relay) observe(ctx context.Context, t durable.EventType, topic, key string, seq int64, err error) {
	durable.Observe(ctx, r.cfg.Observer, durable.Event{Type: t, Component: "outbox", Kind: topic, Key: key, Seq: seq, Err: err})
}

// ---- unordered --------------------------------------------------------------

type eventRow struct {
	id          uuid.UUID
	topic, typ  string
	aggregate   string
	seq         int64
	headers     []byte
	payload     []byte
	isGap       bool
	token       int64
	attempts    int
	maxAttempts int
}

func (e eventRow) message() Message {
	m := Message{ID: e.id.String(), Topic: e.topic, Type: e.typ, AggregateKey: e.aggregate, Seq: e.seq,
		Payload: e.payload, IsGap: e.isGap, Attempt: e.attempts}
	_ = json.Unmarshal(e.headers, &m.Headers)
	if e.isGap {
		m.Type, m.Payload = GapType, json.RawMessage("null")
	}
	return m
}

func (e eventRow) lease(owner string) engine.Row {
	return engine.Row{Table: eventsTable, Running: "running", ID: e.id, Owner: owner, Token: e.token}
}

func (r *Relay) runUnordered(ctx context.Context) (int, error) {
	owner := r.cfg.Owner + ":" + rand.Text()[:8]
	qctx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()
	q := r.q()
	// Events whose lease expired on the final attempt are dead.
	expired, err := q.ExpiredUnorderedEvents(qctx)
	if err != nil {
		return 0, err
	}
	for _, x := range expired {
		row := engine.Row{Table: eventsTable, Running: "running", ID: x.ID, Owner: x.OwnerID.String, Token: x.Token}
		if _, err := row.Expire(qctx, q, "dead", "lease expired on final attempt"); err != nil {
			return 0, err
		}
	}
	claimed, err := q.ClaimUnorderedEvents(qctx, postgres.ClaimUnorderedEventsParams{
		Topics: pg.JSON(r.topics), Lim: int32(r.maxBatch()),
		OwnerID: owner, TtlUs: pg.Micros(r.cfg.LeaseDuration),
	})
	if err != nil {
		return 0, err
	}
	byTopic := map[string][]eventRow{}
	var topics []string
	for _, c := range claimed {
		if _, ok := byTopic[c.Topic]; !ok {
			topics = append(topics, c.Topic)
		}
		byTopic[c.Topic] = append(byTopic[c.Topic], eventRow{
			id: c.ID, topic: c.Topic, typ: c.Type, aggregate: c.PartitionKey,
			headers: c.Headers, payload: c.Payload, token: c.Token,
			attempts: int(c.Attempts), maxAttempts: int(c.MaxAttempts),
		})
	}
	total := 0
	var errs []error
	for _, topic := range topics {
		events := byTopic[topic]
		t := r.o.cfg.Topics[topic]
		msgs := make([]Message, len(events))
		for i, e := range events {
			msgs[i] = e.message()
		}
		pctx, cancel := context.WithTimeout(ctx, r.cfg.PublishTimeout)
		n, perr := r.cfg.Publisher.Publish(pctx, msgs)
		cancel()
		n = max(0, min(n, len(msgs)))
		if n > 0 {
			r.cfg.Hooks.CallAfterPublishBeforeAdvance(ctx, events[n-1].id.String(), 0)
			ids := make([]string, n)
			for i := range n {
				ids[i] = events[i].id.String()
			}
			b, _ := json.Marshal(ids)
			// Owner match is enough (D7): while it matches, no other relay
			// reclaimed the events. Reclaimed ones are published again.
			m, err := q.MarkEventsPublished(context.WithoutCancel(ctx), postgres.MarkEventsPublishedParams{
				Ids: b, OwnerID: sql.NullString{String: owner, Valid: true},
			})
			if err != nil {
				errs = append(errs, err)
			} else if int(m) < n {
				r.observe(ctx, durable.EventLeaseLost, topic, "", 0,
					fmt.Errorf("outbox: %d of %d published events were reclaimed before being recorded", n-int(m), n))
			}
			total += n
		}
		if n < len(events) {
			if perr == nil {
				perr = errors.New("outbox: publisher accepted only part of the batch")
			}
			// Only the rejected message spends an attempt; the ones behind it
			// were never tried and go back immediately. If our own context
			// ended, the broker did not reject anything: nothing spends one.
			rest := events[n:]
			if ctx.Err() == nil {
				if err := r.failEvent(ctx, owner, events[n], t, perr); err != nil {
					errs = append(errs, err)
				}
				rest = events[n+1:]
			}
			for _, e := range rest {
				if err := r.returnEvent(ctx, owner, e); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return total, errors.Join(errs...)
}

func (r *Relay) maxBatch() int {
	m := 0
	for _, t := range r.o.cfg.Topics {
		m = max(m, t.BatchSize)
	}
	return m
}

func (r *Relay) failEvent(ctx context.Context, owner string, e eventRow, t TopicConfig, perr error) error {
	retry := !durable.IsPermanent(perr) && e.attempts < e.maxAttempts
	delay := durable.NextDelay(perr, e.attempts, t.Backoff)
	status := "pending"
	if !retry {
		status = "dead"
		r.observe(ctx, durable.EventFailed, e.topic, e.id.String(), 0, perr)
	} else {
		r.observe(ctx, durable.EventRetryScheduled, e.topic, e.id.String(), 0, perr)
	}
	msg := perr.Error()
	_, err := e.lease(owner).Finish(context.WithoutCancel(ctx), r.q(), engine.Finish{
		Status: status, Delay: &delay, LastError: &msg,
	})
	return err
}

// returnEvent gives back an event that was claimed but never offered to the
// publisher, without consuming an attempt.
func (r *Relay) returnEvent(ctx context.Context, owner string, e eventRow) error {
	_, err := e.lease(owner).Finish(context.WithoutCancel(ctx), r.q(), engine.Finish{
		Status: "pending", Delay: engine.Ptr(time.Duration(0)), AttemptDelta: -1,
	})
	return err
}

// ---- ordered streams --------------------------------------------------------

type stream struct {
	id         uuid.UUID
	topic, key string
	owner      string
	token      int64
	last       int64
	attempts   int
	sent       time.Time
	keeper     *engine.Keeper
}

// lease is the stream's lease row. A held stream stays "active".
func (s *stream) lease() engine.Row {
	return engine.Row{Table: streamsTable, Running: "active", ID: s.id, Owner: s.owner, Token: s.token}
}

func (r *Relay) runStreams(ctx context.Context) (int, error) {
	ordered := false
	for _, t := range r.o.cfg.Topics {
		ordered = ordered || t.Ordered
	}
	if !ordered {
		return 0, nil
	}
	qctx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()
	q := r.q()
	// Streams are created lazily for aggregates with pending events.
	if err := q.EnsureStreams(qctx, pg.JSON(r.topics)); err != nil {
		return 0, err
	}
	owner := r.cfg.Owner + ":" + rand.Text()[:8]
	sent := time.Now()
	rows, err := q.ClaimStreams(qctx, postgres.ClaimStreamsParams{
		Topics: pg.JSON(r.topics), Lim: int32(r.cfg.Concurrency),
		OwnerID: owner, TtlUs: pg.Micros(r.cfg.LeaseDuration),
	})
	if err != nil {
		return 0, err
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
		errs  []error
	)
	for _, row := range rows {
		s := &stream{id: row.ID, topic: row.Topic, key: row.AggregateKey, owner: owner, token: row.Token,
			last: row.LastPublishedSeq, attempts: int(row.Attempts), sent: sent}
		wg.Go(func() {
			n, err := r.runStream(ctx, s)
			mu.Lock()
			total += n
			errs = append(errs, err)
			mu.Unlock()
		})
	}
	wg.Wait()
	return total, errors.Join(errs...)
}

func (r *Relay) runStream(ctx context.Context, s *stream) (int, error) {
	t := r.o.cfg.Topics[s.topic]
	s.keeper = engine.NewKeeper(r.cfg.LeaseDuration, r.cfg.RequestTimeout, s.sent, func(rctx context.Context) (bool, error) {
		return s.lease().Renew(rctx, r.q(), r.cfg.LeaseDuration)
	}, func(hctx context.Context) { r.cfg.Hooks.CallBeforeHeartbeat(hctx, s.topic+"/"+s.key) })
	defer s.keeper.Close()
	published := 0
	var outcome error
	_, _ = s.keeper.Supervise(ctx, s.key, func(workCtx context.Context) error {
		published, outcome = r.drainStream(workCtx, s, t)
		return outcome
	})
	if terminal := s.keeper.Err(); terminal != nil {
		r.observe(ctx, durable.EventLeaseLost, s.topic, s.key, s.last, terminal)
		return published, nil
	}
	if errors.Is(outcome, durable.ErrLeaseLost) {
		return published, nil
	}
	return published, outcome
}

// drainStream publishes the stream's pending events in order until it is
// empty, fails, or the lease is lost.
func (r *Relay) drainStream(ctx context.Context, s *stream, t TopicConfig) (int, error) {
	published := 0
	for {
		events, err := r.loadStream(ctx, s, t.BatchSize)
		if err != nil {
			return published, err
		}
		if len(events) == 0 {
			return published, r.releaseStream(ctx, s, 0, "")
		}
		if events[0].seq != s.last+1 {
			// Cannot happen with gapless sequences; back off and look again.
			err := fmt.Errorf("outbox: stream %s/%s expected seq %d, found %d", s.topic, s.key, s.last+1, events[0].seq)
			return published, errors.Join(err, r.releaseStream(ctx, s, time.Second, err.Error()))
		}
		size := 1
		if t.Advance == AdvancePerBatch {
			size = len(events)
		}
		for i := 0; i < len(events); i += size {
			chunk := events[i:min(i+size, len(events))]
			msgs := make([]Message, len(chunk))
			for j, e := range chunk {
				msgs[j] = e.message()
				msgs[j].Attempt = s.attempts + 1
			}
			pctx, cancel := context.WithTimeout(ctx, r.cfg.PublishTimeout)
			n, perr := r.cfg.Publisher.Publish(pctx, msgs)
			cancel()
			n = max(0, min(n, len(msgs)))
			if n > 0 {
				seq := chunk[n-1].seq
				r.cfg.Hooks.CallAfterPublishBeforeAdvance(ctx, s.topic+"/"+s.key, seq)
				if err := r.advance(ctx, s, seq); err != nil {
					return published, err
				}
				published += n
			}
			if n < len(chunk) {
				if ctx.Err() != nil || s.keeper.Err() != nil {
					// Shutdown or lost lease, not a broker rejection: give the
					// stream back without spending an attempt.
					return published, errors.Join(perr, r.releaseStream(ctx, s, 0, ""))
				}
				if perr == nil {
					perr = errors.New("outbox: publisher accepted only part of the batch")
				}
				return published, r.headFailed(ctx, s, t, chunk[n], perr)
			}
		}
	}
}

func (r *Relay) loadStream(ctx context.Context, s *stream, limit int) ([]eventRow, error) {
	rows, err := r.q().LoadStreamEvents(ctx, postgres.LoadStreamEventsParams{
		Topic: s.topic, AggregateKey: s.key,
		AfterSeq: s.last, Lim: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]eventRow, len(rows))
	for i, e := range rows {
		out[i] = eventRow{id: e.ID, topic: s.topic, aggregate: s.key, typ: e.Type, seq: e.Seq,
			headers: e.Headers, payload: e.Payload, isGap: e.IsGap}
	}
	return out, nil
}

// locked runs fn in a transaction that first locks the stream's lease; a
// stale relay (lease lost or fenced out by an admin change) changes nothing.
func (r *Relay) locked(ctx context.Context, s *stream, fn func(txCtx context.Context, q *postgres.Queries) error) error {
	db := r.o.cfg.DB
	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		q := postgres.New(db.DBTx(txCtx))
		held, err := s.lease().Lock(txCtx, q, true)
		if err != nil {
			return err
		}
		if !held {
			return durable.ErrLeaseLost
		}
		return fn(txCtx, q)
	})
	if errors.Is(err, durable.ErrLeaseLost) {
		s.keeper.Lose(durable.ErrLeaseLost)
	}
	return err
}

// advance records progress under the stream lease.
func (r *Relay) advance(ctx context.Context, s *stream, seq int64) error {
	err := r.locked(ctx, s, func(txCtx context.Context, q *postgres.Queries) error {
		n, err := q.SetStreamPosition(txCtx, postgres.SetStreamPositionParams{Seq: seq, ID: s.id})
		if err != nil {
			return err
		}
		if n == 0 {
			return durable.ErrLeaseLost
		}
		return q.MarkStreamPublished(txCtx, postgres.MarkStreamPublishedParams{
			Topic: s.topic, AggregateKey: s.key,
			AfterSeq: s.last, UpToSeq: seq,
		})
	})
	if err != nil {
		return err
	}
	s.last, s.attempts = seq, 0
	return nil
}

// releaseStream ends this relay's hold on the stream.
func (r *Relay) releaseStream(ctx context.Context, s *stream, delay time.Duration, lastErr string) error {
	f := engine.Finish{Status: "active", Delay: &delay}
	if lastErr != "" {
		f.LastError = &lastErr
	}
	_, err := s.lease().Finish(context.WithoutCancel(ctx), r.q(), f)
	return err
}

// headFailed applies the retry budget and then the topic's head policy. Every
// write is fenced: a relay that lost its stream changes nothing.
func (r *Relay) headFailed(ctx context.Context, s *stream, t TopicConfig, head eventRow, perr error) error {
	attempts := s.attempts + 1
	ctx = context.WithoutCancel(ctx)
	msg := perr.Error()
	finish := func(ctx context.Context, q *postgres.Queries, f engine.Finish) error {
		ok, err := s.lease().Finish(ctx, q, f)
		if err != nil {
			return err
		}
		if !ok {
			s.keeper.Lose(durable.ErrLeaseLost)
			return durable.ErrLeaseLost
		}
		return nil
	}
	if attempts < t.MaxAttempts && !durable.IsPermanent(perr) {
		delay := durable.NextDelay(perr, attempts, t.Backoff)
		r.observe(ctx, durable.EventRetryScheduled, s.topic, s.key, head.seq, perr)
		return finish(ctx, r.q(), engine.Finish{Status: "active", RequireUnexpired: true,
			Delay: &delay, LastError: &msg, AttemptDelta: 1})
	}
	policy := t.OnHeadFailure
	if policy == HeadPark && head.isGap {
		policy = HeadBlock // even the gap marker cannot be published
	}
	switch policy {
	case HeadPark:
		r.observe(ctx, durable.EventStreamParked, s.topic, s.key, head.seq, perr)
		return r.locked(ctx, s, func(txCtx context.Context, q *postgres.Queries) error {
			if err := finish(txCtx, q, engine.Finish{Status: "active", Delay: engine.Ptr(time.Duration(0)),
				LastError: &msg, AttemptDelta: -s.attempts}); err != nil {
				return err
			}
			_, err := q.MarkEventGap(txCtx, postgres.MarkEventGapParams{
				Reason: msg, Topic: s.topic, AggregateKey: s.key,
				Seq: head.seq,
			})
			return err
		})
	default: // HeadBlock, HeadStop
		status := "blocked"
		if policy == HeadStop {
			status = "stopped"
		}
		r.observe(ctx, durable.EventStreamBlocked, s.topic, s.key, head.seq, perr)
		return finish(ctx, r.q(), engine.Finish{Status: status, RequireUnexpired: true,
			LastError: &msg, AttemptDelta: 1})
	}
}
