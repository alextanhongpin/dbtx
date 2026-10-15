package retry

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/testing/dbtest"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const fixtureSchema = `
create type dbtx.retry_test_status as enum ('ready', 'running', 'succeeded', 'failed');
create table dbtx.retry_test_work (
    id text primary key, status dbtx.retry_test_status not null default 'ready',
    attempts int not null default 0, max_attempts int not null default 2,
    lease_owner text, fencing_token bigint not null default 0,
    retry_at timestamptz, lease_expires_at timestamptz,
    last_error text, completed_at timestamptz, updated_at timestamptz not null default now(),
    payload jsonb not null default 'null',
    check (status <> 'running' or (lease_owner is not null and lease_expires_at is not null))
);
create view dbtx.retry_test_state with (security_invoker = true) as
select t.*, id as retry_key from dbtx.retry_test_work t;
`

const testPolicy = `{"running":"running","ready":"ready","success":"succeeded","failure":"failed","check_token":true,"increment_token":true,"separate_lease":true,"complete_failure":true,"finish_time":"clear"}`

var testNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	stop := dbtest.Init(dbtest.Options{
		Image: "postgres:19beta3-alpine3.24",
		Hook: func(dsn string) error {
			db, err := sql.Open("postgres", dsn)
			if err != nil {
				return err
			}
			defer db.Close()
			_, err = db.Exec(Schema + Schema + fixtureSchema)
			return err
		},
	})
	defer stop()
	m.Run()
}

type state struct {
	Status         string     `json:"status"`
	Attempts       int32      `json:"attempts"`
	LeaseOwner     *string    `json:"lease_owner"`
	FencingToken   int64      `json:"fencing_token"`
	RetryAt        *time.Time `json:"retry_at"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at"`
	LastError      *string    `json:"last_error"`
	CompletedAt    *time.Time `json:"completed_at"`
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func transition(ctx context.Context, db queryer, key, action string, owner any, token int64, options string, now time.Time) (state, error) {
	var raw []byte
	err := db.QueryRowContext(ctx, `select * from dbtx.retry_transition(
        'dbtx.retry_test_state'::regclass, $1, $2, $3, $4, $5::jsonb, $6::jsonb, $7)`,
		key, action, owner, token, testPolicy, options, now).Scan(&raw)
	var result state
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}

func seed(t *testing.T, db *sql.DB, key string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `insert into dbtx.retry_test_work(id, retry_at) values($1, $2)`, key, testNow)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`delete from dbtx.retry_test_work where id = $1`, key)
		require.NoError(t, err)
	})
}

func TestLifecycle(t *testing.T) {
	db := dbtest.DB(t)
	key := t.Name() + `'; drop table dbtx.retry_test_work; --`
	seed(t, db, key)

	claimed, err := transition(t.Context(), db, key, "claim", "first", 0, `{"lease_seconds":10}`, testNow)
	require.NoError(t, err)
	require.Equal(t, int32(1), claimed.Attempts)
	require.Equal(t, int64(1), claimed.FencingToken)
	require.Equal(t, testNow.Add(10*time.Second), claimed.LeaseExpiresAt.UTC())
	_, err = transition(t.Context(), db, key, "claim", "second", 0, `{"lease_seconds":10}`, testNow)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = transition(t.Context(), db, key, "complete", nil, 1, `{}`, testNow)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = transition(t.Context(), db, key, "complete", "first", 0, `{}`, testNow)
	require.ErrorIs(t, err, sql.ErrNoRows)

	renewed, err := transition(t.Context(), db, key, "renew", "first", 1, `{"lease_seconds":20}`, testNow)
	require.NoError(t, err)
	require.Equal(t, testNow.Add(20*time.Second), renewed.LeaseExpiresAt.UTC())
	retried, err := transition(t.Context(), db, key, "nack", "first", 1, `{"error":"failed","delay_seconds":5}`, testNow)
	require.NoError(t, err)
	require.Equal(t, "ready", retried.Status)
	require.Nil(t, retried.LeaseOwner)
	require.Nil(t, retried.LeaseExpiresAt)
	require.Equal(t, testNow.Add(5*time.Second), retried.RetryAt.UTC())
	_, err = transition(t.Context(), db, key, "claim", "second", 0, `{"lease_seconds":10}`, testNow)
	require.ErrorIs(t, err, sql.ErrNoRows)
	claimed, err = transition(t.Context(), db, key, "claim", "second", 0, `{"lease_seconds":10}`, testNow.Add(5*time.Second))
	require.NoError(t, err)
	require.Equal(t, int32(2), claimed.Attempts)
	require.Equal(t, int64(2), claimed.FencingToken)
	_, err = transition(t.Context(), db, key, "nack", "first", 1, `{"delay_seconds":0}`, testNow)
	require.ErrorIs(t, err, sql.ErrNoRows)
	failed, err := transition(t.Context(), db, key, "nack", "second", 2, `{"error":"last","delay_seconds":0}`, testNow.Add(5*time.Second))
	require.NoError(t, err)
	require.Equal(t, "failed", failed.Status)
	require.NotNil(t, failed.CompletedAt)
	_, err = transition(t.Context(), db, key, "claim", "third", 0, `{"lease_seconds":10}`, testNow.Add(time.Hour))
	require.ErrorIs(t, err, sql.ErrNoRows)
	reset, err := transition(t.Context(), db, key, "requeue", nil, 0, `{}`, testNow)
	require.NoError(t, err)
	require.Zero(t, reset.Attempts)
	require.Nil(t, reset.LastError)

	var payload []byte
	require.NoError(t, db.QueryRow(`select payload from dbtx.retry_test_work where id = $1`, key).Scan(&payload))
	require.Equal(t, "null", string(payload))
}

func TestRollbackAndSkipLocked(t *testing.T) {
	db := dbtest.DB(t)
	seed(t, db, t.Name())
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = transition(t.Context(), tx, t.Name(), "claim", "first", 0, `{"lease_seconds":10}`, testNow)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = transition(ctx, db, t.Name(), "claim", "second", 0, `{"lease_seconds":10}`, testNow)
	require.ErrorIs(t, err, sql.ErrNoRows) // Must skip the locked row, not time out.
	require.NoError(t, tx.Rollback())
	claimed, err := transition(t.Context(), db, t.Name(), "claim", "second", 0, `{"lease_seconds":10}`, testNow)
	require.NoError(t, err)
	require.Equal(t, int32(1), claimed.Attempts)
	require.Equal(t, int64(1), claimed.FencingToken)
	released, err := transition(t.Context(), db, t.Name(), "release", "second", 1, `{}`, testNow)
	require.NoError(t, err)
	require.Zero(t, released.Attempts)
	require.Equal(t, "ready", released.Status)
}

func TestTakeoverAndTerminalOutcomes(t *testing.T) {
	for _, action := range []string{"complete", "fail", "expire"} {
		t.Run(action, func(t *testing.T) {
			db := dbtest.DB(t)
			seed(t, db, t.Name())
			_, err := transition(t.Context(), db, t.Name(), "claim", "first", 0, `{"lease_seconds":1}`, testNow)
			require.NoError(t, err)
			_, err = transition(t.Context(), db, t.Name(), "claim", "second", 0, `{"lease_seconds":1}`, testNow.Add(time.Second))
			require.NoError(t, err)
			_, err = transition(t.Context(), db, t.Name(), "complete", "first", 1, `{}`, testNow.Add(time.Second))
			require.ErrorIs(t, err, sql.ErrNoRows)
			finished, err := transition(t.Context(), db, t.Name(), action, "second", 2, `{"error":"permanent"}`, testNow.Add(2*time.Second))
			require.NoError(t, err)
			expected := "failed"
			if action == "complete" {
				expected = "succeeded"
			}
			require.Equal(t, expected, finished.Status)
			require.Nil(t, finished.LeaseOwner)
			require.NotNil(t, finished.CompletedAt)
		})
	}
}

func TestWaitingCompletionRechecksOwnership(t *testing.T) {
	db := dbtest.DB(t)
	seed(t, db, t.Name())
	_, err := transition(t.Context(), db, t.Name(), "claim", "old", 0, `{"lease_seconds":1}`, testNow)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = transition(t.Context(), tx, t.Name(), "claim", "new", 0, `{"lease_seconds":1}`, testNow.Add(time.Second))
	require.NoError(t, err)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	var pid int
	require.NoError(t, conn.QueryRowContext(t.Context(), `select pg_backend_pid()`).Scan(&pid))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := transition(ctx, conn, t.Name(), "complete", "old", 1, `{}`, testNow.Add(time.Second))
		done <- err
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		err := db.QueryRow(`select coalesce(wait_event_type = 'Lock', false) from pg_stat_activity where pid = $1`, pid).Scan(&blocked)
		return err == nil && blocked
	}, time.Second, time.Millisecond)
	require.NoError(t, tx.Commit())
	require.ErrorIs(t, <-done, sql.ErrNoRows)
	var owner string
	require.NoError(t, db.QueryRow(`select lease_owner from dbtx.retry_test_work where id = $1`, t.Name()).Scan(&owner))
	require.Equal(t, "new", owner)
}

func TestInvokerPreservesRowSecurity(t *testing.T) {
	db := dbtest.DB(t)
	seed(t, db, "allowed")
	seed(t, db, "forbidden")
	_, err := db.Exec(`
        create role retry_test_worker;
        grant usage on schema dbtx to retry_test_worker;
        grant select, update on dbtx.retry_test_work, dbtx.retry_test_state to retry_test_worker;
        alter table dbtx.retry_test_work enable row level security;
        create policy retry_test_access on dbtx.retry_test_work using (id = 'allowed');
    `)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec(`
            alter table dbtx.retry_test_work disable row level security;
            drop policy retry_test_access on dbtx.retry_test_work;
            drop owned by retry_test_worker;
            drop role retry_test_worker;
        `)
		require.NoError(t, err)
	})
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), `set local role retry_test_worker`)
	require.NoError(t, err)
	_, err = transition(t.Context(), tx, "allowed", "claim", "worker", 0, `{"lease_seconds":1}`, testNow)
	require.NoError(t, err)
	_, err = transition(t.Context(), tx, "forbidden", "claim", "worker", 0, `{"lease_seconds":1}`, testNow)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, tx.Rollback())
}

func TestRetryDelay(t *testing.T) {
	db := dbtest.DB(t)
	for _, tc := range []struct {
		attempt         int
		base, cap, want float64
	}{
		{1, 1, 10, 1}, {2, 1, 10, 2}, {10, 1, 10, 10},
		{math.MaxInt32, 0.001, 3600, 3600}, {1, 10, 1, 1},
		{1, 0, 10, 0}, {1, 1, 0, 0}, {1, -1, 10, 0},
	} {
		var delay float64
		require.NoError(t, db.QueryRow(`select dbtx.retry_delay($1,$2,$3)`, tc.attempt, tc.base, tc.cap).Scan(&delay))
		require.Equal(t, tc.want, delay)
	}
}

func TestSchemaBundles(t *testing.T) {
	ownership, err := os.ReadFile(filepath.Join("..", "lease", "ownership", "schema.sql"))
	require.NoError(t, err)
	require.Equal(t, "-- Code generated by postgres/lease/ownership/cmd/bundle; DO NOT EDIT.\n"+string(ownership), ownershipSchema)
	for _, module := range []string{"idempotent", "inbox", "jobs", "outbox"} {
		bundle, err := os.ReadFile(filepath.Join("..", module, "repository", "shared_retry.sql"))
		require.NoError(t, err)
		require.Equal(t, "-- Code generated by postgres/retry/cmd/bundle; DO NOT EDIT.\n"+Schema, string(bundle))
	}
}
