package cache

import (
	"context"
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

func Idempotent() {}

// in transaction
func Set() {

}

func Del() {

}
