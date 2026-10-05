package cache

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"time"
)

type fun[K, V any] = func(ctx context.Context, req K) (V, time.Duration, error)
type ifun[K, V any] = func(ctx context.Context, req K) (V, bool, error)
type keyfun[K any] = func(ctx context.Context, req K) (string, error)

type FuncConfig[K, V any] struct {
	Cache *Cache
	KeyFn keyfun[K]
}

func Func[K, V any](fn fun[K, V], cfg *FuncConfig[K, V]) ifun[K, V] {
	return func(ctx context.Context, req K) (V, bool, error) {
		var zero V
		key, err := cfg.KeyFn(ctx, req)
		if err != nil {
			return zero, false, err
		}
		return cfg.Cache.LoadOrCreate(ctx, key, func(ctx context.Context, _ string) (V, time.Duration, error) {
			res, ttl, err := fn(ctx, req)
			if err != nil {
				return zero, 0, err
			}

			return res, ttl, nil
		})
	}
}

func Idempotent[K, V any](fn fun[K, V], cfg *FuncConfig[K, V]) ifun[K, V] {
	type dto struct {
		Request  K `json:"request"`
		Response V `json:"response"`
	}

	return func(ctx context.Context, req K) (V, bool, error) {
		var zero V
		key, err := cfg.KeyFn(ctx, req)
		if err != nil {
			return zero, false, err
		}
		res, loaded, err := cfg.Cache.LoadOrCreate(ctx, key, func(ctx context.Context, _ string) (*dto, time.Duration, error) {
			res, ttl, err := fn(ctx, req)
			if err != nil {
				return nil, 0, err
			}

			return &dto{
				Request:  req,
				Response: res,
			}, ttl, nil
		})
		if err != nil {
			return zero, false, err
		}
		if loaded && res == nil {
			// The key holds null, for example from a Store of a nil value.
			return zero, false, fmt.Errorf("%w: key %q does not hold an idempotent result", ErrConflict, key)
		}
		if loaded && !jsonEqual(req, res.Request) {
			return zero, false, fmt.Errorf("%w: request", ErrConflict)
		}

		return res.Response, loaded, err
	}
}

func jsonEqual(a, b any) bool {
	c, err := jsonNorm(a)
	if err != nil {
		return false
	}
	d, err := jsonNorm(b)
	if err != nil {
		return false
	}
	return bytes.Equal(c, d)
}

func jsonNorm(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var a any
	err = json.Unmarshal(b, &a)
	if err != nil {
		return nil, err
	}
	return json.Marshal(a, json.Deterministic(true))
}
