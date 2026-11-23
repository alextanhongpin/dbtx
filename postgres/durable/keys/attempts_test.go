package keys_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/keys"
)

// crash claims key with a Keys from stalledConfig, whose heartbeat is
// blocked, as if the process died. It returns once the lease has expired on
// the server too.
func crash(t *testing.T, k *keys.Keys, key string) *keys.Claim {
	t.Helper()
	c, err := k.Begin(t.Context(), req(key, `{}`))
	if err != nil || c.State != keys.Acquired {
		t.Fatalf("crash: %+v %v", c, err)
	}
	<-c.Done()
	time.Sleep(50 * time.Millisecond)
	return c
}

func stalledConfig(gate *durabletest.Gate) keys.Config {
	return keys.Config{
		InFlightTTL: 100 * time.Millisecond,
		Hooks:       &durable.Hooks{BeforeHeartbeat: func(context.Context, string) { gate.Wait() }},
	}
}

// A retry takes over a key whose holder crashed; the crashed holder is
// fenced out and the takeover counts as a second attempt.
func TestTakeoverFromCrashedHolder(t *testing.T) {
	ctx := t.Context()
	gate := durabletest.NewGate()
	defer gate.Open()
	k, db := setup(t, stalledConfig(gate))
	stale := crash(t, k, "k")

	b, err := k.Begin(ctx, req("k", `{}`))
	if err != nil || b.State != keys.Acquired || b.Attempts != 2 || b.Token <= stale.Token {
		t.Fatalf("takeover: %+v %v", b, err)
	}
	if err := db.RunInTx(ctx, func(txCtx context.Context) error {
		return b.Complete(txCtx, keys.Response{Status: 201})
	}); err != nil {
		t.Fatal(err)
	}
	gate.Open()
	err = db.RunInTx(ctx, func(txCtx context.Context) error {
		return stale.Complete(txCtx, keys.Response{Status: 500})
	})
	if !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("stale complete: %v", err)
	}
	resp, err := k.Do(ctx, req("k", `{}`), func(context.Context) (keys.Response, error) {
		t.Fatal("must not run")
		return keys.Response{}, nil
	})
	if err != nil || resp.Status != 201 {
		t.Fatalf("replay: %+v %v", resp, err)
	}
}

// Failed attempts count; the one that uses up MaxAttempts exhausts the key.
func TestMaxAttempts(t *testing.T) {
	ctx := t.Context()
	k, _ := setup(t, keys.Config{MaxAttempts: 2})
	wantErr := errors.New("upstream down")
	var calls atomic.Int32
	fn := func(context.Context) (keys.Response, error) {
		calls.Add(1)
		return keys.Response{}, wantErr
	}
	if _, err := k.Do(ctx, req("k", `{}`), fn); !errors.Is(err, wantErr) || errors.Is(err, keys.ErrExhausted) {
		t.Fatalf("attempt 1: %v", err)
	}
	_, err := k.Do(ctx, req("k", `{}`), fn)
	if !errors.Is(err, wantErr) || !errors.Is(err, keys.ErrExhausted) || !durable.IsPermanent(err) {
		t.Fatalf("attempt 2: %v", err)
	}
	_, err = k.Do(ctx, req("k", `{}`), fn)
	if !errors.Is(err, keys.ErrExhausted) || !durable.IsPermanent(err) || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("after exhaustion: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("fn ran %d times", calls.Load())
	}
	c, err := k.Begin(ctx, req("k", `{}`))
	if err != nil || c.State != keys.Exhausted || c.Attempts != 2 || c.LastError != wantErr.Error() {
		t.Fatalf("begin: %+v %v", c, err)
	}
	// The request is still pinned to the key.
	if _, err := k.Begin(ctx, req("k", `{"other": 1}`)); !errors.Is(err, durable.ErrRequestMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
}

// A holder that crashes on its last attempt exhausts the key.
func TestMaxAttemptsAfterCrashedHolder(t *testing.T) {
	ctx := t.Context()
	gate := durabletest.NewGate()
	defer gate.Open()
	cfg := stalledConfig(gate)
	cfg.MaxAttempts = 1
	k, _ := setup(t, cfg)
	crash(t, k, "k")
	_, err := k.Do(ctx, req("k", `{}`), func(context.Context) (keys.Response, error) {
		t.Fatal("must not run")
		return keys.Response{}, nil
	})
	if !errors.Is(err, keys.ErrExhausted) || !strings.Contains(err.Error(), "lease expired") {
		t.Fatalf("want exhausted, got %v", err)
	}
}

// Callers that arrive before the backoff has passed get a BackoffError.
func TestBackoff(t *testing.T) {
	ctx := t.Context()
	const wait = 200 * time.Millisecond
	k, _ := setup(t, keys.Config{Backoff: func(int) time.Duration { return wait }})
	wantErr := errors.New("bad gateway")
	var calls atomic.Int32
	fn := func(context.Context) (keys.Response, error) {
		if calls.Add(1) == 1 {
			return keys.Response{}, wantErr
		}
		return keys.Response{Status: 200}, nil
	}
	start := time.Now()
	if _, err := k.Do(ctx, req("k", `{}`), fn); !errors.Is(err, wantErr) {
		t.Fatalf("attempt 1: %v", err)
	}
	_, err := k.Do(ctx, req("k", `{}`), fn)
	var be *keys.BackoffError
	if !errors.Is(err, keys.ErrBackoff) || !errors.As(err, &be) || be.LastError != wantErr.Error() {
		t.Fatalf("want backoff, got %v", err)
	}
	if be.RetryAfter.Before(start.Add(wait-20*time.Millisecond)) || be.RetryAfter.After(time.Now().Add(wait+time.Second)) {
		t.Fatalf("RetryAfter %v out of range", be.RetryAfter)
	}
	time.Sleep(time.Until(be.RetryAfter) + 20*time.Millisecond)
	resp, err := k.Do(ctx, req("k", `{}`), fn)
	if err != nil || resp.Status != 200 || calls.Load() != 2 {
		t.Fatalf("after backoff: %+v %v calls=%d", resp, err, calls.Load())
	}
}

// durable.RetryAfter on the error sets the backoff without a Backoff config.
func TestRetryAfterError(t *testing.T) {
	ctx := t.Context()
	k, _ := setup(t, keys.Config{})
	c, err := k.Begin(ctx, req("k", `{}`))
	if err != nil || c.State != keys.Acquired {
		t.Fatal(c, err)
	}
	if err := c.Abort(ctx, durable.RetryAfter(errors.New("429"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	c, err = k.Begin(ctx, req("k", `{}`))
	if err != nil || c.State != keys.Backoff || time.Until(c.RetryAfter) < 59*time.Minute || c.LastError != "429" {
		t.Fatalf("%+v %v", c, err)
	}
}

// Release gives the attempt back; Abort keeps it.
func TestReleaseRefundsAttempt(t *testing.T) {
	ctx := t.Context()
	k, _ := setup(t, keys.Config{MaxAttempts: 2})
	begin := func(want keys.State, attempts int) *keys.Claim {
		t.Helper()
		c, err := k.Begin(ctx, req("k", `{}`))
		if err != nil || c.State != want || c.Attempts != attempts {
			t.Fatalf("want %s/%d: %+v %v", want, attempts, c, err)
		}
		return c
	}
	if err := begin(keys.Acquired, 1).Abort(ctx, errors.New("first")); err != nil {
		t.Fatal(err)
	}
	if err := begin(keys.Acquired, 2).Release(ctx); err != nil {
		t.Fatal(err)
	}
	c := begin(keys.Acquired, 2)
	if c.LastError != "first" {
		t.Fatalf("LastError %q", c.LastError)
	}
	if err := c.Abort(ctx, errors.New("second")); err != nil {
		t.Fatal(err)
	}
	begin(keys.Exhausted, 2)
}

// A panicking handler counts as a failed attempt.
func TestDoPanicCountsAttempt(t *testing.T) {
	ctx := t.Context()
	k, _ := setup(t, keys.Config{MaxAttempts: 1})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		_, _ = k.Do(ctx, req("k", `{}`), func(context.Context) (keys.Response, error) { panic("boom") })
	}()
	c, err := k.Begin(ctx, req("k", `{}`))
	if err != nil || c.State != keys.Exhausted || c.LastError != "panic: boom" {
		t.Fatalf("after panic: %+v %v", c, err)
	}
}

// A cancelled request is still recorded as a failed attempt.
func TestCancelledAttemptIsRecorded(t *testing.T) {
	k, _ := setup(t, keys.Config{})
	ctx, cancel := context.WithCancel(t.Context())
	_, err := k.Do(ctx, req("k", `{}`), func(context.Context) (keys.Response, error) {
		cancel()
		return keys.Response{}, context.Cause(ctx)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancelled, got %v", err)
	}
	c, err := k.Begin(t.Context(), req("k", `{}`))
	if err != nil || c.State != keys.Acquired || c.Attempts != 2 || !strings.Contains(c.LastError, "canceled") {
		t.Fatalf("%+v %v", c, err)
	}
	_ = c.Release(t.Context())
}

// A retry resumes after the last finished step; finished steps' writes are
// committed exactly once.
func TestDoStepsResumesFromCheckpoint(t *testing.T) {
	ctx := t.Context()
	k, db := setup(t, keys.Config{})
	wantErr := errors.New("step2 failed")
	var steps []string
	var fails atomic.Int32
	fn := func(txCtx context.Context, cp keys.Checkpoint) (keys.Result, error) {
		steps = append(steps, cp.Name)
		insert := func(n int) error {
			_, err := db.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO payments (amount) VALUES ($1)`, n)
			return err
		}
		switch cp.Name {
		case "":
			return keys.Result{Checkpoint: &keys.Checkpoint{Name: "step1", Data: json.RawMessage(`{"n": 1}`)}}, insert(1)
		case "step1":
			return keys.Result{Checkpoint: &keys.Checkpoint{Name: "step2", Data: json.RawMessage(`{"n": 2}`)}}, insert(2)
		case "step2":
			if err := insert(3); err != nil {
				return keys.Result{}, err
			}
			if fails.Add(1) == 1 {
				return keys.Result{}, wantErr // rolls back this step's insert
			}
			return keys.Result{Response: &keys.Response{Status: 200, Body: cp.Data}}, nil
		}
		return keys.Result{}, errors.New("unexpected checkpoint " + cp.Name)
	}
	if _, err := k.DoSteps(ctx, req("k", `{}`), fn); !errors.Is(err, wantErr) {
		t.Fatalf("attempt 1: %v", err)
	}
	resp, err := k.DoSteps(ctx, req("k", `{}`), fn)
	if err != nil || resp.Status != 200 || !strings.Contains(string(resp.Body), `2`) {
		t.Fatalf("attempt 2: %+v %v", resp, err)
	}
	if want := []string{"", "step1", "step2", "step2"}; !slices.Equal(steps, want) {
		t.Fatalf("steps %q, want %q", steps, want)
	}
	var amounts []int
	rows, err := db.DB().QueryContext(ctx, `SELECT amount FROM payments ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		_ = rows.Scan(&n)
		amounts = append(amounts, n)
	}
	if !slices.Equal(amounts, []int{1, 2, 3}) {
		t.Fatalf("payments %v", amounts)
	}
	// Replays never run a step.
	resp, err = k.DoSteps(ctx, req("k", `{}`), func(context.Context, keys.Checkpoint) (keys.Result, error) {
		t.Fatal("must not run")
		return keys.Result{}, nil
	})
	if err != nil || resp.Status != 200 {
		t.Fatalf("replay: %+v %v", resp, err)
	}
}

// A step of a claim that was taken over cannot commit: its checkpoint and
// its writes roll back together.
func TestSaveCheckpointIsFenced(t *testing.T) {
	ctx := t.Context()
	gate := durabletest.NewGate()
	defer gate.Open()
	k, db := setup(t, stalledConfig(gate))
	stale := crash(t, k, "k")
	b, err := k.Begin(ctx, req("k", `{}`))
	if err != nil || b.State != keys.Acquired {
		t.Fatal(b, err)
	}
	gate.Open()
	err = db.RunInTx(ctx, func(txCtx context.Context) error {
		if _, err := db.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO payments (amount) VALUES (1)`); err != nil {
			return err
		}
		return stale.SaveCheckpoint(txCtx, keys.Checkpoint{Name: "stale"})
	})
	if !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("stale checkpoint: %v", err)
	}
	var n int
	if err := db.DB().QueryRowContext(ctx, `SELECT count(*) FROM payments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stale write committed: n=%d %v", n, err)
	}
	if err := db.RunInTx(ctx, func(txCtx context.Context) error {
		return b.SaveCheckpoint(txCtx, keys.Checkpoint{Name: "b1"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveCheckpoint(ctx, keys.Checkpoint{Name: "b2"}); !errors.Is(err, durable.ErrNotInTx) {
		t.Fatalf("outside tx: %v", err)
	}
	if err := b.Release(ctx); err != nil {
		t.Fatal(err)
	}
	// Release keeps a key with a checkpoint, and gives the attempt back.
	c, err := k.Begin(ctx, req("k", `{}`))
	if err != nil || c.State != keys.Acquired || c.Checkpoint.Name != "b1" || c.Attempts != 2 {
		t.Fatalf("resume: %+v %v", c, err)
	}
	_ = c.Release(ctx)
}

func TestDoStepsInvalidResult(t *testing.T) {
	ctx := t.Context()
	k, _ := setup(t, keys.Config{})
	for name, res := range map[string]keys.Result{
		"empty": {},
		"both":  {Checkpoint: &keys.Checkpoint{Name: "x"}, Response: &keys.Response{Status: 200}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := k.DoSteps(ctx, req(name, `{}`), func(context.Context, keys.Checkpoint) (keys.Result, error) {
				return res, nil
			})
			if !errors.Is(err, keys.ErrInvalidResult) {
				t.Fatalf("want ErrInvalidResult, got %v", err)
			}
			resp, err := k.Do(ctx, req(name, `{}`), func(context.Context) (keys.Response, error) {
				return keys.Response{Status: 200}, nil
			})
			if err != nil || resp.Status != 200 {
				t.Fatalf("retry: %+v %v", resp, err)
			}
		})
	}
}

func TestInvalidMaxAttempts(t *testing.T) {
	_, db := setup(t, keys.Config{})
	if _, err := keys.New(keys.Config{DB: db, MaxAttempts: -1}); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
}
