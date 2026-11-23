package cache

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"

	"github.com/alextanhongpin/dbtx/postgres/durable"
)

// FuncConfig configures Func and Idempotent.
type FuncConfig[K any] struct {
	Cache   *Cache
	Key     func(ctx context.Context, req K) (string, error) // cache key for req; required
	Options Options                                          // passed to GetOrCompute
}

func (cfg FuncConfig[K]) validate() error {
	if cfg.Cache == nil || cfg.Key == nil {
		return fmt.Errorf("%w: Cache and Key are required", durable.ErrInvalidConfig)
	}
	return nil
}

// Func memoizes fn through GetOrCompute, with its stampede protection and
// stale-while-revalidate. The returned function reports loaded=true when the
// value came from the cache (fresh or stale) and false when this call
// computed it.
func Func[K, V any](cfg FuncConfig[K], fn func(ctx context.Context, req K) (V, error)) func(ctx context.Context, req K) (v V, loaded bool, err error) {
	return func(ctx context.Context, req K) (V, bool, error) {
		var zero V
		if err := cfg.validate(); err != nil {
			return zero, false, err
		}
		key, err := cfg.Key(ctx, req)
		if err != nil {
			return zero, false, err
		}
		computed := false
		v, err := cfg.Cache.GetOrCompute(ctx, key, cfg.Options, func(ctx context.Context) (V, error) {
			computed = true
			return fn(ctx, req)
		})
		return v, err == nil && !computed, err
	}
}

// idempotent is what Idempotent stores: the response with the request that
// produced it.
type idempotent[K, V any] struct {
	Request  K `json:"request"`
	Response V `json:"response"`
}

// Idempotent is Func that also stores the request with the response. A
// cached response is returned only for an equal request (compared as
// canonical JSON); a different request under the same key returns
// durable.ErrRequestMismatch.
func Idempotent[K, V any](cfg FuncConfig[K], fn func(ctx context.Context, req K) (V, error)) func(ctx context.Context, req K) (v V, loaded bool, err error) {
	call := Func(cfg, func(ctx context.Context, req K) (*idempotent[K, V], error) {
		res, err := fn(ctx, req)
		if err != nil {
			return nil, err
		}
		return &idempotent[K, V]{Request: req, Response: res}, nil
	})
	return func(ctx context.Context, req K) (V, bool, error) {
		var zero V
		res, loaded, err := call(ctx, req)
		if err != nil {
			return zero, false, err
		}
		if res == nil {
			// The key holds null, for example from a Set of a nil value.
			return zero, false, fmt.Errorf("%w: key does not hold an idempotent result", durable.ErrRequestMismatch)
		}
		if loaded && !jsonEqual(req, res.Request) {
			return zero, false, durable.ErrRequestMismatch
		}
		return res.Response, loaded, nil
	}
}

func jsonEqual(a, b any) bool {
	x, err := jsonNorm(a)
	if err != nil {
		return false
	}
	y, err := jsonNorm(b)
	return err == nil && bytes.Equal(x, y)
}

// jsonNorm returns a canonical JSON encoding of v: object members sorted and
// whitespace removed, so requests that differ only in key order or
// formatting (such as raw JSON read back from jsonb) compare equal. Integers
// keep their exact digits instead of rounding through float64.
func jsonNorm(v any) ([]byte, error) {
	b, err := jsonv2.Marshal(v, jsonv2.Deterministic(true))
	if err != nil {
		return nil, err
	}
	val := jsontext.Value(b)
	if err := val.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, err
	}
	return val, nil
}
