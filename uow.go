package uow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Errors
var (
	ErrNestedTransaction = errors.New("uow: transaction cannot be nested")
	ErrLockWithoutTx     = errors.New("uow: lock must be carried out in transaction")
)

// UowContext represents the key for the context containing the pointer of UnitOfWork.
var UowContext = key[*UnitOfWork]("uow")

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

// UOW represents the operations by UnitOfWork.
type UOW interface {
	DB(ctx context.Context) DB
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error, opts ...Option) (err error)
}

// Ensures the struct UnitOfWork implements the interface.
var _ UOW = (*UnitOfWork)(nil)

// UnitOfWork represents a unit of work.
type UnitOfWork struct {
	tx *sql.Tx
	db *sql.DB
}

// New returns a pointer to UnitOfWork.
func New(db *sql.DB) *UnitOfWork {
	return &UnitOfWork{
		db: db,
	}
}

// DB returns the underlying db from the context if provided, else returns the
// default UoW.
func (uow *UnitOfWork) DB(ctx context.Context) DB {
	uowCtx, ok := UowContext.Value(ctx)
	if ok {
		return uowCtx.underlying()
	}

	return uow.underlying()
}

// RunInTx wraps the operation in a transaction. If a context containing tx is
// passed in, then it will use the context tx. Transaction cannot be nested.
func (uow *UnitOfWork) RunInTx(ctx context.Context, fn func(context.Context) error, opts ...Option) (err error) {
	if isTxContext(ctx) {
		return fn(ctx)
	}

	if uow.isTx() {
		return ErrNestedTransaction
	}

	tx, err := uow.db.BeginTx(ctx, getUowOptions(opts...).Tx)
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

	txCtx := UowContext.WithValue(ctx, newTx(tx))
	return fn(txCtx)
}

// Lock locks the given key. If multiple operations lock the same key, it
// will wait for the previous operation to complete.
// Lock must be run within a transaction context, panics otherwise.
func Lock(ctx context.Context, key LockKey) error {
	uowCtx := UowContext.MustValue(ctx)
	if !uowCtx.isTx() {
		return fmt.Errorf("%w: %s", ErrLockWithoutTx, key)
	}

	tx := uowCtx.tx

	switch v := key.(type) {
	case *intLockKey:
		_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, v.m, v.n)
		return err
	case *bigIntLockKey:
		_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, v.b)
		return err
	default:
		panic("sql: invalid LockKey")
	}
}

// TryLock locks the given key. If multiple operations lock the same key, only
// the first will succeed. The rest will fail with the error ErrAlreadyLocked.
// TryLock must be run within a transaction context, panics otherwise.
func TryLock(ctx context.Context, key LockKey) (locked bool, err error) {
	uowCtx := UowContext.MustValue(ctx)
	if !uowCtx.isTx() {
		return false, fmt.Errorf("%w: %s", ErrLockWithoutTx, key)
	}

	tx := uowCtx.tx

	// locked will be true if the key is locked successfully.
	switch v := key.(type) {
	case *intLockKey:
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1, $2)`, v.m, v.n).Scan(&locked)
	case *bigIntLockKey:
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, v.b).Scan(&locked)
	default:
		panic("sql: invalid LockKey")
	}

	return
}

// underlying returns the underlying db client.
func (uow *UnitOfWork) underlying() DB {
	if uow.isTx() {
		return uow.tx
	}

	return uow.db
}

// isTx returns true if the underlying type is a transaction.
func (uow *UnitOfWork) isTx() bool {
	return uow.tx != nil
}

// newTx returns a UnitOfWork with transaction.
func newTx(tx *sql.Tx) *UnitOfWork {
	return &UnitOfWork{
		tx: tx,
	}
}

func getUowOptions(opts ...Option) *UowOption {
	var opt UowOption
	for _, o := range opts {
		o(&opt)
	}

	return &opt
}

func isTxContext(ctx context.Context) bool {
	uowCtx, ok := UowContext.Value(ctx)
	return ok && uowCtx.isTx()
}
