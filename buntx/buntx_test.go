package buntx_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/alextanhongpin/dbtx/buntx"
	"github.com/alextanhongpin/dbtx/testing/buntest"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

var (
	ctx         = context.Background()
	ErrRollback = errors.New("rollback")
	buntestOpts = buntest.Options{
		Image: "postgres:17.4",
		Hook:  migrate,
	}
)

func TestMain(m *testing.M) {
	stop := buntest.Init(buntestOpts)
	defer stop()

	m.Run()
}

func TestTransaction(t *testing.T) {
	t.Run("one", func(t *testing.T) {
		tx := buntest.Tx(t)

		var count int64
		err := tx.NewRaw(`select count(*) from users`).Scan(ctx, &count)
		is := assert.New(t)
		is.Nil(err)
		is.Zero(count)

		var id int64
		err = tx.NewRaw(`insert into users(name) values (?) returning id`, "john").Scan(ctx, &id)
		is.Nil(err)
		is.NotZero(id)
	})

	t.Run("two", func(t *testing.T) {
		tx := buntest.Tx(t)

		var count int64
		err := tx.NewRaw(`select count(*) from users`).Scan(ctx, &count)
		is := assert.New(t)
		is.Nil(err)
		is.Zero(count)

		var id int64
		err = tx.NewRaw(`insert into users(name) values (?) returning id`, "john").Scan(ctx, &id)
		is.Nil(err)
		is.NotZero(id)
	})
}

func TestDB(t *testing.T) {
	t.Run("one", func(t *testing.T) {
		db := buntest.DB(t)

		var count int64
		err := db.NewRaw(`select count(*) from users`).Scan(ctx, &count)
		is := assert.New(t)
		is.Nil(err)
		is.Zero(count)

		var id int64
		err = db.NewRaw(`insert into users(name) values (?) returning id`, "john").Scan(ctx, &id)
		is.Nil(err)
		is.NotZero(id)
	})

	t.Run("two", func(t *testing.T) {
		db := buntest.DB(t)

		var count int64
		err := db.NewRaw(`select count(*) from users`).Scan(ctx, &count)
		is := assert.New(t)
		is.Nil(err)
		is.NotZero(count)

		var id int64
		err = db.NewRaw(`insert into users(name) values (?) returning id`, "alice").Scan(ctx, &id)
		is.Nil(err)
		is.NotZero(id)
	})
}

func TestBun(t *testing.T) {
	bunDB := buntest.New(t, buntestOpts).DB(t)
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

		return ErrRollback
	})
	is := assert.New(t)
	is.ErrorIs(err, ErrRollback)

	var count int64
	err = bunDB.NewRaw(`select count(*) from users`).Scan(ctx, &count)
	is.Nil(err)
	is.Equal(int64(0), count)
}

func migrate(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	_, err = db.Exec(`create table users (
	id bigint generated always as identity,
	name text not null,
	primary key (id),
	unique(name)
)`)
	return err
}
