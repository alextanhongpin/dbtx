package buntx_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/alextanhongpin/core/storage/pg/pgtest"
	"github.com/alextanhongpin/dbtx/buntx"
	_ "github.com/lib/pq"
)

const postgresVersion = "postgres:15.1-alpine"

func TestMain(m *testing.M) {
	stop := pgtest.InitDB(pgtest.Image(postgresVersion), pgtest.Hook(migrate))
	code := m.Run()
	stop() // os.Exit does not care about defer.
	os.Exit(code)
}

func TestBun(t *testing.T) {
	bunDB := pgtest.BunDB(t)
	// Setup unit of work.
	u := buntx.New(bunDB)
	ctx := context.Background()
	err := u.RunInTx(ctx, func(ctx context.Context) error {
		tx := u.Tx(ctx)

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
