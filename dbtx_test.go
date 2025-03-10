package dbtx_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/core/storage/pg/pgtest"
	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/lock"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

const postgresVersion = "postgres:15.1-alpine"

var ErrRollback = errors.New("rollback")

func TestMain(m *testing.M) {
	stop := pgtest.Init(pgtest.Image(postgresVersion), pgtest.Hook(migrate))
	defer stop()

	m.Run()
}

func TestSQL(t *testing.T) {
	var n int
	err := pgtest.DB(t).QueryRow("select 1 + 1").Scan(&n)

	is := assert.New(t)
	is.Nil(err)
	is.Equal(2, n)
}

func TestLoggerContext(t *testing.T) {
	logger := &InMemoryLogger{}
	atm := dbtx.New(pgtest.DB(t), dbtx.WithLogger(logger))
	ctx := context.Background()

	var n int
	err := atm.DB().QueryRow("select 1 + $1", 1).Scan(&n)

	is := assert.New(t)
	is.Nil(err)
	is.Equal(2, n)

	var m int
	err = atm.RunInTx(ctx, func(ctx context.Context) error {
		return atm.Tx(ctx).QueryRow("select 2 + $1", 2).Scan(&m)
	})
	is.Nil(err)
	is.Equal(4, m)

	t.Log("LOG")
	t.Log(logger.Logs)
}

func TestAtomicContext(t *testing.T) {
	atm := dbtx.New(pgtest.DB(t))
	ctx := context.Background()

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
	atm := dbtx.New(pgtest.DB(t))
	err := atm.RunInTx(context.Background(), func(txCtx context.Context) error {
		create(t, atm, txCtx, 41)
		create(t, atm, txCtx, 42)
		count(t, atm, txCtx, 2)

		return ErrRollback
	})

	is := assert.New(t)
	is.ErrorIs(err, ErrRollback, err)
	count(t, atm, context.Background(), 0)
}

// TestPanic tests if the transaction is rollback on panic.
func TestPanic(t *testing.T) {
	atm := dbtx.New(pgtest.DB(t))

	assert.Panics(t, func() {
		_ = atm.RunInTx(context.Background(), func(txCtx context.Context) error {
			create(t, atm, txCtx, 41)
			create(t, atm, txCtx, 42)
			count(t, atm, txCtx, 2)

			panic("server error")
		})
	})

	count(t, atm, context.Background(), 0)
}

func TestAtomicIntKeyPairLocked(t *testing.T) {
	key := lock.NewIntKeyPair(1, 1)
	atm := dbtx.New(pgtest.DB(t))
	err := atm.RunInTx(context.Background(), func(txCtx context.Context) error {
		if err := lock.TryLock(txCtx, key); err != nil {
			return err
		}

		// Locking twice in the same transaction will not cause a deadlock.
		if err := lock.TryLock(txCtx, key); err != nil {
			return err
		}

		return nil
	})
	assert.Nil(t, err)
}

func TestAtomicLockBoundary(t *testing.T) {
	is := assert.New(t)
	tx := dbtx.New(pgtest.DB(t))
	err := tx.RunInTx(context.Background(), func(ctx context.Context) error {
		is.Nil(lock.Lock(ctx, lock.NewIntKeyPair(math.MinInt32, math.MaxInt32)))
		is.Nil(lock.Lock(ctx, lock.NewIntKey(math.MinInt64)))
		is.Nil(lock.Lock(ctx, lock.NewIntKey(math.MaxInt64)))

		return nil
	})
	is.Nil(err)
}

func TestAtomicIntLockKeyLocked(t *testing.T) {
	atm := dbtx.New(pgtest.DB(t))
	key := lock.NewIntKey(10)

	is := assert.New(t)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		err := atm.RunInTx(context.Background(), func(txCtx context.Context) error {
			if err := lock.TryLock(txCtx, key); err != nil {
				return err
			}

			t.Log("goroutine0: locked=true")
			time.Sleep(100 * time.Millisecond)

			return nil
		})
		is.Nil(err)
	}()

	time.Sleep(50 * time.Millisecond)
	err := atm.RunInTx(context.Background(), func(txCtx context.Context) error {
		err := lock.TryLock(txCtx, key)

		// ̃NOTE: Both of this is expected to return false, but it is true now
		// because of the test library which puts everything in a single transaction.
		//assert.False(locked1)
		//assert.False(locked2)
		t.Logf("goroutine1: locked1=%t\n", errors.Is(err, lock.ErrAlreadyLocked))
		return err
	})
	is.ErrorIs(err, lock.ErrAlreadyLocked)
	wg.Wait()
}

func TestAtomicLocker(t *testing.T) {
	is := assert.New(t)

	// Arrange.
	ctx := context.Background()
	key := lock.NewStrKey("The meaning of life...")

	locker := lock.New(pgtest.DB(t))

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()

		var errTimeout = errors.New("timeout")
		ctx, cancel := context.WithTimeoutCause(ctx, 1*time.Second, errTimeout)
		defer cancel()

		// Lock1 locks the key successfully. Forgetting to call unlock locks the key
		// forever unless a timeout is set.
		err := locker.TryLock(ctx, key, func(ctx context.Context) error {
			is.True(dbtx.IsTx(ctx))
			<-ctx.Done()
			return context.Cause(ctx)
		})
		is.ErrorIs(err, errTimeout)
	}()

	go func() {
		defer wg.Done()

		time.Sleep(10 * time.Millisecond)

		// Lock2 fails when locking the same key.
		err := locker.TryLock(ctx, key, func(ctx context.Context) error {
			is.True(dbtx.IsTx(ctx))
			return nil
		})
		is.ErrorIs(err, lock.ErrAlreadyLocked)
	}()

	go func() {
		defer wg.Done()

		time.Sleep(10 * time.Millisecond)

		// Lock3 will wait for the previous lock to be released.
		err := locker.Lock(ctx, key, func(ctx context.Context) error {
			is.True(dbtx.IsTx(ctx))
			return nil
		})
		is.Nil(err)
	}()

	wg.Wait()
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`create table numbers(n int)`)

	return err
}

// create inserts a row with the given number.
func create(t *testing.T, atm atomic, ctx context.Context, n int) {
	t.Helper()

	repo := newNumberRepo(atm)
	rows, err := repo.Create(ctx, n)
	is := assert.New(t)
	is.Nil(err)
	is.Equal(int64(1), rows)
}

// count check that the given number does not exist in the database.
func count(t *testing.T, atm atomic, ctx context.Context, want int) {
	t.Helper()

	repo := newNumberRepo(atm)
	got, err := repo.Count(ctx)
	is := assert.New(t)
	is.Nil(err, err)
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
		QueryRow(`select count(*) from numbers`).
		Scan(&n)
	return n, err
}

func (r *numberRepo) Create(ctx context.Context, n int) (int64, error) {
	res, err := r.DBTx(ctx).Exec(`insert into numbers(n) values ($1)`, n)
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
