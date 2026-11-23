package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/durable"
	"github.com/alextanhongpin/dbtx/postgres/durable/cache"
)

func TestFunc(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	var calls atomic.Int32
	upstream := errors.New("upstream")
	double := cache.Func(cache.FuncConfig[int]{
		Cache:   c,
		Key:     func(_ context.Context, n int) (string, error) { return "double:" + strconv.Itoa(n), nil },
		Options: cache.Options{TTL: time.Minute},
	}, func(_ context.Context, n int) (int, error) {
		calls.Add(1)
		if n == 9 {
			return 0, upstream
		}
		return n * 2, nil
	})

	v, loaded, err := double(ctx, 2)
	if err != nil || loaded || v != 4 {
		t.Fatalf("first: %v %v %v", v, loaded, err)
	}
	v, loaded, err = double(ctx, 2)
	if err != nil || !loaded || v != 4 {
		t.Fatalf("second: %v %v %v", v, loaded, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("computed %d times", calls.Load())
	}
	if _, loaded, err := double(ctx, 9); !errors.Is(err, upstream) || loaded {
		t.Fatalf("error: %v %v", loaded, err)
	}
	if ok, _ := c.Exists(ctx, "double:9"); ok {
		t.Fatal("error was cached")
	}

	keyErr := errors.New("bad key")
	bad := cache.Func(cache.FuncConfig[int]{
		Cache: c, Key: func(context.Context, int) (string, error) { return "", keyErr },
		Options: cache.Options{TTL: time.Minute},
	}, func(context.Context, int) (int, error) { return 0, nil })
	if _, _, err := bad(ctx, 1); !errors.Is(err, keyErr) {
		t.Fatalf("key error: %v", err)
	}
	none := cache.Func(cache.FuncConfig[int]{}, func(context.Context, int) (int, error) { return 0, nil })
	if _, _, err := none(ctx, 1); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("invalid config: %v", err)
	}
}

func TestIdempotent(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)
	type createUser struct {
		IID  string
		Name string
	}
	type user struct{ ID, Name string }
	var calls atomic.Int32
	create := cache.Idempotent(cache.FuncConfig[createUser]{
		Cache:   c,
		Key:     func(_ context.Context, r createUser) (string, error) { return "user:" + r.IID, nil },
		Options: cache.Options{TTL: time.Minute},
	}, func(_ context.Context, r createUser) (*user, error) {
		calls.Add(1)
		return &user{ID: r.IID, Name: r.Name}, nil
	})

	req := createUser{IID: "1", Name: "alice"}
	u, loaded, err := create(ctx, req)
	if err != nil || loaded || u.Name != "alice" {
		t.Fatalf("first: %v %v %v", u, loaded, err)
	}
	u, loaded, err = create(ctx, req)
	if err != nil || !loaded || u.Name != "alice" || calls.Load() != 1 {
		t.Fatalf("replay: %v %v %v (calls %d)", u, loaded, err, calls.Load())
	}
	if _, _, err := create(ctx, createUser{IID: "1", Name: "bob"}); !errors.Is(err, durable.ErrRequestMismatch) {
		t.Fatalf("mismatch: %v", err)
	}

	// A key that holds null is a mismatch, not a panic.
	_ = c.Set[*user](ctx, "user:2", nil, time.Minute, 0)
	if _, _, err := create(ctx, createUser{IID: "2"}); !errors.Is(err, durable.ErrRequestMismatch) {
		t.Fatalf("null: %v", err)
	}
}

func TestIdempotentRequestEquality(t *testing.T) {
	ctx := t.Context()
	c, _ := setup(t)

	t.Run("large integers", func(t *testing.T) {
		type transfer struct{ AccountID int64 }
		idp := cache.Idempotent(cache.FuncConfig[transfer]{
			Cache: c, Key: func(context.Context, transfer) (string, error) { return "transfer", nil },
			Options: cache.Options{TTL: time.Minute},
		}, func(_ context.Context, r transfer) (int64, error) { return r.AccountID, nil })
		if _, _, err := idp(ctx, transfer{AccountID: 9007199254740993}); err != nil {
			t.Fatal(err)
		}
		// Differs from the first request only beyond float64 precision.
		if _, _, err := idp(ctx, transfer{AccountID: 9007199254740992}); !errors.Is(err, durable.ErrRequestMismatch) {
			t.Fatalf("want mismatch, got %v", err)
		}
	})

	t.Run("raw json formatting", func(t *testing.T) {
		var calls atomic.Int32
		idp := cache.Idempotent(cache.FuncConfig[json.RawMessage]{
			Cache: c, Key: func(context.Context, json.RawMessage) (string, error) { return "raw", nil },
			Options: cache.Options{TTL: time.Minute},
		}, func(context.Context, json.RawMessage) (string, error) {
			calls.Add(1)
			return "ok", nil
		})
		if _, _, err := idp(ctx, json.RawMessage(`{"b": 1, "a": 9007199254740993}`)); err != nil {
			t.Fatal(err)
		}
		// The same request, formatted differently.
		res, loaded, err := idp(ctx, json.RawMessage(`{ "a":9007199254740993,"b":1 }`))
		if err != nil || !loaded || res != "ok" || calls.Load() != 1 {
			t.Fatalf("%v %v %v (calls %d)", res, loaded, err, calls.Load())
		}
	})
}
