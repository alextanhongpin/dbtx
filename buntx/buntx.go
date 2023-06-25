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
	IsTx() bool
	DBTx(ctx context.Context) DBTX
	DB(ctx context.Context) DBTX
	Tx(ctx context.Context) DBTX
	RunInTx(ctx context.Context, fn func(context.Context) error) error
}

// Ensures the struct Atomic implements the interface.
var _ atomic = (*Atomic)(nil)

type Atomic struct {
	db *bun.DB
	tx *bun.Tx
}

func New(db *bun.DB) *Atomic {
	return &Atomic{
		db: db,
	}
}

func (a *Atomic) IsTx() bool {
	return a.tx != nil && a.db == nil
}

func (a *Atomic) DBTx(ctx context.Context) bun.IDB {
	atm, ok := Value(ctx)
	if ok && atm.IsTx() {
		return atm.underlying()
	}

	return a.underlying()
}

func (a *Atomic) Tx(ctx context.Context) bun.IDB {
	atm, ok := Value(ctx)
	if ok && atm.IsTx() {
		return atm.underlying()
	}

	panic(ErrNonTransaction)
}

func (a *Atomic) DB(ctx context.Context) bun.IDB {
	if a.IsTx() {
		panic(ErrIsTransaction)
	}

	return a.underlying()
}

func (a *Atomic) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	switch db := a.DBTx(ctx).(type) {
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
	if a.IsTx() {
		return a.tx
	}

	return a.db
}
