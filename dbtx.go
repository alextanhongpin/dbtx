package dbtx

import (
	"context"
	"database/sql"
)

// DB represents the common db operations.
type DB interface {
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
	IsTx() bool
	DB(ctx context.Context) DB
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}

// Ensures the struct Atomic implements the interface.
var _ atomic = (*Atomic)(nil)

// Atomic represents a unit of work.
type Atomic struct {
	tx *sql.Tx
	db *sql.DB
}

// New returns a pointer to Atomic.
func New(db *sql.DB) *Atomic {
	return &Atomic{
		db: db,
	}
}

// DB returns the underlying db from the context if provided, else returns the
// default Atomic.
func (a *Atomic) DB(ctx context.Context) DB {
	atmCtx, ok := Value(ctx)
	if ok {
		return atmCtx.underlying()
	}

	return a.underlying()
}

// RunInTx wraps the operation in a transaction. If a context containing tx is
// passed in, then it will use the context tx. Transaction cannot be nested.
// The transaction can only be committed by the parent.
func (a *Atomic) RunInTx(ctx context.Context, fn func(context.Context) error) (err error) {
	if IsTx(ctx) {
		return fn(ctx)
	}

	if a.IsTx() {
		return fn(WithValue(ctx, a))
	}

	tx, err := a.db.BeginTx(ctx, TxOptions(ctx))
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

	return fn(WithValue(ctx, newTx(tx)))
}

// underlying returns the underlying db client.
func (a *Atomic) underlying() DB {
	if a.IsTx() {
		return a.tx
	}

	return a.db
}

// IsTx returns true if the underlying type is a transaction.
func (a *Atomic) IsTx() bool {
	return a.tx != nil
}

// newTx returns a Atomic with transaction.
func newTx(tx *sql.Tx) *Atomic {
	return &Atomic{
		tx: tx,
	}
}
