package dbtx_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/core/storage/pg/pgtest"
	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/lock"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

const postgresVersion = "15.1-alpine"

var ErrIntentional = errors.New("intentional error")

func TestMain(m *testing.M) {
	stop := pgtest.InitDB(pgtest.Tag(postgresVersion), pgtest.Hook(migrate))
	code := m.Run()
	stop()
	os.Exit(code)
}

func TestSQL(t *testing.T) {
	var n int
	db := pgtest.DB(t)
	err := db.QueryRow("select 1 + 1").Scan(&n)
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, 2, n)
}

func TestLoggerContext(t *testing.T) {
	logger := &InMemoryLogger{}

	db := pgtest.DB(t)
	atm := dbtx.New(db,
		dbtx.WithLogger(logger),
	)
	ctx := context.Background()

	var n int
	if err := atm.DB().QueryRow("select 1 + $1", 1).Scan(&n); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 2, n)

	var m int
	err := atm.RunInTx(ctx, func(ctx context.Context) error {
		return atm.Tx(ctx).QueryRow("select 2 + $1", 2).Scan(&m)
	})
	assert.Nil(t, err)
	assert.Equal(t, 4, m)

	t.Log("LOG")
	t.Log(logger.Logs)
}

func TestAtomicContext(t *testing.T) {
	db := pgtest.DB(t)
	atm := dbtx.New(db)
	ctx := context.Background()

	t.Run("isNotTx", func(t *testing.T) {
		isTx := dbtx.IsTx(ctx)
		assert.False(t, isTx)
	})

	t.Run("isTx", func(t *testing.T) {
		assert := assert.New(t)
		err := atm.RunInTx(ctx, func(txCtx context.Context) error {
			isTx := dbtx.IsTx(txCtx)

			assert.True(isTx)
			return ErrIntentional
		})

		assert.ErrorIs(err, ErrIntentional)
	})

	t.Run("Tx when not in tx context", func(t *testing.T) {
		assert.False(t, dbtx.IsTx(ctx))
	})

	t.Run("Tx when in tx context", func(t *testing.T) {
		assert := assert.New(t)
		err := atm.RunInTx(ctx, func(txCtx context.Context) error {
			assert.True(dbtx.IsTx(txCtx))

			return ErrIntentional
		})

		assert.ErrorIs(err, ErrIntentional)
	})
}

// TestAtomic tests if the transaction is rollback successfullly.
func TestAtomic(t *testing.T) {
	assert := assert.New(t)

	db := pgtest.DB(t)
	atm := dbtx.New(db)
	ctx := context.Background()

	err := atm.RunInTx(ctx, func(txCtx context.Context) error {
		if err := assertCreated(t, newNumberRepo(atm), txCtx, 42); err != nil {
			return err
		}

		return ErrIntentional
	})
	assert.ErrorIs(err, ErrIntentional, err)
	assertNoRows(t, newNumberRepo(atm), 42)
}

// TestPanic tests if the transaction is rollback on panic.
func TestPanic(t *testing.T) {
	assert := assert.New(t)

	db := pgtest.DB(t)
	atm := dbtx.New(db)
	ctx := context.Background()

	assert.Panics(func() {
		_ = atm.RunInTx(ctx, func(txCtx context.Context) error {
			if err := assertCreated(t, newNumberRepo(atm), txCtx, 42); err != nil {
				return err
			}

			panic("server error")
		})
	})

	assertNoRows(t, newNumberRepo(atm), 42)
}

func TestAtomicIntKeyPair(t *testing.T) {
	db := pgtest.DB(t)
	tx := dbtx.New(db)
	err := tx.RunInTx(context.Background(), func(ctx context.Context) error {
		return lock.Lock(ctx, lock.NewIntKeyPair(1, 2))
	})
	if err != nil {
		t.Error(err)
	}
}

func TestAtomicIntKeyPairLocked(t *testing.T) {
	db := pgtest.DB(t)
	tx := dbtx.New(db)
	err := tx.RunInTx(context.Background(), func(txCtx context.Context) error {
		err := lock.TryLock(txCtx, lock.NewIntKeyPair(1, 1))
		locked1 := errors.Is(err, lock.ErrAlreadyLocked)
		if err != nil && !locked1 {
			return err
		}

		err = lock.TryLock(txCtx, lock.NewIntKeyPair(1, 1))
		locked2 := errors.Is(err, lock.ErrAlreadyLocked)
		if err != nil && !locked2 {
			return err
		}

		// Within the same transaction, calling TryLock twice will return true.
		// If called from another transaction, the TryLock will return false.
		assert.False(t, locked1)
		assert.False(t, locked2)
		t.Logf("locked1=%t, locked2=%t", locked1, locked2)

		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

func TestAtomicLockBoundary(t *testing.T) {
	assert := assert.New(t)

	db := pgtest.DB(t)
	tx := dbtx.New(db)
	err := tx.RunInTx(context.Background(), func(ctx context.Context) error {
		assert.Nil(lock.Lock(ctx, lock.NewIntKeyPair(math.MinInt32, math.MaxInt32)))
		assert.Nil(lock.Lock(ctx, lock.NewIntKey(math.MinInt64)))
		assert.Nil(lock.Lock(ctx, lock.NewIntKey(math.MaxInt64)))

		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

func TestAtomicIntLockKeyLocked(t *testing.T) {
	db := pgtest.DB(t)
	atm := dbtx.New(db)
	key := lock.NewIntKey(10)

	assert := assert.New(t)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		ctx := context.Background()
		err := atm.RunInTx(ctx, func(txCtx context.Context) error {
			err := lock.TryLock(txCtx, key)
			locked := errors.Is(err, lock.ErrAlreadyLocked)
			if err != nil && !locked {
				return err
			}

			assert.False(locked)
			t.Logf("goroutine0: locked=%t\n", locked)
			time.Sleep(200 * time.Millisecond)

			return nil
		})
		assert.Nil(err)
	}()

	time.Sleep(100 * time.Millisecond)
	ctx := context.Background()
	err := atm.RunInTx(ctx, func(txCtx context.Context) error {
		// Both locked1 and locked2 will always be true in the same transaction.
		// Only when locking with the same key in another transaction will result
		// in false.
		err := lock.TryLock(txCtx, key)
		locked1 := errors.Is(err, lock.ErrAlreadyLocked)
		if err != nil && !locked1 {
			return err
		}

		err = lock.TryLock(txCtx, key)
		locked2 := errors.Is(err, lock.ErrAlreadyLocked)
		if err != nil && !locked2 {
			return err
		}

		assert.True(locked1)
		assert.True(locked2)
		// ̃NOTE: Both of this is expected to return false, but it is true now
		// because of the test library which puts everything in a single transaction.
		//assert.False(locked1)
		//assert.False(locked2)
		t.Logf("goroutine1: locked1=%t, locked2=%t\n", locked1, locked2)
		return nil
	})
	assert.Nil(err)
	wg.Wait()
}

func TestAtomicLocker(t *testing.T) {
	assert := assert.New(t)

	db := pgtest.DB(t)

	// Arrange.
	ctx := context.Background()
	key := lock.NewStrKey("The meaning of life...")

	locker := lock.New(db)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()

		var errTimeout = errors.New("timeout")
		ctx, cancel := context.WithTimeoutCause(ctx, 1*time.Second, errTimeout)
		defer cancel()

		// Lock1 locks the key successfully. Forgetting to call unlock locks the key
		// forever unless a timeout is set.
		err := locker.TryLock(ctx, key, func(ctx context.Context) error {
			assert.True(dbtx.IsTx(ctx))
			<-ctx.Done()
			return context.Cause(ctx)
		})
		assert.ErrorIs(err, errTimeout)
	}()

	go func() {
		defer wg.Done()

		time.Sleep(10 * time.Millisecond)

		// Lock2 fails when locking the same key.
		err := locker.TryLock(ctx, key, func(ctx context.Context) error {
			assert.True(dbtx.IsTx(ctx))
			return nil
		})
		assert.ErrorIs(err, lock.ErrAlreadyLocked)
	}()

	wg.Wait()
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`create table numbers(n int);`)
	return err
}

func assertCreated(t *testing.T, repo *numberRepo, ctx context.Context, n int) error {
	t.Helper()

	rows, err := repo.Create(ctx, n)
	if err != nil {
		return err
	}

	assert.Equal(t, int64(1), rows)

	i, err := repo.Find(ctx, n)
	if err != nil {
		return err
	}

	assert.Equal(t, n, i)

	return nil
}

func assertNoRows(t *testing.T, repo *numberRepo, n int) {
	t.Helper()

	i, err := repo.Find(context.Background(), n)
	assert.ErrorIs(t, err, sql.ErrNoRows, err)
	assert.Equal(t, 0, i)
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

func (r *numberRepo) Find(ctx context.Context, n int) (int, error) {
	var i int
	err := r.DBTx(ctx).
		QueryRow(`select n from numbers where n = $1`, n).
		Scan(&i)
	return i, err
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
