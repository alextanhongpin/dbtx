package dbtx

import (
	"context"
	"database/sql"
	"errors"
)

var (
	ErrNonTransaction    = errors.New("dbtx: underlying type is not a transaction")
	ErrIsTransaction     = errors.New("dbtx: underlying type is transaction")
	ErrNestedTransaction = errors.New("dbtx: transactions cannot be nested")
)

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
	IsTx() bool
	DBTx(ctx context.Context) DBTX
	DB() *sql.DB
	Tx(ctx context.Context) DBTX
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

// DBTx returns the DBTX from the context, which can be either *sql.DB or
// *sql.Tx.
// Returns the atomic underlying type if the context is empty.
func (a *Atomic) DBTx(ctx context.Context) DBTX {
	atm, ok := Value(ctx)
	if ok {
		return atm.underlying()
	}

	return a.underlying()
}

func (a *Atomic) DB() *sql.DB {
	if a.IsTx() {
		panic(ErrIsTransaction)
	}

	return a.db
}

// Tx returns the *sql.Tx from context. The return type is still a DBTX
// interface to avoid client from calling tx.Commit.
// When dealing with nested transaction, only the parent of the transaction can
// commit the transaction.
func (a *Atomic) Tx(ctx context.Context) DBTX {
	atm, ok := Value(ctx)
	if ok && atm.IsTx() {
		return atm.tx
	}

	panic(ErrNonTransaction)
}

// RunInTx wraps the operation in a transaction. If a context containing tx is
// passed in, then it will use the context tx. Transaction cannot be nested.
// The transaction can only be committed by the parent.
func (a *Atomic) RunInTx(ctx context.Context, fn func(context.Context) error) (err error) {
	if IsTx(ctx) {
		ctx = IncDepth(ctx)
		return fn(ctx)
	}

	if a.IsTx() {
		panic(ErrNestedTransaction)
	}

	return RunInTx(ctx, a.db, TxOptions(ctx), func(tx *sql.Tx) error {
		return fn(WithValue(ctx, NewTx(tx)))
	})
}

func RunInTx(ctx context.Context, db *sql.DB, opt *sql.TxOptions, fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, opt)
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
func (a *Atomic) underlying() DBTX {
	if a.IsTx() {
		return a.tx
	}

	return a.db
}

// IsTx returns true if the underlying type is a transaction.
func (a *Atomic) IsTx() bool {
	return a.tx != nil
}

// NewTx returns a Atomic with transaction.
func NewTx(tx *sql.Tx) *Atomic {
	return &Atomic{
		tx: tx,
	}
}
