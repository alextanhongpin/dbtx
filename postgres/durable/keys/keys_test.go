package keys_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/keys"
)

func setup(t *testing.T, cfg keys.Config) (*keys.Keys, *dbtx.DB) {
	t.Helper()
	db := dbtx.New(durabletest.NewDB(t))
	cfg.DB = db
	k, err := keys.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(t.Context(), `CREATE TABLE payments (id serial, amount int)`); err != nil {
		t.Fatal(err)
	}
	return k, db
}

func req(key, body string) keys.Request {
	return keys.Request{Scope: "t1:payments.create", Key: key, Body: json.RawMessage(body)}
}

// S6: work runs once; replays return the stored response.
func TestDoRunsOnceAndReplays(t *testing.T) {
	ctx := t.Context()
	k, db := setup(t, keys.Config{})
	var calls atomic.Int32
	fn := func(txCtx context.Context) (keys.Response, error) {
		calls.Add(1)
		_, err := db.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO payments (amount) VALUES (10)`)
		return keys.Response{Status: 201, Body: json.RawMessage(`{"id":1}`)}, err
	}
	for range 3 {
		resp, err := k.Do(ctx, req("k1", `{"amount": 10}`), fn)
		if err != nil || resp.Status != 201 || string(resp.Body) != `{"id": 1}` && string(resp.Body) != `{"id":1}` {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fn ran %d times", calls.Load())
	}
	// FM-15: same key, different body (key order does not matter).
	if _, err := k.Do(ctx, req("k1", `{"amount": 11}`), fn); !errors.Is(err, durable.ErrRequestMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
	if _, err := k.Do(ctx, req("k1", ` { "amount":10 } `), fn); err != nil {
		t.Fatalf("whitespace-equal body rejected: %v", err)
	}
}

// FM-14: concurrent callers with one key: exactly one acquires.
func TestConcurrentBegin(t *testing.T) {
	ctx := t.Context()
	k, _ := setup(t, keys.Config{})
	var acquired, inflight atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			c, err := k.Begin(ctx, req("k", `{}`))
			if err != nil {
				t.Error(err)
				return
			}
			switch c.State {
			case keys.Acquired:
				acquired.Add(1)
			case keys.InFlight:
				inflight.Add(1)
			}
		})
	}
	wg.Wait()
	if acquired.Load() != 1 || inflight.Load() != 19 {
		t.Fatalf("acquired=%d inflight=%d", acquired.Load(), inflight.Load())
	}
}

// FM-16: an in-flight request stalls past its lease; a retry takes over and
// the stalled request can no longer complete.
func TestStalledRequestLosesKey(t *testing.T) {
	ctx := t.Context()
	gate := durabletest.NewGate()
	defer gate.Open()
	k, db := setup(t, keys.Config{
		InFlightTTL: 150 * time.Millisecond,
		Hooks:       &durable.Hooks{BeforeHeartbeat: func(context.Context, string) { gate.Wait() }},
	})
	a, err := k.Begin(ctx, req("k", `{}`))
	if err != nil || a.State != keys.Acquired {
		t.Fatalf("a: %v %v", a, err)
	}
	<-a.Done()
	if !errors.Is(a.Err(), durable.ErrLeaseLost) {
		t.Fatalf("a.Err = %v", a.Err())
	}
	time.Sleep(50 * time.Millisecond) // the server-side lease also expires
	b, err := k.Begin(ctx, req("k", `{}`))
	if err != nil || b.State != keys.Acquired || b.Token <= a.Token {
		t.Fatalf("b: %+v %v", b, err)
	}
	gate.Open()
	err = db.RunInTx(ctx, func(txCtx context.Context) error {
		return a.Complete(txCtx, keys.Response{Status: 200})
	})
	if !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("stale complete: %v", err)
	}
	if err := db.RunInTx(ctx, func(txCtx context.Context) error {
		return b.Complete(txCtx, keys.Response{Status: 201})
	}); err != nil {
		t.Fatal(err)
	}
	c, err := k.Begin(ctx, req("k", `{}`))
	if err != nil || c.State != keys.Completed || c.Response.Status != 201 {
		t.Fatalf("replay: %+v %v", c, err)
	}
}

// Q1: permanent failures with a response are stored; transient ones release.
func TestFailurePolicy(t *testing.T) {
	ctx := t.Context()
	k, _ := setup(t, keys.Config{})
	_, err := k.Do(ctx, req("bad", `{}`), func(context.Context) (keys.Response, error) {
		return keys.Response{Status: 422, Body: json.RawMessage(`{"error":"invalid"}`)},
			durable.Permanent(errors.New("invalid"))
	})
	if !durable.IsPermanent(err) {
		t.Fatalf("want permanent, got %v", err)
	}
	resp, err := k.Do(ctx, req("bad", `{}`), func(context.Context) (keys.Response, error) {
		t.Fatal("must not run again")
		return keys.Response{}, nil
	})
	if resp.Status != 422 || !durable.IsPermanent(err) {
		t.Fatalf("replayed failure: %+v %v", resp, err)
	}

	_, err = k.Do(ctx, req("flaky", `{}`), func(context.Context) (keys.Response, error) {
		return keys.Response{}, errors.New("timeout")
	})
	if err == nil {
		t.Fatal("want error")
	}
	resp, err = k.Do(ctx, req("flaky", `{}`), func(context.Context) (keys.Response, error) {
		return keys.Response{Status: 200}, nil
	})
	if err != nil || resp.Status != 200 {
		t.Fatalf("retry after release: %+v %v", resp, err)
	}
}

// FM-17: purged keys can be reused, with a higher token.
func TestPurgeAndReuse(t *testing.T) {
	ctx := t.Context()
	k, _ := setup(t, keys.Config{Retention: 10 * time.Millisecond})
	a, _ := k.Begin(ctx, req("k", `{}`))
	_ = a.Release(ctx)
	b, _ := k.Begin(ctx, req("k", `{}`))
	if _, err := k.Do(ctx, req("other", `{}`), func(context.Context) (keys.Response, error) {
		return keys.Response{Status: 200}, nil
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	n, err := k.Purge(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("purged %d (in-flight key must survive): %v", n, err)
	}
	_ = b.Release(ctx)
	c, err := k.Begin(ctx, req("other", `{"changed": true}`))
	if err != nil || c.State != keys.Acquired || c.Token <= b.Token {
		t.Fatalf("reuse after purge: %+v %v", c, err)
	}
	_ = c.Release(ctx)
}

func TestDerive(t *testing.T) {
	if keys.Derive("a:b", "c") == keys.Derive("a", "b:c") {
		t.Fatal("Derive is ambiguous")
	}
}

// Regression: a panicking handler must not leave the key in flight.
func TestDoPanicReleasesKey(t *testing.T) {
	ctx := t.Context()
	k, _ := setup(t, keys.Config{InFlightTTL: 60 * time.Millisecond})
	func() {
		defer func() { _ = recover() }()
		_, _ = k.Do(ctx, req("k", `{}`), func(context.Context) (keys.Response, error) { panic("boom") })
	}()
	c, err := k.Begin(ctx, req("k", `{}`))
	if err != nil || c.State != keys.Acquired {
		t.Fatalf("after panic: %+v %v", c, err)
	}
	_ = c.Release(ctx)
}
