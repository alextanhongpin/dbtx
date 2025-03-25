package pgtx

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotTransaction = errors.New("dbtx: underlying type is not a transaction")

// DBTX represents the common db operations for both *sql.DB and *sql.Tx.
type DBTX interface {
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// atomic represents the database atomic operations in a transactions.
type atomic interface {
	DB() DBTX
	DBTx(ctx context.Context) DBTX
	Tx(ctx context.Context) DBTX

	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}

// Ensures the struct Atomic implements the interface.
var _ atomic = (*Atomic)(nil)

// Atomic represents a unit of work.
type Atomic struct {
	pool   *pgxpool.Conn
	conn   *pgx.Conn
	isPool bool
	fns    []func(DBTX) DBTX
}

// New returns a pointer to Atomic.
func New(db any, fns ...func(DBTX) DBTX) *Atomic {
	conn, isConn := db.(*pgx.Conn)
	pool, isPool := db.(*pgxpool.Conn)
	if !(isConn || isPool) {
		panic("invalid")
	}

	return &Atomic{
		fns:    fns,
		conn:   conn,
		pool:   pool,
		isPool: isPool,
	}
}

// DB returns the underlying *sql.DB as DBTX interface, to avoid the caller to
// init a new transaction.
// This also allows wrapping the *sql.DB with other implementations, such as
// recorder.
func (a *Atomic) DB() DBTX {
	return apply(a.db(), a.fns...)
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

	var db interface {
		BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	}
	if a.isPool {
		db = a.pool
	} else {
		db = a.conn
	}

	return pgx.BeginTxFunc(ctx, db, TxOptions(ctx), func(tx pgx.Tx) error {
		ctx = withValue(ctx, &Tx{tx: tx, fns: a.fns})
		return fn(ctx)
	})
}

func (a *Atomic) db() DBTX {
	if a.isPool {
		return a.pool
	}
	return a.conn
}

type Tx struct {
	tx  pgx.Tx
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
