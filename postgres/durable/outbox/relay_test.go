package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/outbox"
)

type events struct {
	mu  sync.Mutex
	got []durable.Event
}

func (e *events) Observe(_ context.Context, ev durable.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, ev)
}

func (e *events) count(t durable.EventType) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.got {
		if ev.Type == t {
			n++
		}
	}
	return n
}

func eventStatus(t *testing.T, db *dbtx.DB) (status string, attempts int, lastErr string) {
	t.Helper()
	err := db.DB().QueryRowContext(t.Context(),
		`SELECT status, attempts, coalesce(last_error, '') FROM durable.outbox_events`).Scan(&status, &attempts, &lastErr)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// A panicking publisher is a failed publish: it spends an attempt, records
// the error, emits an observer event, and the relay keeps running.
func TestPublisherPanicUnordered(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"u": {MaxAttempts: 2}})
	publish(t, o, db, "u", "", 1)
	obs := &events{}
	r := relay(t, o, outbox.RelayConfig{Observer: obs, Publisher: outbox.PublisherFunc(
		func(context.Context, []outbox.Message) (int, error) { panic("boom") })})
	for range 2 {
		if _, err := r.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	status, attempts, lastErr := eventStatus(t, db)
	if status != "dead" || attempts != 2 || !strings.Contains(lastErr, "publisher panic: boom") {
		t.Fatalf("status=%s attempts=%d err=%q", status, attempts, lastErr)
	}
	if obs.count(durable.EventRetryScheduled) != 1 || obs.count(durable.EventFailed) != 1 {
		t.Fatalf("observer: %+v", obs.got)
	}
}

func TestPublisherPanicOrdered(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"o": {Ordered: true, MaxAttempts: 1}})
	publish(t, o, db, "o", "k", 1)
	obs := &events{}
	r := relay(t, o, outbox.RelayConfig{Observer: obs, Publisher: outbox.PublisherFunc(
		func(context.Context, []outbox.Message) (int, error) { panic("boom") })})
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	si, err := o.Stream(ctx, "o", "k")
	if err != nil || si.Status != "blocked" || !strings.Contains(si.LastError, "publisher panic") {
		t.Fatalf("%+v %v", si, err)
	}
	if obs.count(durable.EventStreamBlocked) != 1 {
		t.Fatalf("observer: %+v", obs.got)
	}
}

// Run survives a panic and publishes once the publisher recovers.
func TestRunSurvivesPublisherPanic(t *testing.T) {
	o, db := setup(t, map[string]outbox.TopicConfig{"u": {}})
	publish(t, o, db, "u", "", 1)
	var calls atomic.Int32
	done := make(chan struct{})
	r := relay(t, o, outbox.RelayConfig{PollInterval: 10 * time.Millisecond, Publisher: outbox.PublisherFunc(
		func(_ context.Context, m []outbox.Message) (int, error) {
			if calls.Add(1) == 1 {
				panic("boom")
			}
			close(done)
			return len(m), nil
		})})
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error)
	go func() { stopped <- r.Run(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not retry after panic")
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if status, _, _ := eventStatus(t, db); status != "published" {
		t.Fatalf("status=%s", status)
	}
}

// Each topic is claimed with its own batch size.
func TestPerTopicBatchSize(t *testing.T) {
	o, db := setup(t, map[string]outbox.TopicConfig{"small": {BatchSize: 2}, "large": {BatchSize: 5}})
	publish(t, o, db, "small", "", 6)
	publish(t, o, db, "large", "", 6)
	var mu sync.Mutex
	batches := map[string][]int{}
	r := relay(t, o, outbox.RelayConfig{Publisher: outbox.PublisherFunc(func(_ context.Context, m []outbox.Message) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		batches[m[0].Topic] = append(batches[m[0].Topic], len(m))
		return len(m), nil
	})})
	if n, err := r.RunOnce(t.Context()); err != nil || n != 7 {
		t.Fatalf("n=%d %v", n, err)
	}
	drain(t, r)
	if got := fmt.Sprint(batches); got != "map[large:[5 1] small:[2 2 2]]" {
		t.Fatalf("batches %s", got)
	}
}

func TestCount(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"u": {MaxAttempts: 1}, "o": {Ordered: true}})
	publish(t, o, db, "u", "", 3)
	publish(t, o, db, "o", "k", 2)
	if c, err := o.Count(ctx, ""); err != nil || c != (outbox.Counts{Pending: 5}) {
		t.Fatalf("%+v %v", c, err)
	}
	fail := true
	rec := &durabletest.Recorder{Fail: func(m outbox.Message) error {
		if m.Topic == "u" && fail {
			fail = false
			return errors.New("rejected")
		}
		return nil
	}}
	drain(t, relay(t, o, outbox.RelayConfig{Publisher: rec}))
	if c, err := o.Count(ctx, "u"); err != nil || c != (outbox.Counts{Dead: 1}) {
		t.Fatalf("u: %+v %v", c, err)
	}
	if c, err := o.Count(ctx, "o"); err != nil || c != (outbox.Counts{}) {
		t.Fatalf("o: %+v %v", c, err)
	}
}

// Ported from postgres/outbox: shutdown releases claimed events without
// spending an attempt, and another relay publishes them.
func TestCancelReleasesUnordered(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"u": {MaxAttempts: 1}})
	publish(t, o, db, "u", "", 3)
	started := make(chan struct{})
	r := relay(t, o, outbox.RelayConfig{Publisher: outbox.PublisherFunc(func(ctx context.Context, m []outbox.Message) (int, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})})
	rctx, cancel := context.WithCancel(ctx)
	go func() { <-started; cancel() }()
	_, _ = r.RunOnce(rctx)
	if c, _ := o.Count(ctx, ""); c != (outbox.Counts{Pending: 3}) {
		t.Fatalf("after cancel: %+v", c)
	}
	rec := &durabletest.Recorder{}
	drain(t, relay(t, o, outbox.RelayConfig{Publisher: rec}))
	if len(rec.Messages()) != 3 {
		t.Fatalf("published %d", len(rec.Messages()))
	}
	for _, m := range rec.Messages() {
		if m.Attempt != 1 {
			t.Fatalf("cancel spent an attempt: %+v", m)
		}
	}
}

// Ported from postgres/outbox: an event whose lease expires on its final
// attempt is dead, not retried.
func TestLeaseExpiredOnFinalAttemptIsDead(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"u": {MaxAttempts: 1}})
	publish(t, o, db, "u", "", 1)
	_, err := db.DB().ExecContext(ctx, `UPDATE durable.outbox_events
		SET status = 'running', attempts = 1, owner_id = 'crashed', token = 1,
		    lease_expires_at = now() - interval '1 second'`)
	if err != nil {
		t.Fatal(err)
	}
	rec := &durabletest.Recorder{}
	drain(t, relay(t, o, outbox.RelayConfig{Publisher: rec}))
	dead, err := o.DeadLetters(ctx, 10)
	if err != nil || len(dead) != 1 || len(rec.Messages()) != 0 {
		t.Fatalf("dead=%+v published=%d %v", dead, len(rec.Messages()), err)
	}
	if !strings.Contains(dead[0].LastError, "lease expired") {
		t.Fatalf("%+v", dead[0])
	}
}

// Ported from postgres/outbox: purge removes published unordered events only.
func TestPurgeUnordered(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"u": {MaxAttempts: 1}})
	publish(t, o, db, "u", "", 3)
	poison := firstID(t, db)
	rec := &durabletest.Recorder{Fail: func(m outbox.Message) error {
		if m.ID == poison {
			return durable.Permanent(errors.New("poison"))
		}
		return nil
	}}
	drain(t, relay(t, o, outbox.RelayConfig{Publisher: rec}))
	if n, err := o.PurgePublished(ctx, time.Now().Add(-time.Hour), 100); err != nil || n != 0 {
		t.Fatalf("purged recent events: %d %v", n, err)
	}
	if n, err := o.PurgePublished(ctx, time.Now().Add(time.Hour), 100); err != nil || n != 2 {
		t.Fatalf("purged %d %v", n, err)
	}
	if c, _ := o.Count(ctx, ""); c != (outbox.Counts{Dead: 1}) {
		t.Fatalf("dead event purged: %+v", c)
	}
}

func firstID(t *testing.T, db *dbtx.DB) string {
	var id string
	if err := db.DB().QueryRowContext(t.Context(),
		`SELECT id::text FROM durable.outbox_events ORDER BY run_at, id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNewRelayRequiresOneConsumer(t *testing.T) {
	o, _ := setup(t, map[string]outbox.TopicConfig{"u": {}})
	h := func(context.Context, outbox.Message) error { return nil }
	if _, err := outbox.NewRelay(o, outbox.RelayConfig{}); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("none: %v", err)
	}
	if _, err := outbox.NewRelay(o, outbox.RelayConfig{Handler: h, Publisher: &durabletest.Recorder{}}); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("both: %v", err)
	}
}

func sideEffects(t *testing.T, db *dbtx.DB) []string {
	t.Helper()
	rows, err := db.DB().QueryContext(t.Context(), `SELECT v FROM side_effects ORDER BY n`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		out = append(out, v)
	}
	return out
}

func txHandler(t *testing.T, db *dbtx.DB, fail func(m outbox.Message) error) outbox.TxHandler {
	if _, err := db.DB().ExecContext(t.Context(), `CREATE TABLE side_effects (n serial, v text)`); err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, m outbox.Message) error {
		if !db.IsTx(ctx) {
			return errors.New("handler ctx carries no transaction")
		}
		v := fmt.Sprintf("%s/%d/%d", m.AggregateKey, m.Seq, m.Attempt)
		if _, err := db.DBTx(ctx).ExecContext(ctx, `INSERT INTO side_effects (v) VALUES ($1)`, v); err != nil {
			return err
		}
		return fail(m)
	}
}

// Handler mode: the handler's writes commit with the acknowledgement; a
// failed or panicking handler's writes roll back and an attempt is spent.
func TestHandlerUnorderedCommitsWithAck(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"u": {MaxAttempts: 3}})
	publish(t, o, db, "u", "", 1)
	h := txHandler(t, db, func(m outbox.Message) error {
		switch m.Attempt {
		case 1:
			return errors.New("first attempt fails")
		case 2:
			panic("second attempt panics")
		}
		return nil
	})
	drain(t, relay(t, o, outbox.RelayConfig{Handler: h}))
	if got := fmt.Sprint(sideEffects(t, db)); got != "[/0/3]" {
		t.Fatalf("side effects %s", got)
	}
	if status, attempts, _ := eventStatus(t, db); status != "published" || attempts != 3 {
		t.Fatalf("status=%s attempts=%d", status, attempts)
	}
	if c, _ := o.Count(ctx, ""); c != (outbox.Counts{}) {
		t.Fatalf("%+v", c)
	}
}

func TestHandlerUnorderedDeadLetter(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"u": {MaxAttempts: 2}})
	publish(t, o, db, "u", "", 1)
	h := txHandler(t, db, func(outbox.Message) error { return errors.New("always") })
	drain(t, relay(t, o, outbox.RelayConfig{Handler: h}))
	if len(sideEffects(t, db)) != 0 {
		t.Fatal("failed handler writes committed")
	}
	dead, err := o.DeadLetters(ctx, 10)
	if err != nil || len(dead) != 1 || dead[0].LastError != "always" {
		t.Fatalf("%+v %v", dead, err)
	}
}

func TestHandlerOrdered(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"o": {Ordered: true, MaxAttempts: 2}})
	publish(t, o, db, "o", "a", 3)
	publish(t, o, db, "o", "b", 2)
	h := txHandler(t, db, func(m outbox.Message) error {
		if m.AggregateKey == "a" && m.Seq == 2 && m.Attempt == 1 {
			return errors.New("retry me")
		}
		if m.AggregateKey == "b" && m.Seq == 2 {
			panic("poison")
		}
		return nil
	})
	drain(t, relay(t, o, outbox.RelayConfig{Handler: h}))
	var a, b []string
	for _, v := range sideEffects(t, db) {
		if strings.HasPrefix(v, "a/") {
			a = append(a, v)
		} else {
			b = append(b, v)
		}
	}
	if fmt.Sprint(a) != "[a/1/1 a/2/2 a/3/1]" || fmt.Sprint(b) != "[b/1/1]" {
		t.Fatalf("a=%v b=%v", a, b)
	}
	si, err := o.Stream(ctx, "o", "b")
	if err != nil || si.Status != "blocked" || si.LastPublishedSeq != 1 || !strings.Contains(si.LastError, "handler panic: poison") {
		t.Fatalf("%+v %v", si, err)
	}
	si, _ = o.Stream(ctx, "o", "a")
	if si.LastPublishedSeq != 3 || si.Pending != 0 {
		t.Fatalf("%+v", si)
	}
}
