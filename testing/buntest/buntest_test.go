package buntest_test

import (
	"context"
	"testing"

	"github.com/alextanhongpin/dbtx/testing/buntest"
	"github.com/stretchr/testify/assert"
)

var (
	ctx         = context.Background()
	buntestOpts = buntest.Options{
		Image: "postgres:17.4",
		Hook:  migrate,
	}
)

func migrate(dsn string) error {
	bun := buntest.NewBun(dsn)
	defer bun.Close()

	_, err := bun.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS users (
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
	close := buntest.Init(buntestOpts)
	defer func() {
		_ = close()
	}()

	m.Run()
}

func TestDSN(t *testing.T) {
	dsn := buntest.DSN()
	assert.NotEmpty(t, dsn)
}

func TestConnection(t *testing.T) {
	db := buntest.DB(t)

	var n int
	err := db.QueryRowContext(ctx, "SELECT 1 + 1").Scan(&n)
	is := assert.New(t)
	is.Nil(err)
	is.Equal(2, n)
}

func TestStandalone(t *testing.T) {
	// Create a new database for this test.
	// The data is separate from the global database.
	db := buntest.New(t, buntestOpts).DB(t)

	var n int
	err := db.QueryRowContext(ctx, "SELECT 1 + 1").Scan(&n)
	is := assert.New(t)
	is.Nil(err)
	is.Equal(2, n)
}
