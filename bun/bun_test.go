package bun_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	buntx "github.com/alextanhongpin/dbtx/bun"
	_ "github.com/lib/pq"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/extra/bundebug"
)

var bunDB *bun.DB

func TestMain(m *testing.M) {
	stop := startDB()
	code := m.Run()
	stop() // os.Exit does not care about defer.
	os.Exit(code)
}

func TestBun(t *testing.T) {
	// Setup unit of work.
	u := buntx.New(bunDB)
	ctx := context.Background()
	err := u.RunInTx(ctx, func(ctx context.Context) error {
		tx := u.DB(ctx)

		var id int64
		err := tx.NewRaw(`insert into users(name) values (?) returning id`, "john").Scan(ctx, &id)
		if err != nil {
			return err
		}
		t.Logf("got id: %v", id)

		var count int64
		err = tx.NewRaw(`select count(*) from users`).Scan(ctx, &count)
		if err != nil {
			return err
		}
		if count != int64(1) {
			return errors.New("invalid user count")
		}

		return errors.New("rollback")
	})
	if err.Error() != "rollback" {
		t.Fatal("failed to rollback")
	}

	var count int64
	err = bunDB.NewRaw(`select count(*) from users`).Scan(ctx, &count)
	if err != nil {
		t.Error(err)
	}
	if count != int64(0) {
		t.Fatalf("count: want %d, got %d", 1, count)
	}
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`create table users (
	id bigint generated always as identity,
	name text not null,
	primary key (id),
	unique(name)
)`)
	return err
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
		db, err := sql.Open("postgres", databaseURL)
		if err != nil {
			return err
		}
		if err := db.Ping(); err != nil {
			return err
		}
		if err := migrate(db); err != nil {
			return err
		}

		sqlDB := sql.OpenDB(pgdriver.NewConnector(
			pgdriver.WithUser("john"),
			pgdriver.WithAddr(hostAndPort),
			pgdriver.WithPassword("123456"),
			pgdriver.WithDatabase("test"),
			pgdriver.WithApplicationName("test_app"),
			pgdriver.WithInsecure(true),
		))

		bunDB = bun.NewDB(sqlDB, pgdialect.New())
		bunDB.AddQueryHook(bundebug.NewQueryHook(
			bundebug.WithVerbose(true),
		))

		return bunDB.Ping()
	}); err != nil {
		log.Fatal("could not connect to docker:", err)
	}

	return func() {
		if err := bunDB.Close(); err != nil {
			log.Println("failed to close bun:", err)
		}

		if err := pool.Purge(resource); err != nil {
			log.Fatal("could not purge resource:", err)
		}
	}
}
