package pgxtx

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrNotTransaction = errors.New("pgxtx: underlying type is not a transaction")

// DBTX represents the common db operations for *pgx.Conn, *pgxpool.Pool and pgx.Tx.
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

// connOrPool is a common interface for both *pgx.Conn and *pgxpool.Pool.
type connOrPool interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	DBTX
}

// Ensures the struct Atomic implements the interface.
var _ atomic = (*Atomic)(nil)

// Atomic represents a unit of work.
type Atomic struct {
	db  connOrPool
	fns []func(DBTX) DBTX
}

// New returns a pointer to Atomic.
func New(db connOrPool, fns ...func(DBTX) DBTX) *Atomic {
	return &Atomic{
		db:  db,
		fns: fns,
	}
}

// DB returns the underlying *pgx.Conn or *pgxpool.Pool as DBTX interface, to
// avoid the caller to init a new transaction.
// This also allows wrapping the *pgx.Conn/*pgxpool.Pool with other
// implementations, such as recorder.
func (a *Atomic) DB() DBTX {
	return apply(a.db, a.fns...)
}

// DBTx returns the DBTX from the context, which can be either *pgx.Conn,
// *pgxpool.Pool or pgx.Tx.
// Returns the atomic underlying type if the context is empty.
func (a *Atomic) DBTx(ctx context.Context) DBTX {
	if tx, ok := Value(ctx); ok {
		return tx
	}

	return a.DB()
}

// Tx returns the pgx.Tx from context. The return type is still a DBTX
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

	return pgx.BeginTxFunc(ctx, a.db, TxOptions(ctx), func(tx pgx.Tx) error {
		return fn(withValue(ctx, &Tx{tx: tx, fns: a.fns}))
	})
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
