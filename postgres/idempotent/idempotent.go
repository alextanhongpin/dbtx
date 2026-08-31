package idempotent

import (
	"context"
	"errors"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/lock"
)

type fun[K, V any] = func(ctx context.Context, req K) (V, error)
type ifun[K, V any] = func(ctx context.Context, req K) (V, bool, error)
type keyfun[V any] = func(ctx context.Context, key V) (string, error)
type ttlfun[K, V any] = func(ctx context.Context, req K, res V) (time.Duration, error)

func Func[K, V any](fn fun[K, V], id *Idempotent, keyFn keyfun[K]) ifun[K, V] {
	return func(ctx context.Context, req K) (V, bool, error) {
		var zero V
		key, err := keyFn(ctx, req)
		if err != nil {
			return zero, false, err
		}
		return id.LoadOrCreate(ctx, key, func(ctx context.Context, req K) (V, time.Duration, error) {
			res, err := fn(ctx, req)
			if err != nil {
				return zero, 0, err
			}

			ttl, err := ttlfun(ctx, req, res)
			if err != nil {
				return zero, 0, err
			}
			return v, ttl, nil
		}, req)
	}
}

type Idempotent struct {
	*dbtx.DB
}

type dto[K, V any] struct {
	Request   K
	Response  V
	ExpiresAt *time.Time
}

func (l *Idempotent) LoadOrCreate[K, V any](ctx context.Context, key string, fn fun[K, V], req K) (curr V, loaded bool, err error) {
	v, err := l.Load[V](txCtx, key)
	if err == nil {
		if hash(req) != hash(v.Request) {
			return errors.New("mismatch")
		}
		loaded = true
		curr = v.Response

		return nil
	}
	if err != ErrNotExist {
		return err
	}
	var t V
	err = l.RunInTx(ctx, func(txCtx context.Context) error {
		if err := lock.Lock(txCtx, lock.NewStrKey(key)); err != nil {
			return err
		}
		// Check again.
		v, err := l.Load[V](txCtx, key)
		if err == nil {
			if hash(req) != hash(v.Request) {
				return errors.New("mismatch")
			}
			loaded = true
			curr = v.Response

			return nil
		}
		if err != ErrNotExist {
			return err
		}
		newVal, ttl, err := fn(txCtx, key)
		if err != nil {
			return err
		}
		if err := l.Store(txCtx, key, dto{Request: req, Response: newVal}, ttl); err != nil {
			return err
		}
		t = newVal
		loaded = false
		return nil
	})
	return t, loaded, err
}
