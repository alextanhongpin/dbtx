package dbtest_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/alextanhongpin/testdump/yamldump"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

var (
	ctx        = context.Background()
	dbtestOpts = dbtest.Options{
		Image: "postgres:17.4",
		Hook:  migrate,
	}
)

func migrate(dsn string) error {
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}

	_, err = conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS users (
		id int generated always as identity primary key,
		name text not null,
		unique(id)
	)`)
	if err != nil {
		return err
	}
	return nil
}

func TestMain(m *testing.M) {
	// Initialize a global database connection.
	// The connection is closed after the test.
	// You can get the connection by calling pgxtest.DB(ctx, t).
	// You can also create a new connection by calling pgxtest.New(t).DB().
	close := dbtest.Init(dbtestOpts)
	defer func() {
		_ = close()
	}()

	m.Run()
}

func TestDSN(t *testing.T) {
	dsn := dbtest.DSN()
	assert.NotEmpty(t, dsn)
}

func TestConnection(t *testing.T) {
	db := dbtest.DB(t)

	var n int
	err := db.QueryRowContext(ctx, "SELECT 1 + 1").Scan(&n)
	is := assert.New(t)
	is.NoError(err)
	is.Equal(2, n)
}

func TestStandalone(t *testing.T) {
	// Create a new database for this test.
	// The data is separate from the global database.
	db := dbtest.New(t, dbtestOpts).DB(t)

	var n int
	err := db.QueryRowContext(ctx, "SELECT 1 + 1").Scan(&n)
	is := assert.New(t)
	is.NoError(err)
	is.Equal(2, n)
}

func TestDump(t *testing.T) {
	is := assert.New(t)

	db := dbtest.Tx(t)
	_, err := db.ExecContext(ctx, `INSERT INTO users (name) VALUES ('Alice'), ('Bob')`)
	is.NoError(err)

	dbtest.Dump(t, db, "select * from users", nil)
	dbtest.Dump(t, db, "select * from users where name=$1", []any{"Bob"}, yamldump.File("where"))
}
