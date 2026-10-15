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
			_, err = db.Exec(baseSchema + `CREATE VIEW dbtx.idempotent_retry_state AS SELECT * FROM dbtx.idempotency_keys;
CREATE FUNCTION dbtx.idempotent_retry(text, text, bigint, jsonb) RETURNS SETOF dbtx.idempotency_keys
LANGUAGE sql AS $$ SELECT * FROM dbtx.idempotency_keys WHERE false $$;`)
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
	var removed bool
	require.NoError(t, db.QueryRowContext(t.Context(), "select to_regclass('dbtx.idempotent_retry_state') is null").Scan(&removed))
	require.True(t, removed, "obsolete lifecycle view remains")
	require.NoError(t, db.QueryRowContext(t.Context(), "select to_regprocedure('dbtx.idempotent_retry(text, text, bigint, jsonb)') is null").Scan(&removed))
	require.True(t, removed, "obsolete adapter remains")

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
