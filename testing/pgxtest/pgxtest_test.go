package pgxtest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alextanhongpin/dbtx/testing/pgxtest"
	"github.com/stretchr/testify/assert"
)

var (
	ErrRollback = errors.New("pgxtest_test: rollback")
	ctx         = context.Background()
	dbOpts      = pgxtest.Options{
		Image: "postgres:17.4",
		Hook: func(dsn string) error {
			// TODO: Migrate the database.
			return nil
		},
	}
)

func TestMain(m *testing.M) {
	// Initialize the pgxtest package.
	stop := pgxtest.Init(dbOpts)
	defer func() {
		_ = stop()
	}()

	// Run the tests.
	m.Run()
}

func TestDSN(t *testing.T) {
	// Arrange: get the DSN from pgxtest.
	dsn := pgxtest.DSN()

	// Act: perform a simple check.
	is := assert.New(t)
	is.NotEmpty(dsn)
}

func TestConn(t *testing.T) {
	t.Run("db", func(t *testing.T) {
		// Arrange: get a DB connection from dbtest.
		db := pgxtest.DB(t)

		// Act: perform a simple query.
		var result int
		err := db.QueryRow(ctx, "SELECT 1 + 1").Scan(&result)

		// Assert: ensure there's no error and the result matches expectations.
		is := assert.New(t)
		is.NoError(err)
		is.Equal(2, result)
	})

	t.Run("conn", func(t *testing.T) {
		// Arrange: get a DB connection from dbtest.
		db := pgxtest.Conn(t)

		// Act: perform a simple query.
		var result int
		err := db.QueryRow(ctx, "SELECT 1 + 1").Scan(&result)

		// Assert: ensure there's no error and the result matches expectations.
		is := assert.New(t)
		is.NoError(err)
		is.Equal(2, result)
	})

	t.Run("pool", func(t *testing.T) {
		// Arrange: get a DB connection from dbtest.
		db := pgxtest.Pool(t)

		// Act: perform a simple query.
		var result int
		err := db.QueryRow(ctx, "SELECT 1 + 1").Scan(&result)

		// Assert: ensure there's no error and the result matches expectations.
		is := assert.New(t)
		is.NoError(err)
		is.Equal(2, result)
	})
}

func TestNew(t *testing.T) {
	c := pgxtest.New(t, dbOpts)

	t.Run("db", func(t *testing.T) {
		// Arrange: get a DB connection from dbtest.
		db := c.DB(t)

		// Act: perform a simple query.
		var result int
		err := db.QueryRow(ctx, "SELECT 1 + 1").Scan(&result)

		// Assert: ensure there's no error and the result matches expectations.
		is := assert.New(t)
		is.NoError(err)
		is.Equal(2, result)
	})

	t.Run("conn", func(t *testing.T) {
		// Arrange: get a DB connection from dbtest.
		db := c.Conn(t)

		// Act: perform a simple query.
		var result int
		err := db.QueryRow(ctx, "SELECT 1 + 1").Scan(&result)

		// Assert: ensure there's no error and the result matches expectations.
		is := assert.New(t)
		is.NoError(err)
		is.Equal(2, result)
	})

	t.Run("pool", func(t *testing.T) {
		// Arrange: get a DB connection from dbtest.
		db := c.Pool(t)

		// Act: perform a simple query.
		var result int
		err := db.QueryRow(ctx, "SELECT 1 + 1").Scan(&result)

		// Assert: ensure there's no error and the result matches expectations.
		is := assert.New(t)
		is.NoError(err)
		is.Equal(2, result)
	})
}
