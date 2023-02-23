package uow

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

// UOW represents the operations by UnitOfWork.
type UOW interface {
	IsTx() bool
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
	uowCtx, ok := Value(ctx)
	if ok {
		return uowCtx.underlying()
	}

	return uow.underlying()
}

// RunInTx wraps the operation in a transaction. If a context containing tx is
// passed in, then it will use the context tx. Transaction cannot be nested.
// The transaction can only be committed by the parent.
func (uow *UnitOfWork) RunInTx(ctx context.Context, fn func(context.Context) error, opts ...Option) (err error) {
	if isTxContext(ctx) {
		return fn(ctx)
	}

	if uow.IsTx() {
		return fn(WithValue(ctx, uow))
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

	return fn(WithValue(ctx, newTx(tx)))
}

// underlying returns the underlying db client.
func (uow *UnitOfWork) underlying() DB {
	if uow.IsTx() {
		return uow.tx
	}

	return uow.db
}

// IsTx returns true if the underlying type is a transaction.
func (uow *UnitOfWork) IsTx() bool {
	return uow.tx != nil
}

// newTx returns a UnitOfWork with transaction.
func newTx(tx *sql.Tx) *UnitOfWork {
	return &UnitOfWork{
		tx: tx,
	}
}

func isTxContext(ctx context.Context) bool {
	uow, ok := Value(ctx)
	return ok && uow.IsTx()
}

func getUowOptions(opts ...Option) *UowOption {
	var opt UowOption
	for _, o := range opts {
		o(&opt)
	}

	return &opt
}
