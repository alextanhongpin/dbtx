package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/outbox"
)

var noBackoff = durable.Backoff(func(int) time.Duration { return 0 })

func setup(t *testing.T, topics map[string]outbox.TopicConfig) (*outbox.Outbox, *dbtx.DB) {
	t.Helper()
	db := dbtx.New(durabletest.NewDB(t))
	for name, c := range topics {
		c.Backoff = noBackoff
		topics[name] = c
	}
	o, err := outbox.New(outbox.Config{DB: db, Topics: topics})
	if err != nil {
		t.Fatal(err)
	}
	return o, db
}

func relay(t *testing.T, o *outbox.Outbox, cfg outbox.RelayConfig) *outbox.Relay {
	t.Helper()
	r, err := outbox.NewRelay(o, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func publish(t *testing.T, o *outbox.Outbox, db *dbtx.DB, topic, key string, n int) {
	t.Helper()
	for i := range n {
		err := db.RunInTx(t.Context(), func(txCtx context.Context) error {
			_, err := o.Publish(txCtx, outbox.Event{Topic: topic, Type: "e", AggregateKey: key, Payload: map[string]int{"i": i}})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func drain(t *testing.T, r *outbox.Relay) {
	t.Helper()
	// A pass can publish nothing while it schedules a retry or parks a head
	// event, so stop only after several idle passes in a row.
	idle := 0
	for range 200 {
		n, err := r.RunOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			idle = 0
		} else if idle++; idle == 3 {
			return
		}
	}
	t.Fatal("relay did not drain")
}

func seqs(msgs []outbox.Message, key string) []int64 {
	var out []int64
	for _, m := range msgs {
		if m.AggregateKey == key {
			out = append(out, m.Seq)
		}
	}
	return out
}

func TestPublishValidation(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"orders": {Ordered: true}, "plays": {}})
	if _, err := o.Publish(ctx, outbox.Event{Topic: "plays", Type: "x"}); !errors.Is(err, durable.ErrNotInTx) {
		t.Fatalf("want ErrNotInTx, got %v", err)
	}
	_ = db.RunInTx(ctx, func(txCtx context.Context) error {
		if _, err := o.Publish(txCtx, outbox.Event{Topic: "nope", Type: "x"}); !errors.Is(err, durable.ErrUnknownTopic) {
			t.Errorf("want ErrUnknownTopic, got %v", err)
		}
		if _, err := o.Publish(txCtx, outbox.Event{Topic: "orders", Type: "x"}); !errors.Is(err, durable.ErrAggregateRequired) {
			t.Errorf("want ErrAggregateRequired, got %v", err)
		}
		return nil
	})
}

// FM-19: sequence order equals commit order, and rollbacks leave no gaps.
func TestSequenceFollowsCommitOrder(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"orders": {Ordered: true}})
	first := make(chan struct{})
	var wg sync.WaitGroup
	var seqA, seqB int64
	wg.Go(func() {
		_ = db.RunInTx(ctx, func(txCtx context.Context) error {
			p, err := o.Publish(txCtx, outbox.Event{Topic: "orders", Type: "a", AggregateKey: "o1"})
			seqA = p.Seq
			close(first)
			time.Sleep(150 * time.Millisecond) // commits late
			return err
		})
	})
	<-first
	_ = db.RunInTx(ctx, func(txCtx context.Context) error {
		p, err := o.Publish(txCtx, outbox.Event{Topic: "orders", Type: "b", AggregateKey: "o1"})
		seqB = p.Seq
		return err
	})
	wg.Wait()
	if seqA != 1 || seqB != 2 {
		t.Fatalf("seqA=%d seqB=%d", seqA, seqB)
	}
	_ = db.RunInTx(ctx, func(txCtx context.Context) error {
		_, _ = o.Publish(txCtx, outbox.Event{Topic: "orders", Type: "c", AggregateKey: "o1"})
		return errors.New("rollback")
	})
	var p outbox.Published
	_ = db.RunInTx(ctx, func(txCtx context.Context) error {
		var err error
		p, err = o.Publish(txCtx, outbox.Event{Topic: "orders", Type: "d", AggregateKey: "o1"})
		return err
	})
	if p.Seq != 3 {
		t.Fatalf("seq after rollback = %d, want 3", p.Seq)
	}
}

func TestUnorderedRelayRetriesAndDeadLetters(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"plays": {MaxAttempts: 2}})
	publish(t, o, db, "plays", "", 5)
	rec := &durabletest.Recorder{}
	poison := ""
	rec.Fail = func(m outbox.Message) error {
		if poison == "" {
			poison = m.ID
		}
		if m.ID == poison {
			return errors.New("too large")
		}
		return nil
	}
	drain(t, relay(t, o, outbox.RelayConfig{Publisher: rec}))
	if got := len(rec.Messages()); got != 4 {
		t.Fatalf("published %d, want 4", got)
	}
	dead, err := o.DeadLetters(ctx, 10)
	if err != nil || len(dead) != 1 || dead[0].ID.String() != poison || dead[0].Attempts != 2 {
		t.Fatalf("dead=%+v %v", dead, err)
	}
	rec.Fail = nil
	if err := o.Requeue(ctx, dead[0].ID); err != nil {
		t.Fatal(err)
	}
	drain(t, relay(t, o, outbox.RelayConfig{Publisher: rec}))
	if got := len(rec.Messages()); got != 5 {
		t.Fatalf("published %d after requeue", got)
	}
}

func TestOrderedRelayKeepsOrderPerAggregate(t *testing.T) {
	o, db := setup(t, map[string]outbox.TopicConfig{"orders": {Ordered: true, BatchSize: 3}})
	for i := range 5 {
		publish(t, o, db, "orders", "a", 1)
		publish(t, o, db, "orders", fmt.Sprint("b", i%2), 1)
	}
	rec := &durabletest.Recorder{}
	drain(t, relay(t, o, outbox.RelayConfig{Publisher: rec}))
	if s := seqs(rec.Messages(), "a"); fmt.Sprint(s) != "[1 2 3 4 5]" {
		t.Fatalf("a: %v", s)
	}
	if s := seqs(rec.Messages(), "b0"); fmt.Sprint(s) != "[1 2 3]" {
		t.Fatalf("b0: %v", s)
	}
}

// FM-20/21: relay A pauses after publishing seq 2 (heartbeats paused too);
// relay B takes over; A's late advance is rejected. Nothing is lost and the
// broker sees seq 2 twice.
func TestPausedRelayCannotAdvance(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"orders": {Ordered: true}})
	publish(t, o, db, "orders", "o1", 5)
	gate := durabletest.NewGate()
	defer gate.Open()
	rec := &durabletest.Recorder{}
	a := relay(t, o, outbox.RelayConfig{Publisher: rec, Owner: "A", LeaseDuration: 200 * time.Millisecond,
		Hooks: &durable.Hooks{
			AfterPublishBeforeAdvance: func(_ context.Context, _ string, seq int64) {
				if seq == 2 {
					gate.Wait()
				}
			},
			BeforeHeartbeat: func(context.Context, string) { gate.Wait() },
		}})
	b := relay(t, o, outbox.RelayConfig{Publisher: rec, Owner: "B", LeaseDuration: 200 * time.Millisecond})
	aDone := make(chan struct{})
	go func() { defer close(aDone); _, _ = a.RunOnce(ctx) }()
	<-gate.Reached()
	time.Sleep(300 * time.Millisecond)
	drain(t, b)
	gate.Open()
	<-aDone
	got := seqs(rec.Messages(), "o1")
	if fmt.Sprint(got) != "[1 2 2 3 4 5]" {
		t.Fatalf("broker saw %v", got)
	}
	si, err := o.Stream(ctx, "orders", "o1")
	if err != nil || si.LastPublishedSeq != 5 || si.Pending != 0 {
		t.Fatalf("%+v %v", si, err)
	}
}

// FM-22: head-of-line policies.
func TestHeadPolicies(t *testing.T) {
	for _, policy := range []outbox.HeadPolicy{outbox.HeadBlock, outbox.HeadPark, outbox.HeadStop} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := t.Context()
			o, db := setup(t, map[string]outbox.TopicConfig{
				"orders": {Ordered: true, MaxAttempts: 2, OnHeadFailure: policy},
			})
			publish(t, o, db, "orders", "bad", 3)
			publish(t, o, db, "orders", "good", 2)
			rec := &durabletest.Recorder{Fail: func(m outbox.Message) error {
				if m.AggregateKey == "bad" && m.Seq == 2 && !m.IsGap {
					return errors.New("rejected by broker")
				}
				return nil
			}}
			drain(t, relay(t, o, outbox.RelayConfig{Publisher: rec}))
			msgs := rec.Messages()
			if s := seqs(msgs, "good"); fmt.Sprint(s) != "[1 2]" {
				t.Fatalf("good stream blocked: %v", s)
			}
			si, _ := o.Stream(ctx, "orders", "bad")
			switch policy {
			case outbox.HeadPark:
				if s := seqs(msgs, "bad"); fmt.Sprint(s) != "[1 2 3]" || !msgs[slicesIndex(msgs, "bad", 2)].IsGap {
					t.Fatalf("park: %v", s)
				}
			default:
				want := map[outbox.HeadPolicy]string{outbox.HeadBlock: "blocked", outbox.HeadStop: "stopped"}[policy]
				if si.Status != want || si.LastPublishedSeq != 1 {
					t.Fatalf("%+v", si)
				}
				if err := o.Skip(ctx, "orders", "bad", 2, "INC-1"); err != nil {
					t.Fatal(err)
				}
				drain(t, relay(t, o, outbox.RelayConfig{Publisher: rec}))
				si, _ = o.Stream(ctx, "orders", "bad")
				if si.Status != "active" || si.LastPublishedSeq != 3 {
					t.Fatalf("after skip: %+v", si)
				}
			}
		})
	}
}

func slicesIndex(msgs []outbox.Message, key string, seq int64) int {
	for i, m := range msgs {
		if m.AggregateKey == key && m.Seq == seq {
			return i
		}
	}
	return -1
}

// FM-23: with per-batch advance, the pointer moves exactly to the published prefix.
func TestPartialBatch(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"orders": {Ordered: true, Advance: outbox.AdvancePerBatch, MaxAttempts: 5}})
	publish(t, o, db, "orders", "o1", 5)
	fail := true
	rec := &durabletest.Recorder{Fail: func(m outbox.Message) error {
		if m.Seq == 3 && fail {
			fail = false
			return errors.New("broker hiccup")
		}
		return nil
	}}
	r := relay(t, o, outbox.RelayConfig{Publisher: rec})
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	si, _ := o.Stream(ctx, "orders", "o1")
	if si.LastPublishedSeq != 2 || si.Attempts != 1 {
		t.Fatalf("%+v", si)
	}
	drain(t, r)
	if s := seqs(rec.Messages(), "o1"); fmt.Sprint(s) != "[1 2 3 4 5]" {
		t.Fatalf("%v", s)
	}
}

// S24 + Q7: replay within the retention window; purge keeps positions.
func TestReplayAndPurge(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"orders": {Ordered: true}})
	publish(t, o, db, "orders", "o1", 3)
	rec := &durabletest.Recorder{}
	r := relay(t, o, outbox.RelayConfig{Publisher: rec})
	drain(t, r)
	if n, err := o.Replay(ctx, "orders", "o1", 2); err != nil || n != 2 {
		t.Fatalf("replay %d %v", n, err)
	}
	drain(t, r)
	if s := seqs(rec.Messages(), "o1"); fmt.Sprint(s) != "[1 2 3 2 3]" {
		t.Fatalf("%v", s)
	}
	if n, err := o.PurgePublished(ctx, time.Now().Add(time.Hour), 100); err != nil || n != 3 {
		t.Fatalf("purged %d %v", n, err)
	}
	if _, err := o.Replay(ctx, "orders", "o1", 1); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("replay of purged events: %v", err)
	}
	publish(t, o, db, "orders", "o1", 1)
	drain(t, r)
	if s := seqs(rec.Messages(), "o1"); s[len(s)-1] != 4 {
		t.Fatalf("sequence restarted after purge: %v", s)
	}
}

// Regression: shutdown during publish is not a broker rejection; the head is
// not parked and no attempt is spent.
func TestShutdownDoesNotParkHead(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"t": {Ordered: true, OnHeadFailure: outbox.HeadPark, MaxAttempts: 1}})
	publish(t, o, db, "t", "agg", 1)
	started := make(chan struct{}, 1)
	r := relay(t, o, outbox.RelayConfig{Publisher: outbox.PublisherFunc(func(ctx context.Context, m []outbox.Message) (int, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return 0, ctx.Err()
	})})
	rctx, cancel := context.WithCancel(ctx)
	go func() { <-started; cancel() }()
	_, _ = r.RunOnce(rctx)
	var gap bool
	_ = db.DB().QueryRowContext(ctx, `SELECT is_gap FROM durable.outbox_events WHERE seq = 1`).Scan(&gap)
	si, _ := o.Stream(ctx, "t", "agg")
	if gap || si.Attempts != 0 || si.Status != "active" {
		t.Fatalf("gap=%v %+v", gap, si)
	}
}

// Regression: a slow unordered publish is recorded as published, not dead.
func TestSlowUnorderedPublishRecorded(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"u": {MaxAttempts: 1}})
	publish(t, o, db, "u", "", 1)
	var calls atomic.Int32
	r := relay(t, o, outbox.RelayConfig{LeaseDuration: 50 * time.Millisecond, Publisher: outbox.PublisherFunc(
		func(_ context.Context, m []outbox.Message) (int, error) {
			calls.Add(1)
			time.Sleep(150 * time.Millisecond) // ignores ctx
			return len(m), nil
		})})
	_, _ = r.RunOnce(ctx)
	_, _ = r.RunOnce(ctx)
	var status string
	_ = db.DB().QueryRowContext(ctx, `SELECT status FROM durable.outbox_events`).Scan(&status)
	if status != "published" || calls.Load() != 1 {
		t.Fatalf("status=%s calls=%d", status, calls.Load())
	}
}

// Regression: Replay fences out a relay holding the stream, so its stale
// advance cannot skip the replayed events.
func TestReplayFencesOutHolder(t *testing.T) {
	ctx := t.Context()
	o, db := setup(t, map[string]outbox.TopicConfig{"orders": {Ordered: true}})
	publish(t, o, db, "orders", "o1", 3)
	gate := durabletest.NewGate()
	defer gate.Open()
	rec := &durabletest.Recorder{}
	a := relay(t, o, outbox.RelayConfig{Publisher: rec, Hooks: &durable.Hooks{
		AfterPublishBeforeAdvance: func(_ context.Context, _ string, seq int64) {
			if seq == 3 {
				gate.Wait()
			}
		}}})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = a.RunOnce(ctx) }()
	<-gate.Reached()
	if n, err := o.Replay(ctx, "orders", "o1", 1); err != nil || n != 2 {
		t.Fatalf("replay %d %v", n, err)
	}
	gate.Open()
	<-done
	drain(t, relay(t, o, outbox.RelayConfig{Publisher: rec}))
	si, _ := o.Stream(ctx, "orders", "o1")
	if got := fmt.Sprint(seqs(rec.Messages(), "o1")); got != "[1 2 3 1 2 3]" || si.LastPublishedSeq != 3 || si.Pending != 0 {
		t.Fatalf("broker saw %s, %+v", got, si)
	}
}
