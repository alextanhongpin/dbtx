package uow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
)

var (
	ErrNestedTransaction = errors.New("uow: transaction cannot be nested")
	ErrAlreadyLocked     = errors.New("uow: already locked")
	ErrConstructor       = errors.New("uow: constructor accepts 0 or 1 sql.TxOptions")
)

const logPrefix = "[uow] "

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
	DB() DB
	DBCtx(ctx context.Context) DB
	IsTx() bool
	Commit() error
	Rollback() error
	BeginTx(ctx context.Context, opt *sql.TxOptions) (*UnitOfWork, error)
	RunInTx(ctx context.Context, fn func(*UnitOfWork) error, opts ...*sql.TxOptions) (err error)
	RunInTxContext(ctx context.Context, fn func(ctx context.Context) error, opts ...*sql.TxOptions) (err error)
	Lock(ctx context.Context, n int, fn func(uow *UnitOfWork) error, opts ...*sql.TxOptions) error
	LockContext(ctx context.Context, n int, fn func(ctx context.Context) error, opts ...*sql.TxOptions) error
	TryLock(ctx context.Context, n *big.Int, fn func(uow *UnitOfWork) error, opts ...*sql.TxOptions) error
	TryLockContext(ctx context.Context, n *big.Int, fn func(ctx context.Context) error, opts ...*sql.TxOptions) error
	TryLock2(ctx context.Context, m, n int, fn func(uow *UnitOfWork) error, opts ...*sql.TxOptions) error
	TryLock2Context(ctx context.Context, m, n int, fn func(ctx context.Context) error, opts ...*sql.TxOptions) error
}

var _ UOW = (*UnitOfWork)(nil)

// UnitOfWork represents a unit of work.
type UnitOfWork struct {
	tx *sql.Tx
	db *sql.DB
}

// New returns a pointer to UnitOfWork.
func New(db *sql.DB) *UnitOfWork {
	uow := &UnitOfWork{
		db: db,
	}

	return uow
}

// NewTx returns a UnitOfWork with transaction.
func NewTx(tx *sql.Tx) *UnitOfWork {
	return &UnitOfWork{
		tx: tx,
	}
}

func (uow *UnitOfWork) DB() DB {
	if uow.IsTx() {
		return uow.tx
	}

	return uow.db
}

// DBCtx returns the UoW from the context if provided, else returns the default UoW.
func (uow *UnitOfWork) DBCtx(ctx context.Context) DB {
	uowCtx, ok := UowContext.Value(ctx)
	if ok {
		return uowCtx.DB()
	}

	return uow.DB()
}

// BeginTx creates a new UnitOfPointer with the underlying db transaction
// driver. Not recommended to be used directly, since it is easy to forget to
// commit and/or rollback. Use RunInTx instead.
func (uow *UnitOfWork) BeginTx(ctx context.Context, opt *sql.TxOptions) (*UnitOfWork, error) {
	if uow.IsTx() {
		return nil, ErrNestedTransaction
	}

	tx, err := uow.db.BeginTx(ctx, opt)
	if err != nil {
		return nil, err
	}

	t := NewTx(tx)

	return t, nil
}

// IsTx returns true if the underlying type is a transaction.
func (uow *UnitOfWork) IsTx() bool {
	return uow.tx != nil
}

// Commit commits a transaction.
func (uow *UnitOfWork) Commit() error {
	return uow.tx.Commit()
}

// Rollback rolls back a transaction.
func (uow *UnitOfWork) Rollback() error {
	return uow.tx.Rollback()
}

// RunInTx wraps the operation in a transaction.
func (uow *UnitOfWork) RunInTx(ctx context.Context, fn func(*UnitOfWork) error, opts ...*sql.TxOptions) (err error) {
	if uow.IsTx() {
		return ErrNestedTransaction
	}

	tx, err := uow.BeginTx(ctx, getTxOptions(opts...))
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

// RunInTxContext is similar to RunInTx, except it passes the context
// containing the pointer of UnitOfWork as an argument instead of the pointer
// UnitOfWork directly.
func (uow *UnitOfWork) RunInTxContext(ctx context.Context, fn func(ctx context.Context) error, opts ...*sql.TxOptions) (err error) {
	return uow.RunInTx(ctx, func(uow *UnitOfWork) error {
		ctx = UowContext.WithValue(ctx, uow)

		return fn(ctx)
	}, opts...)
}

// Lock locks the given key. If multiple operations lock the same key, it
// will wait for the previous operation to complete.
func (uow *UnitOfWork) Lock(ctx context.Context, n int, fn func(uow *UnitOfWork) error, opts ...*sql.TxOptions) error {
	return uow.RunInTx(ctx, func(uow *UnitOfWork) error {
		if _, err := uow.DB().ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, n); err != nil {
			return err
		}

		return fn(uow)
	}, opts...)
}

// LockContext is similar to Lock, except it passes the context containing the
// pointer of UnitOfWork as an argument instead of the pointer UnitOfWork
// directly.
func (uow *UnitOfWork) LockContext(ctx context.Context, n int, fn func(ctx context.Context) error, opts ...*sql.TxOptions) error {
	return uow.Lock(ctx, n, func(uow *UnitOfWork) error {
		ctx = UowContext.WithValue(ctx, uow)

		return fn(ctx)
	}, opts...)
}

// TryLock locks the given key. If multiple operations lock the same key, only
// the first will succeed. The rest will fail with the error ErrAlreadyLocked.
func (uow *UnitOfWork) TryLock(ctx context.Context, n *big.Int, fn func(uow *UnitOfWork) error, opts ...*sql.TxOptions) error {
	return uow.RunInTx(ctx, func(uow *UnitOfWork) error {
		var locked bool
		if err := uow.DB().QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, n).Scan(&locked); err != nil {
			return err
		}

		if locked {
			return fmt.Errorf("%w: key=(%d)", ErrAlreadyLocked, n)
		}

		return fn(uow)
	}, opts...)
}

// TryLockContext is similar to TryLock, except it passes the context
// containing the pointer of UnitOfWork as an argument instead of the pointer
// UnitOfWork directly.
func (uow *UnitOfWork) TryLockContext(ctx context.Context, n *big.Int, fn func(ctx context.Context) error, opts ...*sql.TxOptions) error {
	return uow.TryLock(ctx, n, func(uow *UnitOfWork) error {

		ctx = UowContext.WithValue(ctx, uow)

		return fn(ctx)
	}, opts...)
}

// TryLock2 is similar to TryLock, but locks the key tuple.
func (uow *UnitOfWork) TryLock2(ctx context.Context, m, n int, fn func(uow *UnitOfWork) error, opts ...*sql.TxOptions) error {
	return uow.RunInTx(ctx, func(uow *UnitOfWork) error {
		var locked bool
		if err := uow.DB().QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1, $2)`, m, n).Scan(&locked); err != nil {
			return err
		}

		if locked {
			return fmt.Errorf("%w: key=(%d, %d)", ErrAlreadyLocked, m, n)
		}

		return fn(uow)
	}, opts...)
}

// TryLock2Context is similar to TryLock2, except it passes the context
// containing the pointer of UnitOfWork as an argument instead of the pointer
// UnitOfWork directly.
func (uow *UnitOfWork) TryLock2Context(ctx context.Context, m, n int, fn func(ctx context.Context) error, opts ...*sql.TxOptions) error {
	return uow.TryLock2(ctx, m, n, func(uow *UnitOfWork) error {

		ctx = UowContext.WithValue(ctx, uow)

		return fn(ctx)
	}, opts...)
}

func getTxOptions(opts ...*sql.TxOptions) *sql.TxOptions {
	switch len(opts) {
	case 0:
		return nil
	case 1:
		return opts[0]
	default:
		panic(ErrConstructor)
	}
}
