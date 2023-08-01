package sqlxtx

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
)

var (
	ErrIsTransaction     = errors.New("sqltx: underlying type is transaction")
	ErrNestedTransaction = errors.New("sqltx: transactions cannot be nested")
	ErrNonTransaction    = errors.New("sqltx: underlying type is not a transaction")
)

// DBTX represents the common db operations for both *sql.DB and *sql.Tx.
type DBTX = sqlx.ExtContext

// atomic represents the database atomic operations in a transactions.
type atomic interface {
	IsTx() bool
	DBTx(ctx context.Context) DBTX
	DB(ctx context.Context) DBTX
	Tx(ctx context.Context) DBTX
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}

type Atomic struct {
	tx   *sqlx.Tx
	db   *sqlx.DB
	opts []Option
}

var _ atomic = (*Atomic)(nil)

func New(db *sqlx.DB, opts ...Option) *Atomic {
	return &Atomic{
		db:   db,
		opts: opts,
	}
}

func (a *Atomic) DBTx(ctx context.Context) DBTX {
	atm, ok := Value(ctx)
	if ok {
		return atm.underlying(ctx)
	}

	return a.underlying(ctx)
}

func (a *Atomic) DB(ctx context.Context) DBTX {
	if a.IsTx() {
		panic(ErrIsTransaction)
	}

	return a.underlying(ctx)
}

func (a *Atomic) Tx(ctx context.Context) DBTX {
	atm, ok := Value(ctx)
	if ok && atm.IsTx() {
		return atm.underlying(ctx)
	}

	panic(ErrNonTransaction)
}

func (a *Atomic) RunInTx(ctx context.Context, fn func(context.Context) error) (err error) {
	if IsTx(ctx) {
		return fn(ctx)
	}

	if a.IsTx() {
		panic(ErrNestedTransaction)
	}

	return RunInTx(ctx, a.db, TxOptions(ctx), func(tx *sqlx.Tx) error {
		return fn(WithValue(ctx, NewTx(tx, a.opts...)))
	})
}

func RunInTx(ctx context.Context, db *sqlx.DB, opt *sql.TxOptions, fn func(tx *sqlx.Tx) error) (err error) {
	tx, err := db.BeginTxx(ctx, opt)
	if err != nil {
		return err
	}

	defer func() {
		if p := recover(); p != nil {
			// A panic occur, rollback and repanic.
			err = tx.Rollback()
			panic(p)
		} else if err != nil {
			// Something went wrong, rollback, but keep the original error.
			_ = tx.Rollback()
		} else {
			// Success, commit.
			err = tx.Commit()
		}
	}()

	return fn(tx)
}

// underlying returns the underlying db client.
func (a *Atomic) underlying(ctx context.Context) DBTX {
	if a.IsTx() {
		return a.apply(a.tx)
	}

	return a.apply(a.db)
}

func (a *Atomic) apply(dbtx DBTX) DBTX {
	for _, opt := range a.opts {
		switch t := (opt).(type) {
		case Middleware:
			dbtx = t(dbtx)
		}
	}

	return dbtx
}

// IsTx returns true if the underlying type is a transaction.
func (a *Atomic) IsTx() bool {
	return a.tx != nil
}

// NewTx returns a Atomic with transaction.
func NewTx(tx *sqlx.Tx, opts ...Option) *Atomic {
	return &Atomic{
		tx:   tx,
		opts: opts,
	}
}

type Option interface {
	isOption()
}

type Middleware func(DBTX) DBTX

func (Middleware) isOption() {}
