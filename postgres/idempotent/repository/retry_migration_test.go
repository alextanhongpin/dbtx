package repository

import (
	"database/sql"
	"testing"

	"github.com/alextanhongpin/dbtx/postgres/idempotent"
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
	id := "migration-key"
	_, err := db.ExecContext(t.Context(), `insert into dbtx.idempotency_keys(idempotency_key, request, status, attempts, fencing_token, lease_owner, lease_expires_at, error) values ($1, 'null', 'in_progress', 1, nextval('dbtx.idempotency_fencing_token_seq'), 'old', now() + interval '30 seconds', 'previous')`, id)
	require.NoError(t, err)
	var token int64
	require.NoError(t, db.QueryRowContext(t.Context(), "select fencing_token from dbtx.idempotency_keys where idempotency_key = $1", id).Scan(&token))
	var before, after []byte
	query := "select to_jsonb(t) from dbtx.idempotency_keys t where idempotency_key = $1"
	require.NoError(t, db.QueryRowContext(t.Context(), query, id).Scan(&before))
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), Migrations+Migrations)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var viewColumns []string
	rows, err := db.QueryContext(t.Context(), `select attname from pg_attribute
        where attrelid = 'dbtx.idempotent_retry_state'::regclass and attnum > 0 and not attisdropped order by attnum`)
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
	err = repo.Nack(t.Context(), idempotent.NackParams{IdempotencyKey: id, FencingToken: token, MaxAttempts: 2, Error: "retry", TtlSeconds: 3600})
	require.NoError(t, err)
	claim, err := repo.Claim(t.Context(), idempotent.ClaimParams{IdempotencyKey: id, Request: []byte(`null`), LeaseOwner: "new", LeaseSeconds: 30, MaxAttempts: 2, TtlSeconds: 3600})
	require.NoError(t, err)
	require.Equal(t, idempotent.OutcomeResumed, claim.Outcome)
	require.Equal(t, int32(2), claim.Attempts)
	require.NotEqual(t, token, claim.FencingToken)
	require.ErrorIs(t, repo.Ack(t.Context(), idempotent.AckParams{IdempotencyKey: id, FencingToken: token, Response: []byte(`null`), TtlSeconds: 3600}), idempotent.ErrClaimed)
}
