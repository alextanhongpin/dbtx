package buntx

import (
	"context"
	"errors"

	"github.com/uptrace/bun"
)

var (
	ErrNonTransaction = errors.New("dbtx: underlying type is not a transaction")
	ErrIsTransaction  = errors.New("dbtx: underlying type is transaction")
)

// DBTX is an alias to bun.IDB.
type DBTX = bun.IDB

type atomic interface {
	DBTx(ctx context.Context) DBTX
	DB() DBTX
	Tx(ctx context.Context) DBTX
	RunInTx(ctx context.Context, fn func(context.Context) error) error
}

// Ensures the struct Atomic implements the interface.
var _ atomic = (*Atomic)(nil)

type Atomic struct {
	db  *bun.DB
	fns []func(DBTX) DBTX
}

func New(db *bun.DB, fns ...func(DBTX) DBTX) *Atomic {
	return &Atomic{
		db:  db,
		fns: fns,
	}
}

func (a *Atomic) DB() DBTX {
	return apply(a.db, a.fns...)
}

func (a *Atomic) DBTx(ctx context.Context) DBTX {
	if tx, ok := Value(ctx); ok {
		return tx
	}

	return a.DB()
}

func (a *Atomic) Tx(ctx context.Context) DBTX {
	tx, ok := Value(ctx)
	if !ok {
		panic(ErrNonTransaction)
	}

	return tx
}

func (a *Atomic) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	_, ok := value(ctx)
	if ok {
		return fn(ctx)
	}

	return a.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		ctx = withValue(ctx, &Tx{tx: &tx, fns: a.fns})

		return fn(ctx)
	})
}

func apply(dbtx DBTX, fns ...func(DBTX) DBTX) DBTX {
	for _, fn := range fns {
		dbtx = fn(dbtx)
	}

	return dbtx
}

type Tx struct {
	tx  *bun.Tx
	fns []func(DBTX) DBTX
}

func (t *Tx) underlying() DBTX {
	return apply(t.tx, t.fns...)
}
