package dbtest_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

var postgresImage = "postgres:17.4"
var ctx = context.Background()
var ErrRollback = errors.New("dbtest_test: rollback")

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
	close := dbtest.Init(dbtest.InitOptions{
		Driver: "postgres",
		Image:  postgresImage,
		Hook:   migrate,
	})
	defer close()

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
	is.Nil(err)
	is.Equal(2, n)
}

func TestStandalone(t *testing.T) {
	// Create a new database for this test.
	// The data is separate from the global database.
	db := dbtest.New(t, dbtest.Options{Driver: "postgres", Image: postgresImage, Hook: migrate}).DB(t)

	var n int
	err := db.QueryRowContext(ctx, "SELECT 1 + 1").Scan(&n)
	is := assert.New(t)
	is.Nil(err)
	is.Equal(2, n)

	t.Run("multiple dbs", func(t *testing.T) {
		is := assert.New(t)

		repos := make([]*userRepository, 3)
		for i := range 3 {
			db := dbtest.New(t, dbtest.Options{Driver: "postgres", Image: postgresImage, Hook: migrate}).DB(t)
			uow := dbtx.New(db)
			repo := &userRepository{uow: uow}
			_, err := repo.Create(ctx, "john")
			is.Nil(err)
			repos[i] = repo
		}

		for i := range 3 {
			userID, err := repos[i].Find(ctx, "john")
			is.Nil(err)
			is.Equal(1, userID)
		}
	})
}

func TestTransaction(t *testing.T) {
	// This will be rollbacked after the test completes.
	repo := &userRepository{uow: dbtx.New(dbtest.Tx(t))}
	_, err := repo.Create(ctx, "john")
	is := assert.New(t)
	is.Nil(err)
}

func TestRollback(t *testing.T) {
	db := dbtest.DB(t)

	// Create a new Unit of Work.
	uow := dbtx.New(db)

	// Pass the unit of work to the repository.
	repo := &userRepository{uow: uow}

	is := assert.New(t)

	// Start a new transaction with the transaction context.
	err := uow.RunInTx(ctx, func(txCtx context.Context) error {
		is.False(dbtx.IsTx(ctx))
		is.True(dbtx.IsTx(txCtx))

		{
			id, err := repo.Create(txCtx, "john")
			if err != nil {
				return err
			}
			is.True(id > 0)

			// User is created in the transaction.
			userID, err := repo.Find(txCtx, "john")
			if err != nil {
				return err
			}
			is.NotZero(userID)
			is.Equal(id, userID)
		}

		{
			id, err := repo.Create(ctx, "alice")
			if err != nil {
				return err
			}
			is.True(id > 0)
		}

		n, err := repo.Count(txCtx)
		is.Equal(2, n)
		is.Nil(err)

		return ErrRollback
	})

	is.ErrorIs(err, ErrRollback)

	// Rollback. The user should not be found.
	_, err = repo.Find(ctx, "john")
	is.ErrorIs(err, sql.ErrNoRows)

	n, err := repo.Count(ctx)
	is.Equal(1, n)
	is.Nil(err)

	id, err := repo.Find(ctx, "alice")
	is.NotZero(id)
	is.Nil(err)
}

type userRepository struct {
	uow *dbtx.Atomic
}

func (u *userRepository) Create(ctx context.Context, name string) (int, error) {
	var id int
	err := u.db(ctx).QueryRowContext(ctx, "INSERT INTO users (name) VALUES ($1) RETURNING id", name).Scan(&id)
	return id, err
}

func (u *userRepository) Find(ctx context.Context, name string) (int, error) {
	var id int
	err := u.db(ctx).QueryRowContext(ctx, "SELECT id FROM users WHERE name = $1", name).Scan(&id)
	return id, err
}

func (u *userRepository) Count(ctx context.Context) (int, error) {
	var n int
	err := u.db(ctx).QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

func (u *userRepository) db(ctx context.Context) dbtx.DBTX {
	return u.uow.DBTx(ctx)
}
