package sqlxtx

import (
	"context"
	"errors"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

var ErrNotTransaction = errors.New("sqltx: underlying type is not a transaction")

// DBTX represents the common db operations for both *sql.DB and *sql.Tx.
type DBTX = sqlx.ExtContext

// atomic represents the database atomic operations in a transactions.
type atomic interface {
	DBTx(ctx context.Context) DBTX
	DB() DBTX
	Tx(ctx context.Context) DBTX
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}

type Atomic struct {
	db  *sqlx.DB
	fns []func(DBTX) DBTX
}

var _ atomic = (*Atomic)(nil)

func New(db *sqlx.DB, fns ...func(DBTX) DBTX) *Atomic {
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
		panic(ErrNotTransaction)
	}

	return tx
}

func (a *Atomic) RunInTx(ctx context.Context, fn func(context.Context) error) (err error) {
	if IsTx(ctx) {
		return fn(ctx)
	}

	tx, err := a.db.BeginTxx(ctx, TxOptions(ctx))
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ctx = withValue(ctx, &Tx{tx: tx, fns: a.fns})
	if err := fn(ctx); err != nil {
		return err
	}

	return tx.Commit()
}

func apply(dbtx DBTX, fns ...func(DBTX) DBTX) DBTX {
	for _, fn := range fns {
		dbtx = fn(dbtx)
	}

	return dbtx
}

type Tx struct {
	tx  *sqlx.Tx
	fns []func(DBTX) DBTX
}

func (t *Tx) underlying() DBTX {
	return apply(t.tx, t.fns...)
}
