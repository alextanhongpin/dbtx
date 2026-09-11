package dbtx

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
	"uuid"

	"github.com/lib/pq"
)

var (
	ErrOutOfTx        = errors.New("dbtx: running outside of transaction boundary")
	ErrNotTransaction = errors.New("dbtx: underlying type is not a transaction")
)

// DBTX represents the common db operations for both *sql.DB and *sql.Tx.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// UnitOfWork represents the database UnitOfWork operations in a transactions.
type UnitOfWork interface {
	DB() DBTX
	DBTx(ctx context.Context) DBTX
	Tx(ctx context.Context) DBTX

	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error
}

// Ensures the struct DB implements the interface.
var _ UnitOfWork = (*DB)(nil)

// DB represents a unit of work.
type DB struct {
	id  string
	db  *sql.DB
	fns []func(DBTX) DBTX
}

// New returns a pointer to DB.
func New(db *sql.DB, fns ...func(DBTX) DBTX) *DB {
	return &DB{
		id:  ID,
		db:  db,
		fns: fns,
	}
}

func (d *DB) ID() string {
	return d.id
}

func (d *DB) SetID(id string) {
	d.id = id
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
// Returns the UnitOfWork underlying type if the context is empty.
func (d *DB) DBTx(ctx context.Context) DBTX {
	if tx, ok := NamedValue(ctx, d.id); ok {
		return tx
	}

	return d.DB()
}

// Tx returns the *sql.Tx from context. The return type is still a DBTX
// interface to avoid client from calling tx.Commit.
// When dealing with nested transaction, only the parent of the transaction can
// commit the transaction.
func (d *DB) Tx(ctx context.Context) DBTX {
	tx, ok := NamedValue(ctx, d.id)
	if !ok {
		panic(ErrNotTransaction)
	}

	return tx
}

// RunInTx wraps the operation in a transaction. If a context containing tx is
// passed in, then it will use the context tx. Transaction cannot be nested.
// The transaction can only be committed by the parent.
func (d *DB) RunInTx(ctx context.Context, fn func(context.Context) error) (err error) {
	return d.runInNamedTx(ctx, d.id, fn)
}

func (d *DB) RunInSubTx(ctx context.Context, fn func(context.Context) error) (err error) {
	return d.runInNamedSubTx(ctx, d.id, fn)
}

func (d *DB) RunInTx2[T any](ctx context.Context, fn func(context.Context) (T, error)) (res T, err error) {
	return d.runInNamedTx2(ctx, d.id, fn)
}

func (d *DB) RunInSubTx2[T any](ctx context.Context, fn func(context.Context) (T, error)) (res T, err error) {
	return d.runInNamedSubTx2(ctx, d.id, fn)
}

func (d *DB) runInNamedTx(ctx context.Context, name string, fn func(context.Context) error) (err error) {
	if IsNamedTx(ctx, name) {
		return fn(ctx)
	}

	tx, err := d.db.BeginTx(ctx, NamedTxOptions(ctx, name))
	if err != nil {
		return err
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		} else if err != nil {
			_ = tx.Rollback()
		}
	}()

	ctx = WithNamedValue(ctx, name, &Tx{
		tx:  tx,
		fns: d.fns,
	})
	if err := fn(ctx); err != nil {
		return err
	}

	return tx.Commit()
}

func (d *DB) runInNamedTx2[T any](ctx context.Context, name string, fn func(context.Context) (T, error)) (res T, err error) {
	if IsNamedTx(ctx, name) {
		return fn(ctx)
	}
	var zero T
	tx, err := d.db.BeginTx(ctx, NamedTxOptions(ctx, name))
	if err != nil {
		return zero, err
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		} else if err != nil {
			_ = tx.Rollback()
		}
	}()

	ctx = WithNamedValue(ctx, name, &Tx{
		tx:  tx,
		fns: d.fns,
	})
	res, err = fn(ctx)
	if err != nil {
		return zero, err
	}

	if err := tx.Commit(); err != nil {
		return zero, err
	}

	return res, nil
}

func (d *DB) runInNamedSubTx(ctx context.Context, name string, fn func(context.Context) error) (err error) {
	tx, ok := NamedValue(ctx, name)
	if !ok {
		return ErrOutOfTx
	}

	var once sync.Once
	id := uuid.NewV7().String()
	rollback := func() (err error) {
		once.Do(func() {
			_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+pq.QuoteIdentifier(id))
		})
		return err
	}
	release := func() (err error) {
		once.Do(func() {
			_, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT "+pq.QuoteIdentifier(id))
		})
		return err
	}

	_, err = tx.ExecContext(ctx, "SAVEPOINT "+pq.QuoteIdentifier(id))
	if err != nil {
		return err
	}
	defer func() {
		_ = rollback()
	}()

	err = fn(ctx)
	if err != nil {
		return err
	}
	return release()
}

func (d *DB) runInNamedSubTx2[T any](ctx context.Context, name string, fn func(context.Context) (T, error)) (zero T, err error) {
	tx, ok := NamedValue(ctx, name)
	if !ok {
		return zero, ErrOutOfTx
	}

	var once sync.Once
	id := uuid.NewV7().String()
	rollback := func() (err error) {
		once.Do(func() {
			_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+pq.QuoteIdentifier(id))
		})
		return err
	}
	release := func() (err error) {
		once.Do(func() {
			_, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT "+pq.QuoteIdentifier(id))
		})
		return err
	}

	_, err = tx.ExecContext(ctx, "SAVEPOINT "+pq.QuoteIdentifier(id))
	if err != nil {
		return zero, err
	}
	defer func() {
		_ = rollback()
	}()

	res, err := fn(ctx)
	if err != nil {
		return zero, err
	}
	if err := release(); err != nil {
		return zero, err
	}
	return res, nil
}

func (d *DB) Unwrap() *sql.DB {
	return d.db
}

func (d *DB) IsTx(ctx context.Context) bool {
	return IsNamedTx(ctx, d.id)
}

type Tx struct {
	tx  *sql.Tx
	fns []func(DBTX) DBTX
}

func (t *Tx) Tx() DBTX {
	return apply(t.tx, t.fns...)
}

func (t *Tx) Unwrap() *sql.Tx {
	return t.tx
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
