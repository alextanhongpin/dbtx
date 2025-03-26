package pgxtx_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alextanhongpin/dbtx/pgxtx"
	"github.com/alextanhongpin/dbtx/testing/pgxtest"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
)

var postgresImage = "postgres:17.4"
var ctx = context.Background()
var ErrRollback = errors.New("pgxtx_test: rollback")

func migrate(dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS users (
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
	close := pgxtest.Init(pgxtest.InitOptions{
		Image: postgresImage,
		Hook:  migrate,
	})
	defer close()

	m.Run()
}

func TestDSN(t *testing.T) {
	dsn := pgxtest.DSN()
	assert.NotEmpty(t, dsn)
}

func TestConnection(t *testing.T) {
	db := pgxtest.DB(t)

	var n int
	err := db.QueryRow(ctx, "SELECT 1 + 1").Scan(&n)
	is := assert.New(t)
	is.Nil(err)
	is.Equal(2, n)
}

func TestStandalone(t *testing.T) {
	// Create a new database for this test.
	// The data is separate from the global database.
	db := pgxtest.New(t, pgxtest.Options{Image: postgresImage, Hook: migrate}).DB(t)

	var n int
	err := db.QueryRow(ctx, "SELECT 1 + 1").Scan(&n)
	is := assert.New(t)
	is.Nil(err)
	is.Equal(2, n)

	t.Run("multiple dbs", func(t *testing.T) {
		is := assert.New(t)

		repos := make([]*userRepository, 3)
		for i := range 3 {
			db := pgxtest.New(t, pgxtest.Options{Image: postgresImage, Hook: migrate}).DB(t)
			uow := pgxtx.New(db)
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

func TestRollback(t *testing.T) {
	db := pgxtest.DB(t)

	// Create a new Unit of Work.
	uow := pgxtx.New(db)

	// Pass the unit of work to the repository.
	repo := &userRepository{uow: uow}

	is := assert.New(t)

	// Start a new transaction with the transaction context.
	err := uow.RunInTx(ctx, func(txCtx context.Context) error {
		is.False(pgxtx.IsTx(ctx))
		is.True(pgxtx.IsTx(txCtx))

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
	is.ErrorIs(err, pgx.ErrNoRows)

	n, err := repo.Count(ctx)
	is.Equal(1, n)
	is.Nil(err)

	id, err := repo.Find(ctx, "alice")
	is.NotZero(id)
	is.Nil(err)
}

type userRepository struct {
	uow *pgxtx.Atomic
}

func (u *userRepository) Create(ctx context.Context, name string) (int, error) {
	var id int
	err := u.db(ctx).QueryRow(ctx, "INSERT INTO users (name) VALUES ($1) RETURNING id", name).Scan(&id)
	return id, err
}

func (u *userRepository) Find(ctx context.Context, name string) (int, error) {
	var id int
	err := u.db(ctx).QueryRow(ctx, "SELECT id FROM users WHERE name = $1", name).Scan(&id)
	return id, err
}

func (u *userRepository) Count(ctx context.Context) (int, error) {
	var n int
	err := u.db(ctx).QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

func (u *userRepository) db(ctx context.Context) pgxtx.DBTX {
	return u.uow.DBTx(ctx)
}
