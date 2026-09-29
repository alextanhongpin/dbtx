package idempotent

import (
	_ "embed"

	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"github.com/alextanhongpin/dbtx/postgres/idempotent/internal"
	"github.com/zeebo/xxh3"
)

//go:embed internal/schema.sql
var Schema string

var ErrRequestConflict = errors.New("request conflict")

type fun[K, V any] = func(ctx context.Context, req K) (V, error)

type idempotent[K, V any] interface {
	LoadOrCreate(ctx context.Context, key string, req K) (V, bool, error)
}

type cached[T any] struct {
	Hit bool
	Val T
}

type Idempotent struct {
	repo repository
}

type IdempotencyKey = internal.IdempotencyKey

type Repository = internal.Repository

var NewRepository = internal.NewRepository

type repository interface {
	Delete(ctx context.Context, key string) (*IdempotencyKey, error)
	LoadOrStore(ctx context.Context, key string) (*IdempotencyKey, bool, error)
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error
	Update(ctx context.Context, key string, req, res jsontext.Value) error
}

func New(repo repository) *Idempotent {
	return &Idempotent{
		repo: repo,
	}
}

func (i *Idempotent) Delete(ctx context.Context, key string) (*IdempotencyKey, error) {
	return i.repo.Delete(ctx, key)
}

type idempotentHandler[K, V any] func(ctx context.Context, key string, req K) (V, bool, error)

func (h idempotentHandler[K, V]) LoadOrCreate(ctx context.Context, key string, req K) (V, bool, error) {
	return h(ctx, key, req)
}

func (i *Idempotent) Func[K, V any](fn fun[K, V]) idempotent[K, V] {
	return idempotentHandler[K, V](func(ctx context.Context, key string, req K) (V, bool, error) {
		var c *cached[V]
		err := i.repo.RunInTx(ctx, func(ctx context.Context) error {
			row, loaded, err := i.repo.LoadOrStore(ctx, key)
			if err != nil {
				return err
			}

			reqb, err := json.Marshal(req)
			if err != nil {
				return err
			}

			if loaded {
				want, err := hashBytes(row.Request)
				if err != nil {
					return err
				}
				got, err := hashBytes(reqb)
				if err != nil {
					return err
				}
				if want != got {
					return ErrRequestConflict
				}
				var v V
				err = json.Unmarshal(row.Response, &v)
				if err != nil {
					return err
				}

				c = &cached[V]{
					Val: v,
					Hit: true,
				}
				return nil
			}
			res, err := fn(ctx, req)
			if err != nil {
				return err
			}
			resb, err := json.Marshal(res)
			if err != nil {
				return err
			}

			err = i.repo.Update(ctx, key, reqb, resb)
			if err != nil {
				return err
			}
			c = &cached[V]{
				Val: res,
			}
			return nil
		})
		var zero V
		if err != nil {
			return zero, false, err
		}

		return c.Val, c.Hit, nil
	})
}

func hashBytes(b []byte) (uint64, error) {
	b, err := orderedBytes(b)
	if err != nil {
		return 0, err
	}

	return xxh3.Hash(b), nil
}

func orderedBytes(b []byte) ([]byte, error) {
	// Unmarshal to map[string]any.
	var a any
	err := json.Unmarshal(b, &a)
	if err != nil {
		return nil, err
	}

	// Marshal with deterministic ordering for map.
	return json.Marshal(a, json.Deterministic(true))
}
