package dbtx

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrNotTransaction = errors.New("dbtx: underlying type is not a transaction")

// DBTX represents the common db operations for both *sql.DB and *sql.Tx.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// atomic represents the database atomic operations in a transactions.
type atomic interface {
	DB() DBTX
	DBTx(ctx context.Context) DBTX
	Tx(ctx context.Context) DBTX

	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}

// Ensures the struct DB implements the interface.
var _ atomic = (*DB)(nil)

// DB represents a unit of work.
type DB struct {
	db  *sql.DB
	fns []func(DBTX) DBTX
}

// New returns a pointer to DB.
func New(db *sql.DB, fns ...func(DBTX) DBTX) *DB {
	return &DB{
		db:  db,
		fns: fns,
	}
}

// DB returns the underlying *sql.DB as DBTX interface, to avoid the caller to
// init a new transaction.
// This also allows wrapping the *sql.DB with other implementations, such as
// recorder.
func (d *DB) DB() DBTX {
	return apply(d.db, d.fns...)
}

// DBTx returns the DBTX from the context, which can be either *sql.DB or
// *sql.Tx.
// Returns the atomic underlying type if the context is empty.
func (d *DB) DBTx(ctx context.Context) DBTX {
	if tx, ok := Value(ctx); ok {
		return tx
	}

	return d.DB()
}

// Tx returns the *sql.Tx from context. The return type is still a DBTX
// interface to avoid client from calling tx.Commit.
// When dealing with nested transaction, only the parent of the transaction can
// commit the transaction.
func (d *DB) Tx(ctx context.Context) DBTX {
	tx, ok := Value(ctx)
	if !ok {
		panic(ErrNotTransaction)
	}

	return tx
}

// RunInTx wraps the operation in a transaction. If a context containing tx is
// passed in, then it will use the context tx. Transaction cannot be nested.
// The transaction can only be committed by the parent.
func (d *DB) RunInTx(ctx context.Context, fn func(context.Context) error) (err error) {
	if IsTx(ctx) {
		return fn(ctx)
	}

	tx, err := d.db.BeginTx(ctx, TxOptions(ctx))
	if err != nil {
		return err
	}

	defer func() {
		if r := recover(); r != nil {
			txErr := tx.Rollback()
			if e, ok := r.(error); ok {
				panic(errors.Join(err, e, txErr))
			} else {
				panic(r)
			}
		}
	}()

	ctx = txCtxKey.WithValue(ctx, &Tx{
		tx:  tx,
		fns: d.fns,
	})
	if err := fn(ctx); err != nil {
		return errors.Join(tx.Rollback(), err)
	}

	return tx.Commit()
}

type Tx struct {
	tx  *sql.Tx
	fns []func(DBTX) DBTX
}

func (t *Tx) Tx() DBTX {
	return apply(t.tx, t.fns...)
}

func apply(dbtx DBTX, fns ...func(DBTX) DBTX) DBTX {
	for _, fn := range fns {
		dbtx = fn(dbtx)
	}

	return dbtx
}

func SetDefaults(db *sql.DB) {
	// https://www.alexedwards.net/blog/configuring-sqldb
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(1 * time.Hour)
	db.SetConnMaxIdleTime(5 * time.Minute)
}
