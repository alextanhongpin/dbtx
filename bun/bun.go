package bun

import (
	"context"

	"github.com/uptrace/bun"
)

type UOW interface {
	IsTx() bool
	DB(ctx context.Context) bun.IDB
	RunInTx(ctx context.Context, fn func(context.Context) error) error
}

// Ensures the struct UnitOfWork implements the interface.
var _ UOW = (*UnitOfWork)(nil)

type UnitOfWork struct {
	db *bun.DB
	tx *bun.Tx
	//db bun.IDB
}

func New(db *bun.DB) *UnitOfWork {
	return &UnitOfWork{
		db: db,
	}
}

func (uow *UnitOfWork) IsTx() bool {
	return uow.tx != nil && uow.db == nil
}

func (uow *UnitOfWork) DB(ctx context.Context) bun.IDB {
	uowCtx, ok := Value(ctx)
	if !ok {
		return uow.underlying()
	}

	return uowCtx.underlying()
}

func (uow *UnitOfWork) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	switch db := uow.DB(ctx).(type) {
	case *bun.DB:
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			ctx = WithValue(ctx, &UnitOfWork{tx: &tx})

			return fn(ctx)
		})
	case *bun.Tx:
		ctx = WithValue(ctx, uow)

		return fn(ctx)
	default:
		panic(ErrContextNotFound)
	}
}

func (uow *UnitOfWork) underlying() bun.IDB {
	if uow.db != nil {
		return uow.db
	}

	return uow.tx
}
