package uow_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alextanhongpin/uow"
	"github.com/alextanhongpin/uow/postgres/lock"
	_ "github.com/lib/pq"
	"github.com/ory/dockertest"
	"github.com/ory/dockertest/docker"
)

var db *sql.DB

var ErrIntentional = errors.New("intentional error")

func TestMain(m *testing.M) {
	stopDB := startDB()
	migrate()

	// Run tests.
	code := m.Run()

	stopDB()

	os.Exit(code)
}

func TestSQL(t *testing.T) {
	var n int
	err := db.QueryRow("select 1 + 1").Scan(&n)
	if err != nil {
		t.Error(err)
	}
	t.Log("got n:", n)
}

func TestUOW(t *testing.T) {
	u := uow.New(db)
	err := u.RunInTx(context.Background(), func(ctx context.Context) error {
		tx := u.DB(ctx)
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

func TestUOWNested(t *testing.T) {
	u := uow.New(db)
	err := u.RunInTx(context.Background(), func(ctx1 context.Context) error {
		return u.RunInTx(ctx1, func(ctx2 context.Context) error {
			tx := u.DB(ctx2)
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

func TestUOWIntLockKey(t *testing.T) {
	u := uow.New(db)
	err := u.RunInTx(context.Background(), func(ctx context.Context) error {
		return lock.Lock(ctx, lock.IntKey(1, 2))
	})
	if err != nil {
		t.Error(err)
	}
}

func TestUOWIntLockKeyLocked(t *testing.T) {
	u := uow.New(db)
	err := u.RunInTx(context.Background(), func(txCtx context.Context) error {
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

func TestUOWBigIntLockKey(t *testing.T) {
	u := uow.New(db)
	err := u.RunInTx(context.Background(), func(ctx context.Context) error {
		return lock.Lock(ctx, lock.BigIntKey(big.NewInt(10)))
	})
	if err != nil {
		t.Error(err)
	}
}

func TestUOWBigIntLockKeyLocked(t *testing.T) {
	u := uow.New(db)
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		err := u.RunInTx(context.Background(), func(ctx context.Context) error {
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
	err := u.RunInTx(context.Background(), func(ctx context.Context) error {
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

func migrate() {
	_, err := db.Exec(`create table numbers(n int);`)
	if err != nil {
		panic(err)
	}
}

func startDB() func() {
	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatal("could not construct pool:", err)
	}

	err = pool.Client.Ping()
	if err != nil {
		log.Fatal("could not connect to docker:", err)
	}

	// Pulls an image, creates a container based on it and run it.
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "postgres",
		Tag:        "15.1-alpine",
		Env: []string{
			"POSTGRES_PASSWORD=123456",
			"POSTGRES_USER=john",
			"POSTGRES_DB=test",
			"listen_addresses = '*'",
		},
	}, func(config *docker.HostConfig) {
		// Set AutoRemove to true so that stopped container goes away by itself.
		config.AutoRemove = true
		config.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		log.Fatal("could not start resources:", err)
	}
	hostAndPort := resource.GetHostPort("5432/tcp")
	databaseURL := fmt.Sprintf("postgres://john:123456@%s/test?sslmode=disable", hostAndPort)

	log.Println("connecting to database on url:", databaseURL)

	resource.Expire(120) // Tell docker to kill the container in 120 seconds.

	// Exponential backoff-retry, because the application in the container might
	// not be ready to accept connections yet.
	pool.MaxWait = 120 * time.Second
	if err = pool.Retry(func() error {
		db, err = sql.Open("postgres", databaseURL)
		if err != nil {
			return err
		}
		return db.Ping()
	}); err != nil {
		log.Fatal("could not connect to docker:", err)
	}

	return func() {
		if err := pool.Purge(resource); err != nil {
			log.Fatal("could not purge resource:", err)
		}
	}
}
