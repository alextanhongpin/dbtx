package repository

import (
	"database/sql"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/jobs/repository/postgres"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Verify the native claim/heartbeat/complete workload writes each row once. Tuple
// counts are transaction-local and deterministic. WAL and elapsed time are
// observations only: background WAL and host load can affect them.
func TestNativeTransitionWriteCost(t *testing.T) {
	client := dbtest.New(t, dbtest.Options{Image: "postgres:19beta3-alpine3.24", Hook: func(dsn string) error {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = db.Exec(baseSchema)
		return err
	}})
	db := client.DB(t)
	const batch = 100
	t.Run("native", func(t *testing.T) {
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer tx.Rollback()
		_, err = tx.ExecContext(t.Context(), `insert into dbtx.jobs(idempotency_key,request,max_attempts,visible_at)
     select 'cost-' || n, '{}', 3, now() from generate_series(1,$1) n`, batch)
		require.NoError(t, err)
		var beforeUpdates int64
		require.NoError(t, tx.QueryRowContext(t.Context(), `select n_tup_upd from pg_stat_xact_user_tables where relid='dbtx.jobs'::regclass`).Scan(&beforeUpdates))
		var startLSN string
		require.NoError(t, tx.QueryRowContext(t.Context(), `select pg_current_wal_insert_lsn()::text`).Scan(&startLSN))
		started := time.Now()
		q := postgres.New(tx)
		var claimed []*postgres.DbtxJob
		claimed, err = q.FindAvailableJobs(t.Context(), postgres.FindAvailableJobsParams{WorkerID: "cost", LeaseSeconds: 60, Limit: batch})
		require.NoError(t, err)
		require.Len(t, claimed, batch)
		for _, job := range claimed {
			_, err = q.Heartbeat(t.Context(), postgres.HeartbeatParams{ID: job.ID, WorkerID: "cost", FencingToken: job.FencingToken, LeaseSeconds: 60})
			require.NoError(t, err)
			_, err = q.Complete(t.Context(), postgres.CompleteParams{ID: job.ID, WorkerID: "cost", FencingToken: job.FencingToken, Response: jsontext.Value(`null`)})
			require.NoError(t, err)
		}
		elapsed := time.Since(started)
		var updates, wal int64
		require.NoError(t, tx.QueryRowContext(t.Context(), `select n_tup_upd from pg_stat_xact_user_tables where relid='dbtx.jobs'::regclass`).Scan(&updates))
		require.NoError(t, tx.QueryRowContext(t.Context(), `select pg_wal_lsn_diff(pg_current_wal_insert_lsn(),$1::pg_lsn)::bigint`, startLSN).Scan(&wal))
		expected := int64(3 * batch)
		updates -= beforeUpdates
		require.Equal(t, expected, updates)
		t.Logf("native: %.0f jobs/s, %d tuple updates/job, %d WAL bytes/job (%d jobs)", float64(batch)/elapsed.Seconds(), updates/batch, wal/batch, batch)
	})
}
