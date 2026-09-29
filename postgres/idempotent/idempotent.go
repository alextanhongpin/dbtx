package idempotent

import (
	_ "embed"

	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/idempotent/internal/postgres"
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
	db *dbtx.DB
}

func New(db *sql.DB) *Idempotent {
	return &Idempotent{
		db: dbtx.New(db),
	}
}

func (i *Idempotent) Delete(ctx context.Context, key string) (bool, error) {
	q := postgres.New(i.db.DBTx(ctx))
	_, err := q.Delete(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
}

type idempotentHandler[K, V any] func(ctx context.Context, key string, req K) (V, bool, error)

func (h idempotentHandler[K, V]) LoadOrCreate(ctx context.Context, key string, req K) (V, bool, error) {
	return h(ctx, key, req)
}

func (i *Idempotent) Func[K, V any](fn fun[K, V]) idempotent[K, V] {
	return idempotentHandler[K, V](func(ctx context.Context, key string, req K) (V, bool, error) {
		c, err := i.db.RunInTx2(ctx, func(ctx context.Context) (*cached[V], error) {
			row, loaded, err := i.loadOrStore(ctx, key)
			if err != nil {
				return nil, err
			}

			reqb, err := json.Marshal(req)
			if err != nil {
				return nil, err
			}

			if loaded {
				want, err := hashBytes(row.Request)
				if err != nil {
					return nil, err
				}
				got, err := hashBytes(reqb)
				if err != nil {
					return nil, err
				}
				if want != got {
					return nil, ErrRequestConflict
				}
				var v V
				err = json.Unmarshal(row.Response, &v)
				if err != nil {
					return nil, err
				}

				return &cached[V]{
					Val: v,
					Hit: true,
				}, nil
			}
			res, err := fn(ctx, req)
			if err != nil {
				return nil, err
			}
			resb, err := json.Marshal(res)
			if err != nil {
				return nil, err
			}

			q := postgres.New(i.db.DBTx(ctx))
			err = q.Update(ctx, postgres.UpdateParams{
				Request:  reqb,
				Response: resb,
				Key:      key,
			})
			if err != nil {
				return nil, err
			}
			return &cached[V]{
				Val: res,
			}, nil
		})
		var zero V
		if err != nil {
			return zero, false, err
		}

		return c.Val, c.Hit, nil
	})
}

const insertStmt = `insert into dbtx.idempotency_keys(key, request, response)
values ($1, '{}', '{}')
on conflict (key) do select
returning key, request, response, created_at, (xmin::text = txid_current()::text) AS is_new`

func (i *Idempotent) loadOrStore(ctx context.Context, key string) (*postgres.DbtxIdempotencyKey, bool, error) {
	var row postgres.DbtxIdempotencyKey
	var stored bool
	err := i.db.DBTx(ctx).QueryRowContext(ctx, insertStmt, key).Scan(&row.Key, &row.Request, &row.Response, &row.CreatedAt, &stored)
	if err != nil {
		return nil, false, err
	}
	return &row, !stored, nil
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
