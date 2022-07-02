package uow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
)

var (
	ErrNestedTransaction = errors.New("uow: transaction cannot be nested")
	ErrAlreadyLocked     = errors.New("uow: already locked")
	ErrContextNotFound   = errors.New("uow: UnitOfWork not found in context")
)

const logPrefix = "[uow]"

// UowContext represents the key for the context containing the pointer of UnitOfWork.
var UowContext = key[*UnitOfWork]("uow")

// IDB represents the common db operations.
type IDB interface {
	Exec(query string, args ...any) (sql.Result, error)
	Prepare(query string) (*sql.Stmt, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row

	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// NewLogger returns an instance of the logger with the prefix set.
func NewLogger() *log.Logger {
	logger := log.Default()
	logger.SetPrefix(logPrefix)

	return logger
}

// UnitOfWork represents a unit of work.
type UnitOfWork struct {
	tx *sql.Tx
	db *sql.DB
	IDB
	once   sync.Once
	Logger *log.Logger
}

// New returns a pointer to UnitOfWork.
func New(db *sql.DB) *UnitOfWork {
	uow := &UnitOfWork{
		db:     db,
		IDB:    db,
		Logger: NewLogger(),
	}

	// Consume the 'Once', since it will only be used for transaction.
	uow.once.Do(func() {})

	return uow
}

// NewTx returns a UnitOfWork with transaction.
func NewTx(tx *sql.Tx) *UnitOfWork {
	return &UnitOfWork{
		tx:  tx,
		IDB: tx,
	}
}

func (uow *UnitOfWork) atomic(ctx context.Context, opts ...*sql.TxOptions) (*UnitOfWork, error) {
	if uow.IsTx() {
		return nil, ErrNestedTransaction
	}

	var tx *sql.Tx
	var err error

	switch len(opts) {
	case 0:
		tx, err = uow.db.Begin()
	case 1:
		tx, err = uow.db.BeginTx(ctx, opts[0])
	default:
		panic("uow: multiple *sql.TxOptions")
	}

	if err != nil {
		return nil, err
	}

	t := NewTx(tx)
	t.Logger = uow.Logger

	return t, nil
}

// IsTx returns true if the underlying type is a transaction.
func (uow *UnitOfWork) IsTx() bool {
	return uow.tx != nil
}

// Commit commits a transaction.
func (uow *UnitOfWork) Commit() (err error) {
	uow.once.Do(func() {
		err = uow.tx.Commit()
	})

	return
}

// Rollback rolls back a transaction.
func (uow *UnitOfWork) Rollback() (err error) {
	uow.once.Do(func() {
		err = uow.tx.Rollback()
	})

	return
}

// AtomicFn wraps the operation in a transaction.
func (uow *UnitOfWork) AtomicFn(ctx context.Context, fn func(*UnitOfWork) error, opts ...*sql.TxOptions) (err error) {
	if uow.IsTx() {
		return ErrNestedTransaction
	}

	tx, err := uow.atomic(ctx, opts...)
	if err != nil {
		return err
	}

	defer func() {
		if err := tx.Rollback(); err != nil {
			if uow.Logger != nil {
				uow.Logger.Printf("rollback failed: %v\n", err)
			}
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}

	return tx.Commit()
}

// AtomicFnContext is similar to AtomicFn, except it passes the context
// containing the pointer of UnitOfWork as an argument instead of the pointer
// UnitOfWork directly.
func (uow *UnitOfWork) AtomicFnContext(ctx context.Context, fn func(ctx context.Context) error, opts ...*sql.TxOptions) (err error) {
	if uow.IsTx() {
		return ErrNestedTransaction
	}

	tx, err := uow.atomic(ctx, opts...)
	if err != nil {
		return err
	}

	defer func() {
		if err := tx.Rollback(); err != nil {
			if uow.Logger != nil {
				uow.Logger.Printf("rollback failed: %v\n", err)
			}
		}
	}()

	ctx = UowContext.WithValue(ctx, tx)

	if err := fn(ctx); err != nil {
		return err
	}

	return tx.Commit()
}

// AtomicLock locks the given key. If multiple operations lock the same key, it
// will wait for the previous operation to complete.
func (uow *UnitOfWork) AtomicLock(ctx context.Context, n int, fn func(uow *UnitOfWork) error, opts ...*sql.TxOptions) error {
	return uow.AtomicFn(ctx, func(uow *UnitOfWork) error {
		if _, err := uow.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, n); err != nil {
			return err
		}

		return fn(uow)
	}, opts...)
}

// AtomicLockContext is similar to AtomicLock, except it passes the context
// containing the pointer of UnitOfWork as an argument instead of the pointer
// UnitOfWork directly.
func (uow *UnitOfWork) AtomicLockContext(ctx context.Context, n int, fn func(ctx context.Context) error, opts ...*sql.TxOptions) error {
	return uow.AtomicFn(ctx, func(uow *UnitOfWork) error {
		if _, err := uow.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, n); err != nil {
			return err
		}

		ctx = UowContext.WithValue(ctx, uow)

		return fn(ctx)
	}, opts...)
}

// AtomicTryLock locks the given key. If multiple operations lock the same key,
// only the first will succeed. The rest will fail with the error
// ErrAlreadyLocked.
func (uow *UnitOfWork) AtomicTryLock(ctx context.Context, n int, fn func(uow *UnitOfWork) error, opts ...*sql.TxOptions) error {
	return uow.AtomicFn(ctx, func(uow *UnitOfWork) error {
		var locked bool
		if err := uow.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, n).Scan(&locked); err != nil {
			return err
		}

		if locked {
			return fmt.Errorf("%w: %d", ErrAlreadyLocked, n)
		}

		return fn(uow)
	}, opts...)
}

// AtomicTryLockContext is similar to AtomicTryLock, except it passes the
// context containing the pointer of UnitOfWork as an argument instead of the
// pointer UnitOfWork directly.
func (uow *UnitOfWork) AtomicTryLockContext(ctx context.Context, n int, fn func(ctx context.Context) error, opts ...*sql.TxOptions) error {
	return uow.AtomicFn(ctx, func(uow *UnitOfWork) error {
		var locked bool
		if err := uow.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, n).Scan(&locked); err != nil {
			return err
		}

		if locked {
			return fmt.Errorf("%w: %d", ErrAlreadyLocked, n)
		}

		ctx = UowContext.WithValue(ctx, uow)

		return fn(ctx)
	}, opts...)
}

// AtomicTryLock2 is similar to AtomicTryLock, but locks the key tuple.
func (uow *UnitOfWork) AtomicTryLock2(ctx context.Context, m, n int, fn func(uow *UnitOfWork) error, opts ...*sql.TxOptions) error {
	return uow.AtomicFn(ctx, func(uow *UnitOfWork) error {
		var locked bool
		if err := uow.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1, $2)`, m, n).Scan(&locked); err != nil {
			return err
		}

		if locked {
			return fmt.Errorf("%w: %d", ErrAlreadyLocked, n)
		}

		return fn(uow)
	}, opts...)
}

// AtomicTryLock2Context is similar to AtomicTryLock2, except it passes the
// context containing the pointer of UnitOfWork as an argument instead of the
// pointer UnitOfWork directly.
func (uow *UnitOfWork) AtomicTryLock2Context(ctx context.Context, m, n int, fn func(ctx context.Context) error, opts ...*sql.TxOptions) error {
	return uow.AtomicFn(ctx, func(uow *UnitOfWork) error {
		var locked bool
		if err := uow.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1, $2)`, m, n).Scan(&locked); err != nil {
			return err
		}

		if locked {
			return fmt.Errorf("%w: %d", ErrAlreadyLocked, n)
		}

		ctx = UowContext.WithValue(ctx, uow)

		return fn(ctx)
	}, opts...)
}
