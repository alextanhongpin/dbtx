package buntx

import (
	"context"

	"github.com/uptrace/bun"
)

// DB is an alias to bun.IDB.
type DB = bun.IDB

type atomic interface {
	IsTx() bool
	DB(ctx context.Context) DB
	RunInTx(ctx context.Context, fn func(context.Context) error) error
}

// Ensures the struct Atomic implements the interface.
var _ atomic = (*Atomic)(nil)

type Atomic struct {
	db *bun.DB
	tx *bun.Tx
	//db bun.IDB
}

func New(db *bun.DB) *Atomic {
	return &Atomic{
		db: db,
	}
}

func (a *Atomic) IsTx() bool {
	return a.tx != nil && a.db == nil
}

func (a *Atomic) DB(ctx context.Context) bun.IDB {
	atmCtx, ok := Value(ctx)
	if !ok {
		return a.underlying()
	}

	return atmCtx.underlying()
}

func (a *Atomic) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	switch db := a.DB(ctx).(type) {
	case *bun.DB:
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			ctx = WithValue(ctx, &Atomic{tx: &tx})

			return fn(ctx)
		})
	case *bun.Tx:
		ctx = WithValue(ctx, a)

		return fn(ctx)
	default:
		panic(ErrContextNotFound)
	}
}

func (a *Atomic) underlying() bun.IDB {
	if a.db != nil {
		return a.db
	}

	return a.tx
}
