package dbtx_test

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/core/test/containers"
	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/lock"
	_ "github.com/lib/pq"
)

const postgresVersion = "15.1-alpine"

var ErrIntentional = errors.New("intentional error")

func TestMain(m *testing.M) {
	stop := containers.StartPostgres(postgresVersion, migrate)
	// Run tests.
	code := m.Run()
	stop()

	os.Exit(code)
}

func TestSQL(t *testing.T) {
	var n int
	db := containers.PostgresDB(t)
	err := db.QueryRow("select 1 + 1").Scan(&n)
	if err != nil {
		t.Error(err)
	}
	t.Log("got n:", n)
}

func TestAtomic(t *testing.T) {
	db := containers.PostgresDB(t)
	tx := dbtx.New(db)
	ctx := context.Background()

	if dbtx.IsTx(ctx) {
		t.Fatal("dbtx.IsTx: want false, got true")
	}

	err := tx.RunInTx(ctx, func(ctx context.Context) error {
		if !dbtx.IsTx(ctx) {
			t.Fatal("dbtx.IsTx: want true, got false")
		}

		tx := tx.DB(ctx)
		res, err := tx.Exec(`insert into numbers(n) values ($1)`, 1)
		if err != nil {
			return err
		}

		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}

		t.Logf("inserted %d rows\n", rows)

		return ErrIntentional
	})
	if err != nil && !errors.Is(err, ErrIntentional) {
		t.Error(err)
	}

	var c int
	err = db.QueryRow(`select count(*) from numbers`).Scan(&c)
	if err != nil {
		t.Error(err)
	}
	if c != 0 {
		t.Fatalf("expected count to be 0, got %d", c)
	}
	t.Logf("count is %d\n", c)
}

func TestAtomicNested(t *testing.T) {
	db := containers.PostgresDB(t)
	tx := dbtx.New(db)
	err := tx.RunInTx(context.Background(), func(ctx1 context.Context) error {
		return tx.RunInTx(ctx1, func(ctx2 context.Context) error {
			tx := tx.DB(ctx2)
			res, err := tx.Exec(`insert into numbers(n) values ($1)`, 1)
			if err != nil {
				return err
			}

			rows, err := res.RowsAffected()
			if err != nil {
				return err
			}

			t.Logf("inserted %d rows\n", rows)

			return ErrIntentional
		})
	})
	if err != nil && !errors.Is(err, ErrIntentional) {
		t.Error(err)
	}

	var c int
	err = db.QueryRow(`select count(*) from numbers`).Scan(&c)
	if err != nil {
		t.Error(err)
	}
	if c != 0 {
		t.Fatalf("expected count to be 0, got %d", c)
	}
	t.Logf("count is %d\n", c)
}

func TestAtomicIntLockKey(t *testing.T) {
	db := containers.PostgresDB(t)
	tx := dbtx.New(db)
	err := tx.RunInTx(context.Background(), func(ctx context.Context) error {
		return lock.Lock(ctx, lock.IntKey(1, 2))
	})
	if err != nil {
		t.Error(err)
	}
}

func TestAtomicIntLockKeyLocked(t *testing.T) {
	db := containers.PostgresDB(t)
	tx := dbtx.New(db)
	err := tx.RunInTx(context.Background(), func(txCtx context.Context) error {
		locked1, err := lock.TryLock(txCtx, lock.IntKey(1, 1))
		if err != nil {
			return err
		}
		locked2, err := lock.TryLock(txCtx, lock.IntKey(1, 1))
		if err != nil {
			return err
		}

		t.Logf("got locked1=%t, locked2=%t\n", locked1, locked2)

		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

func TestAtomicBigIntLockKey(t *testing.T) {
	db := containers.PostgresDB(t)
	tx := dbtx.New(db)
	err := tx.RunInTx(context.Background(), func(ctx context.Context) error {
		return lock.Lock(ctx, lock.BigIntKey(big.NewInt(10)))
	})
	if err != nil {
		t.Error(err)
	}
}

func TestAtomicBigIntLockKeyLocked(t *testing.T) {
	db := containers.PostgresDB(t)
	tx := dbtx.New(db)
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		err := tx.RunInTx(context.Background(), func(ctx context.Context) error {
			locked, err := lock.TryLock(ctx, lock.BigIntKey(big.NewInt(1)))
			if err != nil {
				return err
			}

			time.Sleep(200 * time.Millisecond)
			t.Logf("goroutine locked=%t\n", locked)
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	err := tx.RunInTx(context.Background(), func(ctx context.Context) error {
		// Both locked1 and locked2 will always be true in the same transaction.
		// Only when locking with the same key in another transaction will result
		// in false.
		locked1, err := lock.TryLock(ctx, lock.BigIntKey(big.NewInt(1)))
		if err != nil {
			return err
		}

		locked2, err := lock.TryLock(ctx, lock.BigIntKey(big.NewInt(1)))
		if err != nil {
			return err
		}

		t.Logf("got locked1=%t, locked2=%t\n", locked1, locked2)
		return nil
	})
	if err != nil {
		t.Error(err)
	}

	wg.Wait()
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`create table numbers(n int);`)
	return err
}
