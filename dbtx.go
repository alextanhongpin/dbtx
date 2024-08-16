package dbtx

import (
	"context"
	"database/sql"
	"errors"
)

var ErrNotTransaction = errors.New("dbtx: underlying type is not a transaction")

// DBTX represents the common db operations for both *sql.DB and *sql.Tx.
type DBTX interface {
	Exec(query string, args ...any) (sql.Result, error)
	Prepare(query string) (*sql.Stmt, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row

	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// atomic represents the database atomic operations in a transactions.
type atomic interface {
	DBTx(ctx context.Context) DBTX
	DB() DBTX
	Tx(ctx context.Context) DBTX
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}

// Ensures the struct Atomic implements the interface.
var _ atomic = (*Atomic)(nil)

// Atomic represents a unit of work.
type Atomic struct {
	db  *sql.DB
	fns []func(DBTX) DBTX
}

// New returns a pointer to Atomic.
func New(db *sql.DB, fns ...func(DBTX) DBTX) *Atomic {
	return &Atomic{
		db:  db,
		fns: fns,
	}
}

// DB returns the underlying *sql.DB as DBTX interface, to avoid the caller to
// init a new transaction.
// This also allows wrapping the *sql.DB with other implementations, such as
// recorder.
func (a *Atomic) DB() DBTX {
	return apply(a.db, a.fns...)
}

// DBTx returns the DBTX from the context, which can be either *sql.DB or
// *sql.Tx.
// Returns the atomic underlying type if the context is empty.
func (a *Atomic) DBTx(ctx context.Context) DBTX {
	if tx, ok := Value(ctx); ok {
		return tx
	}

	return a.DB()
}

// Tx returns the *sql.Tx from context. The return type is still a DBTX
// interface to avoid client from calling tx.Commit.
// When dealing with nested transaction, only the parent of the transaction can
// commit the transaction.
func (a *Atomic) Tx(ctx context.Context) DBTX {
	tx, ok := Value(ctx)
	if !ok {
		panic(ErrNotTransaction)
	}

	return tx
}

// RunInTx wraps the operation in a transaction. If a context containing tx is
// passed in, then it will use the context tx. Transaction cannot be nested.
// The transaction can only be committed by the parent.
func (a *Atomic) RunInTx(ctx context.Context, fn func(context.Context) error) (err error) {
	if IsTx(ctx) {
		return fn(ctx)
	}

	tx, err := a.db.BeginTx(ctx, TxOptions(ctx))
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

type Tx struct {
	tx  *sql.Tx
	fns []func(DBTX) DBTX
}

func (t *Tx) underlying() DBTX {
	return apply(t.tx, t.fns...)
}

func apply(dbtx DBTX, fns ...func(DBTX) DBTX) DBTX {
	for _, fn := range fns {
		dbtx = fn(dbtx)
	}

	return dbtx
}
