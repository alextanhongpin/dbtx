package repository

import (
	"database/sql"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/outbox"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestLifecycleMigrationPreservesLiveClaim(t *testing.T) {
	client := dbtest.New(t, dbtest.Options{
		Image: "postgres:19beta3-alpine3.24",
		Hook: func(dsn string) error {
			db, err := sql.Open("postgres", dsn)
			if err != nil {
				return err
			}
			defer db.Close()
			_, err = db.Exec(baseSchema + `CREATE VIEW dbtx.outbox_retry_state AS SELECT * FROM dbtx.outbox;
CREATE FUNCTION dbtx.outbox_retry(uuid, text, text, jsonb) RETURNS SETOF dbtx.outbox
LANGUAGE sql AS $$ SELECT * FROM dbtx.outbox WHERE false $$;`)
			return err
		},
	})
	db := client.DB(t)
	id := uuid.NewV7()
	_, err := db.ExecContext(t.Context(), `insert into dbtx.outbox(id, aggregate_type, aggregate_id, event_type, payload, status, attempts, max_attempts, locked_by, last_error) values ($1, 'migration', 'aggregate', 'test', 'null', 'processing', 1, 2, 'old', 'previous')`, id)
	require.NoError(t, err)
	var before, after []byte
	query := "select to_jsonb(t) from dbtx.outbox t where id = $1"
	require.NoError(t, db.QueryRowContext(t.Context(), query, id).Scan(&before))
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), Migrations+Migrations)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var removed bool
	require.NoError(t, db.QueryRowContext(t.Context(), "select to_regclass('dbtx.outbox_retry_state') is null").Scan(&removed))
	require.True(t, removed, "obsolete lifecycle view remains")
	require.NoError(t, db.QueryRowContext(t.Context(), "select to_regprocedure('dbtx.outbox_retry(uuid, text, text, jsonb)') is null").Scan(&removed))
	require.True(t, removed, "obsolete adapter remains")

	require.NoError(t, db.QueryRowContext(t.Context(), query, id).Scan(&after))
	require.JSONEq(t, string(before), string(after))
	repo := New(db)
	err = repo.Nack(t.Context(), id, "old", false, "retry", 0)
	require.NoError(t, err)
	msgs, err := repo.Poll(t.Context(), "new", 1, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, int32(2), msgs[0].Attempts)
	require.ErrorIs(t, repo.Ack(t.Context(), id, "old"), outbox.ErrLeaseExpired)
}
