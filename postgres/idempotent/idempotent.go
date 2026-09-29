package idempotent

import (
	_ "embed"

	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/idempotent/internal"
)

//go:embed internal/schema.sql
var Schema string

var (
	ErrNotFound        = internal.ErrNotFound
	ErrRequestConflict = internal.ErrRequestConflict
)

// Handler executes a request at most once per idempotency key.
type Handler[K, V any] interface {
	LoadOrCreate(ctx context.Context, key string, req K) (V, bool, error)
}

type Idempotent struct {
	repo Repository
}

type IdempotencyKey = internal.IdempotencyKey

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository = internal.Repository

var NewRepository = internal.NewRepository

var _ Repository = (*PostgresRepository)(nil)

// Repository stores idempotency keys. Keys are unique per scope.
type Repository interface {
	Delete(ctx context.Context, scope, key string) (*IdempotencyKey, error)
	DeleteBefore(ctx context.Context, t time.Time) (int64, error)
	LoadOrStore(ctx context.Context, scope, key string, req jsontext.Value) (*IdempotencyKey, bool, error)
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error
	Update(ctx context.Context, scope, key string, res jsontext.Value) error
}

func New(repo Repository) *Idempotent {
	return &Idempotent{
		repo: repo,
	}
}

// Delete removes the idempotency key in the given scope. It returns
// ErrNotFound if the key does not exist.
func (i *Idempotent) Delete(ctx context.Context, scope, key string) (*IdempotencyKey, error) {
	return i.repo.Delete(ctx, scope, key)
}

// DeleteBefore removes all idempotency keys created before t, and returns the
// number of keys removed. Call it periodically to keep the table small.
func (i *Idempotent) DeleteBefore(ctx context.Context, t time.Time) (int64, error) {
	return i.repo.DeleteBefore(ctx, t)
}

type handlerFunc[K, V any] func(ctx context.Context, key string, req K) (V, bool, error)

func (h handlerFunc[K, V]) LoadOrCreate(ctx context.Context, key string, req K) (V, bool, error) {
	return h(ctx, key, req)
}

// Func wraps fn so that it is executed at most once per key. Keys are unique
// per scope, so handlers with different scopes can use the same key without
// conflicting. It panics if scope is empty.
//
// fn runs inside the same transaction that holds the idempotency key, and the
// ctx passed to fn carries that transaction, so writes made through it commit
// or roll back together with the key. Concurrent calls with the same key block
// until the first call completes.
//
// If fn returns an error, the transaction is rolled back and nothing is
// stored, so the next call with the same key executes fn again.
func (i *Idempotent) Func[K, V any](scope string, fn func(ctx context.Context, req K) (V, error)) Handler[K, V] {
	if scope == "" {
		panic("idempotent: scope must not be empty")
	}

	return handlerFunc[K, V](func(ctx context.Context, key string, req K) (res V, loaded bool, err error) {
		reqb, err := json.Marshal(req)
		if err != nil {
			var zero V
			return zero, false, err
		}

		err = i.repo.RunInTx(ctx, func(ctx context.Context) error {
			row, ok, err := i.repo.LoadOrStore(ctx, scope, key, reqb)
			if err != nil {
				return err
			}
			if ok {
				loaded = true
				return json.Unmarshal(row.Response, &res)
			}

			res, err = fn(ctx, req)
			if err != nil {
				return err
			}

			resb, err := json.Marshal(res)
			if err != nil {
				return err
			}

			return i.repo.Update(ctx, scope, key, resb)
		})
		if err != nil {
			var zero V
			return zero, false, err
		}

		return res, loaded, nil
	})
}
