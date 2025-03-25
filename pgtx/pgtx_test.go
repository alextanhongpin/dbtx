package pgtx_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alextanhongpin/dbtx/pgtx"
	"github.com/alextanhongpin/dbtx/pgtx/pgxtest"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
)

var ctx = context.Background()
var ErrRollback = errors.New("pgtxt_test: rollback")

func migrate(conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS users (
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
	close := pgxtest.Init(pgxtest.Hook(migrate))
	defer close()

	m.Run()
}

func TestConnection(t *testing.T) {
	db := pgxtest.DB(ctx, t)

	var n int
	err := db.QueryRow(ctx, "SELECT 1 + 1").Scan(&n)
	is := assert.New(t)
	is.Nil(err)
	is.Equal(2, n)
}

func TestStandalone(t *testing.T) {
	// Create a new database for this test.
	// The data is separate from the global database.
	db := pgxtest.New(t, pgxtest.Hook(migrate)).DB(ctx)

	var n int
	err := db.QueryRow(ctx, "SELECT 1 + 1").Scan(&n)
	is := assert.New(t)
	is.Nil(err)
	is.Equal(2, n)

	t.Run("multiple dbs", func(t *testing.T) {
		is := assert.New(t)

		repos := make([]*userRepository, 3)
		for i := range 3 {
			db := pgxtest.New(t, pgxtest.Hook(migrate)).DB(ctx)
			uow := pgtx.New(db)
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
	db := pgxtest.DB(ctx, t)

	// Create a new Unit of Work.
	uow := pgtx.New(db)

	// Pass the unit of work to the repository.
	repo := &userRepository{uow: uow}

	is := assert.New(t)

	// Start a new transaction with the transaction context.
	err := uow.RunInTx(ctx, func(txCtx context.Context) error {
		is.True(pgtx.IsTx(txCtx))
		is.False(pgtx.IsTx(ctx))

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

		return ErrRollback
	})

	is.ErrorIs(err, ErrRollback)

	// Rollback. The user should not be found.
	_, err = repo.Find(ctx, "john")
	is.ErrorIs(err, pgx.ErrNoRows)
}

type userRepository struct {
	uow *pgtx.Atomic
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

func (u *userRepository) db(ctx context.Context) pgtx.DBTX {
	return u.uow.DBTx(ctx)
}
