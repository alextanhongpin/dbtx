package sqlxtx_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/alextanhongpin/dbtx/sqlxtx"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
)

const postgresVersion = "postgres:15.1-alpine"

var ctx = context.Background()
var ErrRollback = errors.New("intentional rollback")

func TestMain(m *testing.M) {
	stop := dbtest.Init(dbtest.InitOptions{
		Image: postgresVersion,
		Hook:  migrate,
	})
	code := m.Run()
	stop()
	os.Exit(code)
}

func TestQuery(t *testing.T) {
	db := dbtest.DB(t)
	dbx := sqlx.NewDb(db, "postgres")
	atm := sqlxtx.New(dbx)

	type Result struct {
		Sum  int  `db:"sum"`
		Even bool `db:"even"`
	}

	var r Result
	if err := atm.DB().QueryRowxContext(ctx, `select 1 + 1 as sum, true as even`).StructScan(&r); err != nil {
		t.Fatal(err)
	}

	assert := assert.New(t)
	assert.Equal(2, r.Sum)
	assert.True(r.Even)
	t.Log(r)
}

func TestRollback(t *testing.T) {
	db := dbtest.DB(t)
	dbx := sqlx.NewDb(db, "postgres")
	atm := sqlxtx.New(dbx)

	assert := assert.New(t)

	err := atm.RunInTx(ctx, func(txCtx context.Context) error {
		n := 10
		res, err := atm.DBTx(txCtx).ExecContext(ctx, `insert into numbers (n) values ($1)`, n)
		if err != nil {
			return err
		}

		i, err := res.RowsAffected()
		if err != nil {
			return err
		}
		assert.Equal(i, int64(1))

		return ErrRollback
	})

	assert.ErrorIs(err, ErrRollback)

	var n int
	err = atm.DB().QueryRowxContext(ctx, `select count(*) from numbers`).Scan(&n)
	assert.Nil(err)
	assert.Equal(0, n)
}

func migrate(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	_, err = db.Exec(`create table numbers(n int);`)
	return err
}
