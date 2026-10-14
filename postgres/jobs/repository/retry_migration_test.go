package repository

import (
	"database/sql"
	"testing"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/jobs"
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
			_, err = db.Exec(baseSchema) // Install the legacy schema only.
			return err
		},
	})
	db := client.DB(t)
	id := uuid.NewV7()
	_, err := db.ExecContext(t.Context(), `insert into dbtx.jobs(id, idempotency_key, request, response, status, attempts, max_attempts, worker_id, fencing_token, visible_at, error) values ($1, 'migration', 'null', 'null', 'running', 1, 2, 'old', 7, now(), 'previous')`, id)
	require.NoError(t, err)
	var before, after []byte
	query := "select to_jsonb(t) from dbtx.jobs t where id = $1"
	require.NoError(t, db.QueryRowContext(t.Context(), query, id).Scan(&before))
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), Migrations+Migrations)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var viewColumns []string
	rows, err := db.QueryContext(t.Context(), `select attname from pg_attribute
        where attrelid = 'dbtx.jobs_retry_state'::regclass and attnum > 0 and not attisdropped order by attnum`)
	// Keep the same lifecycle column contract across all four module tables.
	require.NoError(t, err)
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		viewColumns = append(viewColumns, column)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []string{"retry_key", "status", "attempts", "max_attempts", "lease_owner", "fencing_token", "retry_at", "lease_expires_at", "last_error", "completed_at", "updated_at"}, viewColumns)

	require.NoError(t, db.QueryRowContext(t.Context(), query, id).Scan(&after))
	require.JSONEq(t, string(before), string(after))
	repo := New(db)
	_, err = repo.Retry(t.Context(), jobs.RetryParams{ID: id, WorkerID: "old", FencingToken: 7, Error: "retry"})
	require.NoError(t, err)
	msgs, err := repo.FindAvailableJobs(t.Context(), jobs.FindAvailableJobsParams{WorkerID: "new", Limit: 1, LeaseSeconds: 30})
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, int32(2), msgs[0].Attempts)
	require.Equal(t, int64(8), msgs[0].FencingToken)
	require.Equal(t, "null", string(msgs[0].Request))
	require.Equal(t, "null", string(msgs[0].Response))
	_, err = repo.Complete(t.Context(), jobs.CompleteParams{ID: id, WorkerID: "old", FencingToken: 7, Response: []byte(`null`)})
	require.ErrorIs(t, err, jobs.ErrNoRows)
}
