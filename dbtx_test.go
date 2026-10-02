package dbtx_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "github.com/lib/pq"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
)

var (
	ErrRollback = errors.New("rollback")
	dbtestOpts  = dbtest.Options{
		Image: "postgres:17.4",
		Hook:  migrate,
	}
)

func TestMain(m *testing.M) {
	stop := dbtest.Init(dbtestOpts)
	defer stop()

	m.Run()
}

func TestSQL(t *testing.T) {
	ctx := t.Context()
	var n int
	err := dbtest.DB(t).QueryRowContext(ctx, "select 1 + 1").Scan(&n)

	is := assert.New(t)
	is.NoError(err)
	is.Equal(2, n)
}

func TestLoggerContext(t *testing.T) {
	logger := &InMemoryLogger{}
	atm := dbtx.New(dbtest.DB(t), dbtx.WithLogger(logger))
	ctx := t.Context()

	var n int
	err := atm.DB().QueryRowContext(ctx, "select 1 + $1", 1).Scan(&n)

	is := assert.New(t)
	is.NoError(err)
	is.Equal(2, n)

	var m int
	err = atm.RunInTx(ctx, func(ctx context.Context) error {
		return atm.Tx(ctx).QueryRowContext(ctx, "select 2 + $1", 2).Scan(&m)
	})
	is.NoError(err)
	is.Equal(4, m)

	t.Log("LOG")
	t.Log(logger.Logs)
}

func TestAtomicContext(t *testing.T) {
	atm := dbtx.New(dbtest.DB(t))
	ctx := t.Context()

	t.Run("isNotTx", func(t *testing.T) {
		assert.False(t, dbtx.IsTx(ctx))
	})

	t.Run("isTx", func(t *testing.T) {
		is := assert.New(t)
		err := atm.RunInTx(ctx, func(txCtx context.Context) error {
			is.True(dbtx.IsTx(txCtx))

			return ErrRollback
		})
		is.ErrorIs(err, ErrRollback)
	})
}

// TestAtomic tests if the transaction is rollback successfullly.
func TestAtomic(t *testing.T) {
	atm := dbtx.New(dbtest.DB(t))
	err := atm.RunInTx(t.Context(), func(txCtx context.Context) error {
		create(t, atm, txCtx, 41)
		create(t, atm, txCtx, 42)
		count(t, atm, txCtx, 2)

		return ErrRollback
	})

	is := assert.New(t)
	is.ErrorIs(err, ErrRollback, err)
	count(t, atm, t.Context(), 0)
}

// TestPanic tests if the transaction is rollback on panic.
func TestPanic(t *testing.T) {
	atm := dbtx.New(dbtest.DB(t))

	assert.Panics(t, func() {
		_ = atm.RunInTx(t.Context(), func(txCtx context.Context) error {
			create(t, atm, txCtx, 41)
			create(t, atm, txCtx, 42)
			count(t, atm, txCtx, 2)

			panic("server error")
		})
	})

	count(t, atm, t.Context(), 0)
}

func TestSubTransaction(t *testing.T) {
	t.Run("outside tx", func(t *testing.T) {
		// Arrange.
		ctx := t.Context()

		db := dbtx.New(dbtest.DB(t))
		err := db.RunInSubTx(ctx, func(ctx context.Context) error {
			panic("won't be called")
		})
		is := assert.New(t)
		is.ErrorIs(err, dbtx.ErrOutOfTx)
	})

	t.Run("inside tx", func(t *testing.T) {
		// Arrange.
		ctx := t.Context()

		db := dbtx.New(dbtest.DB(t))

		is := assert.New(t)
		err := db.RunInTx(ctx, func(txCtx context.Context) error {
			// Given that there is a row created in tx,
			create(t, db, txCtx, 40)
			// And there are two more rows created in subtransaction,
			subTxErr := db.RunInSubTx(txCtx, func(stxCtx context.Context) error {
				create(t, db, stxCtx, 41)
				create(t, db, stxCtx, 42)

				// Then the count until here is 3
				count(t, db, stxCtx, 3)

				// When subtransaction is rolled back,

				return ErrRollback
			})

			// Then the rows created by subtransaction will be undo,
			// And the rows created by transaction will remain.
			is.ErrorIs(subTxErr, ErrRollback)
			count(t, db, txCtx, 1)
			return nil
		})
		is.NoError(err)
	})
}

func migrate(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`create table numbers(n int)`)

	return err
}

// create inserts a row with the given number.
func create(t *testing.T, atm atomic, ctx context.Context, n int) {
	t.Helper()

	repo := newNumberRepo(atm)
	rows, err := repo.Create(ctx, n)
	is := assert.New(t)
	is.NoError(err)
	is.Equal(int64(1), rows)
}

// count check that the given number does not exist in the database.
func count(t *testing.T, atm atomic, ctx context.Context, want int) {
	t.Helper()

	repo := newNumberRepo(atm)
	got, err := repo.Count(ctx)
	is := assert.New(t)
	is.NoError(err, err)
	is.Equal(want, got)
}

type atomic interface {
	DBTx(ctx context.Context) dbtx.DBTX
}

type numberRepo struct {
	atomic
}

func newNumberRepo(atm atomic) *numberRepo {
	return &numberRepo{
		atomic: atm,
	}
}

func (r *numberRepo) Count(ctx context.Context) (int, error) {
	var n int
	err := r.DBTx(ctx).
		QueryRowContext(ctx, `select count(*) from numbers`).
		Scan(&n)
	return n, err
}

func (r *numberRepo) Create(ctx context.Context, n int) (int64, error) {
	res, err := r.DBTx(ctx).ExecContext(ctx, `insert into numbers(n) values ($1)`, n)
	if err != nil {
		return 0, err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}

	return rows, nil
}

type Log struct {
	Method string
	Query  string
	Args   []any
}

type InMemoryLogger struct {
	Logs []Log
}

func (l *InMemoryLogger) Log(ctx context.Context, method, query string, args ...any) {
	l.Logs = append(l.Logs, Log{
		Method: method,
		Query:  query,
		Args:   args,
	})
}
