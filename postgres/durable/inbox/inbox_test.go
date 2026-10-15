package inbox_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/inbox"
	"github.com/alextanhongpin/dbtx/postgres/durable/jobs"
	"github.com/alextanhongpin/dbtx/postgres/durable/outbox"
)

type applied struct {
	mu   sync.Mutex
	seqs []int64
	fail map[int64]int // seq -> remaining failures
}

func (a *applied) apply(db *dbtx.DB) inbox.ApplyFunc {
	return func(txCtx context.Context, m inbox.Message) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.fail[m.Seq] > 0 {
			a.fail[m.Seq]--
			return errors.New("apply failed")
		}
		if _, err := db.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO projection (seq) VALUES ($1)`, m.Seq); err != nil {
			return err
		}
		a.seqs = append(a.seqs, m.Seq)
		return nil
	}
}

func setup(t *testing.T) (*inbox.Inbox, *dbtx.DB) {
	t.Helper()
	db := dbtx.New(durabletest.NewDB(t))
	if _, err := db.DB().ExecContext(t.Context(), `CREATE TABLE projection (seq bigint)`); err != nil {
		t.Fatal(err)
	}
	in, err := inbox.New(inbox.Config{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	return in, db
}

func projected(t *testing.T, db *dbtx.DB) []int64 {
	t.Helper()
	rows, err := db.DB().QueryContext(t.Context(), `SELECT seq FROM projection`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var s int64
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func seqMsg(seq int64) inbox.Message {
	return inbox.Message{Source: "orders", MessageID: fmt.Sprint("m", seq), AggregateKey: "o1", Seq: seq}
}

// S25 + FM-26: dedup, and a failed apply leaves no record.
func TestDedup(t *testing.T) {
	ctx := t.Context()
	in, db := setup(t)
	a := &applied{fail: map[int64]int{7: 1}}
	m := inbox.Message{Source: "psp", MessageID: "evt_1", Seq: 7}
	if _, err := in.Receive(ctx, inbox.ModeDedup, m, a.apply(db)); err == nil {
		t.Fatal("want apply error")
	}
	for _, want := range []inbox.Result{inbox.Applied, inbox.Duplicate} {
		res, err := in.Receive(ctx, inbox.ModeDedup, m, a.apply(db))
		if err != nil || res != want {
			t.Fatalf("got %v %v, want %v", res, err, want)
		}
	}
	if p := projected(t, db); fmt.Sprint(p) != "[7]" {
		t.Fatalf("projection %v", p)
	}
}

// FM-24: out-of-order delivery is buffered and drained in order.
func TestSequencedBuffersAndDrains(t *testing.T) {
	ctx := t.Context()
	in, db := setup(t)
	a := &applied{}
	want := []inbox.Result{inbox.Applied, inbox.Buffered, inbox.Buffered, inbox.Duplicate, inbox.Applied, inbox.Duplicate}
	for i, seq := range []int64{1, 4, 3, 1, 2, 3} {
		res, err := in.Receive(ctx, inbox.ModeSequenced, seqMsg(seq), a.apply(db))
		if err != nil || res != want[i] {
			t.Fatalf("seq %d: got %v %v, want %v", seq, res, err, want[i])
		}
	}
	if p := projected(t, db); fmt.Sprint(p) != "[1 2 3 4]" {
		t.Fatalf("projection %v", p)
	}
	if gaps, _ := in.Gaps(ctx, 0); len(gaps) != 0 {
		t.Fatalf("gaps %v", gaps)
	}
}

func TestSequencedGapMarkerAndSkipGap(t *testing.T) {
	ctx := t.Context()
	in, db := setup(t)
	a := &applied{}
	gap := seqMsg(2)
	gap.IsGap = true
	for _, m := range []inbox.Message{seqMsg(1), gap, seqMsg(3), seqMsg(6)} {
		if _, err := in.Receive(ctx, inbox.ModeSequenced, m, a.apply(db)); err != nil {
			t.Fatal(err)
		}
	}
	gaps, err := in.Gaps(ctx, 0)
	if err != nil || len(gaps) != 1 || gaps[0].LastApplied != 3 || gaps[0].NextBuffered != 6 {
		t.Fatalf("gaps %+v %v", gaps, err)
	}
	from, to, n, err := in.SkipGap(ctx, "orders", "o1", a.apply(db))
	if err != nil || from != 4 || to != 5 || n != 1 {
		t.Fatalf("skip %d..%d applied %d: %v", from, to, n, err)
	}
	if p := projected(t, db); fmt.Sprint(p) != "[1 3 6]" {
		t.Fatalf("projection %v", p)
	}
}

// A buffered message that fails during drain stays buffered; Drain retries it.
func TestDrainFailureStaysBuffered(t *testing.T) {
	ctx := t.Context()
	in, db := setup(t)
	a := &applied{fail: map[int64]int{2: 1}}
	for _, seq := range []int64{2, 3, 1} {
		if _, err := in.Receive(ctx, inbox.ModeSequenced, seqMsg(seq), a.apply(db)); err != nil {
			t.Fatal(err)
		}
	}
	if p := projected(t, db); fmt.Sprint(p) != "[1]" {
		t.Fatalf("projection %v", p)
	}
	n, err := in.Drain(ctx, "orders", "o1", a.apply(db))
	if err != nil || n != 2 {
		t.Fatalf("drained %d %v", n, err)
	}
	if p := projected(t, db); fmt.Sprint(p) != "[1 2 3]" {
		t.Fatalf("projection %v", p)
	}
}

// S28 + FM-25: only newer versions apply.
func TestVersioned(t *testing.T) {
	ctx := t.Context()
	in, db := setup(t)
	a := &applied{}
	want := map[int64]inbox.Result{2: inbox.Applied, 1: inbox.Duplicate, 3: inbox.Applied, 3 + 100: inbox.Applied}
	for _, v := range []int64{2, 1, 3, 103} {
		m := inbox.Message{Source: "psp", MessageID: fmt.Sprint("evt", v), AggregateKey: "sub_1", Version: v, Seq: v}
		res, err := in.Receive(ctx, inbox.ModeVersioned, m, a.apply(db))
		if err != nil || res != want[v] {
			t.Fatalf("v%d: %v %v", v, res, err)
		}
	}
	if p := projected(t, db); fmt.Sprint(p) != "[2 3 103]" {
		t.Fatalf("projection %v", p)
	}
}

// S27: store-and-forward submits exactly one job.
func TestEnqueue(t *testing.T) {
	ctx := t.Context()
	db := dbtx.New(durabletest.NewDB(t))
	jc, _ := jobs.NewClient(jobs.Config{DB: db, Kinds: []string{"catalog.reindex"}})
	in, _ := inbox.New(inbox.Config{DB: db, Jobs: jc})
	m := inbox.Message{Source: "catalog", MessageID: "e1", Payload: []byte(`{"track": 9}`)}
	res, id, err := in.Enqueue(ctx, m, jobs.SubmitRequest{Kind: "catalog.reindex"})
	if err != nil || res != inbox.Enqueued {
		t.Fatalf("%v %v", res, err)
	}
	res, _, err = in.Enqueue(ctx, m, jobs.SubmitRequest{Kind: "catalog.reindex"})
	if err != nil || res != inbox.Duplicate {
		t.Fatalf("%v %v", res, err)
	}
	j, err := jc.Get(ctx, id)
	if err != nil || string(j.Payload) != `{"track": 9}` || j.Key != "catalog:e1" {
		t.Fatalf("%+v %v", j, err)
	}
}

// FM-21 end to end: two relays (one paused mid-stream) deliver duplicates
// and interleaving; the sequenced inbox applies exactly 1..N in order.
func TestOrderedOutboxIntoSequencedInbox(t *testing.T) {
	ctx := t.Context()
	db := dbtx.New(durabletest.NewDB(t))
	if _, err := db.DB().ExecContext(ctx, `CREATE TABLE projection (seq bigint)`); err != nil {
		t.Fatal(err)
	}
	ob, _ := outbox.New(outbox.Config{DB: db, Topics: map[string]outbox.TopicConfig{"orders": {Ordered: true}}})
	for range 6 {
		_ = db.RunInTx(ctx, func(txCtx context.Context) error {
			_, err := ob.Publish(txCtx, outbox.Event{Topic: "orders", Type: "changed", AggregateKey: "o1", Payload: map[string]int{}})
			return err
		})
	}
	gate := durabletest.NewGate()
	defer gate.Open()
	rec := &durabletest.Recorder{}
	a, _ := outbox.NewRelay(ob, outbox.RelayConfig{Publisher: rec, LeaseDuration: 200 * time.Millisecond,
		Hooks: &durable.Hooks{
			AfterPublishBeforeAdvance: func(_ context.Context, _ string, seq int64) {
				if seq == 3 {
					gate.Wait()
				}
			},
			BeforeHeartbeat: func(context.Context, string) { gate.Wait() },
		}})
	b, _ := outbox.NewRelay(ob, outbox.RelayConfig{Publisher: rec, LeaseDuration: 200 * time.Millisecond})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = a.RunOnce(ctx) }()
	<-gate.Reached()
	time.Sleep(300 * time.Millisecond)
	for range 5 {
		_, _ = b.RunOnce(ctx)
	}
	gate.Open()
	<-done
	msgs := rec.Messages()
	if len(msgs) <= 6 {
		t.Fatalf("expected duplicates at the broker, got %d messages", len(msgs))
	}
	// Deliver in a scrambled order with duplicates, as a broker might.
	slices.Reverse(msgs)
	msgs = append(msgs, msgs[:3]...)
	in, _ := inbox.New(inbox.Config{DB: db})
	ap := &applied{}
	for _, m := range msgs {
		_, err := in.Receive(ctx, inbox.ModeSequenced, inbox.Message{
			Source: "orders-service", MessageID: m.ID, AggregateKey: m.AggregateKey, Seq: m.Seq,
			Type: m.Type, Payload: m.Payload, IsGap: m.IsGap,
		}, ap.apply(db))
		if err != nil {
			t.Fatal(err)
		}
	}
	if p := projected(t, db); fmt.Sprint(p) != "[1 2 3 4 5 6]" {
		t.Fatalf("projection %v", p)
	}
}
