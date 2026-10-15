package retry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/testing/dbtest"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func database(t *testing.T) *sql.DB {
	t.Helper()
	dsn := dbtest.DSN()
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	name := "ownership_test_" + strings.ToLower(rand.Text()[:12])
	_, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+name)
	require.NoError(t, err)
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		require.NoError(t, err)
	})
	_, err = db.ExecContext(t.Context(), Schema+Schema+`
        CREATE SEQUENCE dbtx.test_seq;
        CREATE TABLE dbtx.work (
            id uuid primary key, status text not null default 'running',
            owner_id text, token bigint not null default 0,
            lease_expires_at timestamptz, updated_at timestamptz not null default now());
        CREATE VIEW dbtx.work_ownership WITH (security_invoker=true) AS SELECT * FROM dbtx.work;
        CREATE TABLE dbtx.resources (
            namespace text, key text, owner_id text, fencing_token bigint not null default 0,
            expires_at timestamptz not null default '-infinity', PRIMARY KEY(namespace,key));
        CREATE VIEW dbtx.resource_ownership WITH (security_invoker=true) AS
            SELECT namespace,key AS id,owner_id,fencing_token AS token,expires_at AS lease_expires_at FROM dbtx.resources;
    `)
	require.NoError(t, err)
	return db
}

func TestStrictAndPermissiveExpiry(t *testing.T) {
	db := database(t)
	id := uuid.NewV7()
	_, err := db.ExecContext(t.Context(), `INSERT INTO dbtx.work(id,owner_id,token,lease_expires_at) VALUES($1,'owner',7,clock_timestamp()-interval '1 second')`, id)
	require.NoError(t, err)
	for _, tc := range []struct {
		owner        string
		token        int64
		strict, want bool
	}{
		{"owner", 7, true, false}, {"stale", 7, false, false}, {"owner", 6, false, false}, {"owner", 7, false, true},
	} {
		var ok bool
		err := db.QueryRowContext(t.Context(), `SELECT dbtx.ownership_lock('dbtx.work', $1, NULL,$2,$3,'running',$4,true,true)`, id.String(), tc.owner, tc.token, tc.strict).Scan(&ok)
		require.NoError(t, err)
		require.Equal(t, tc.want, ok)
	}
	var ok bool
	err = db.QueryRowContext(t.Context(), `SELECT dbtx.ownership_renew('dbtx.work',$1,NULL,'owner',7,'running',true,true,true,interval '1 second',true,NULL)`, id.String()).Scan(&ok)
	require.NoError(t, err)
	require.False(t, ok)
	err = db.QueryRowContext(t.Context(), `SELECT dbtx.ownership_renew('dbtx.work',$1,NULL,'owner',7,'running',false,true,true,interval '1 second',true,NULL)`, id.String()).Scan(&ok)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = db.ExecContext(t.Context(), `UPDATE dbtx.work SET owner_id='next',token=8 WHERE id=$1`, id)
	require.NoError(t, err)
	err = db.QueryRowContext(t.Context(), `SELECT dbtx.ownership_release('dbtx.work',$1,NULL,'owner',7,'running',false,true,true,NULL,true,NULL)`, id.String()).Scan(&ok)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestCompositeKeysAndTokenHistory(t *testing.T) {
	db := database(t)
	key := `'; DROP TABLE dbtx.resources; --`
	for _, ns := range []string{"one", "two"} {
		_, err := db.ExecContext(t.Context(), `INSERT INTO dbtx.resources(namespace,key) VALUES($1,$2)`, ns, key)
		require.NoError(t, err)
		var token int64
		err = db.QueryRowContext(t.Context(), `SELECT dbtx.ownership_grant('dbtx.resource_ownership',$1,$2,'owner',interval '1 hour',NULL,true,false,NULL)`, key, ns).Scan(&token)
		require.NoError(t, err)
		require.Equal(t, int64(1), token)
	}
	var ok bool
	err := db.QueryRowContext(t.Context(), `SELECT dbtx.ownership_release('dbtx.resource_ownership',$1,'one','owner',1,NULL,false,true,true,'-infinity',false,NULL)`, key).Scan(&ok)
	require.NoError(t, err)
	require.True(t, ok)
	var next int64
	err = db.QueryRowContext(t.Context(), `SELECT dbtx.ownership_grant('dbtx.resource_ownership',$1,'one','next',interval '1 hour',NULL,true,false,NULL)`, key).Scan(&next)
	require.NoError(t, err)
	require.Equal(t, int64(2), next)
	// Purgeable work uses a sequence instead of resetting a per-row generation.
	id := uuid.NewV7()
	var previous int64
	for range 2 {
		_, err = db.ExecContext(t.Context(), `INSERT INTO dbtx.work(id) VALUES($1)`, id)
		require.NoError(t, err)
		err = db.QueryRowContext(t.Context(), `SELECT dbtx.ownership_grant('dbtx.work',$1,NULL,'owner',interval '1 hour','dbtx.test_seq',true,true,NULL)`, id.String()).Scan(&next)
		require.NoError(t, err)
		require.Greater(t, next, previous)
		previous = next
		_, err = db.ExecContext(t.Context(), `DELETE FROM dbtx.work WHERE id=$1`, id)
		require.NoError(t, err)
	}
}

func TestExpiryAfterLockWait(t *testing.T) {
	db := database(t)
	id := uuid.NewV7()
	_, err := db.ExecContext(t.Context(), `INSERT INTO dbtx.work(id,owner_id,token,lease_expires_at) VALUES($1,'owner',1,clock_timestamp()+interval '250 milliseconds')`, id)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), `SELECT id FROM dbtx.work WHERE id=$1 FOR UPDATE`, id)
	require.NoError(t, err)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	var pid int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		r.err = conn.QueryRowContext(ctx, `SELECT dbtx.ownership_renew('dbtx.work',$1,NULL,'owner',1,'running',true,true,true,interval '1 hour',true,NULL)`, id.String()).Scan(&r.ok)
		done <- r
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRowContext(ctx, `SELECT coalesce(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, time.Millisecond)
	// No row version is changed while the waiter sleeps. Expiry must be checked
	// after locking even when PostgreSQL need not re-evaluate a modified tuple.
	require.Eventually(t, func() bool {
		var expired bool
		err := db.QueryRowContext(ctx, `SELECT lease_expires_at<=clock_timestamp() FROM dbtx.work WHERE id=$1`, id).Scan(&expired)
		return err == nil && expired
	}, 3*time.Second, 5*time.Millisecond)
	require.NoError(t, tx.Commit())
	r := <-done
	require.NoError(t, r.err)
	require.False(t, r.ok)
}

func TestRollbackAndRowSecurity(t *testing.T) {
	db := database(t)
	allowed, denied := uuid.NewV7(), uuid.NewV7()
	_, err := db.ExecContext(t.Context(), `INSERT INTO dbtx.work(id) VALUES($1),($2)`, allowed, denied)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var token int64
	err = tx.QueryRowContext(t.Context(), `SELECT dbtx.ownership_grant('dbtx.work',$1,NULL,'owner',interval '1 hour',NULL,true,true,NULL)`, allowed.String()).Scan(&token)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	var owner sql.NullString
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT owner_id,token FROM dbtx.work WHERE id=$1`, allowed).Scan(&owner, &token))
	require.False(t, owner.Valid)
	require.Zero(t, token)
	role := "ownership_worker_" + strings.ToLower(rand.Text()[:10])
	_, err = db.ExecContext(t.Context(), `CREATE ROLE `+role+`;
        GRANT USAGE ON SCHEMA dbtx TO `+role+`;
        GRANT SELECT,UPDATE ON dbtx.work,dbtx.work_ownership TO `+role+`;
        ALTER TABLE dbtx.work ENABLE ROW LEVEL SECURITY;
        CREATE POLICY only_allowed ON dbtx.work USING (id='`+allowed.String()+`');`)
	require.NoError(t, err)
	t.Cleanup(func() { _, err := db.Exec(`DROP OWNED BY ` + role + `; DROP ROLE ` + role); require.NoError(t, err) })
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), `SET LOCAL ROLE `+role)
	require.NoError(t, err)
	for _, id := range []uuid.UUID{allowed, denied} {
		err = tx.QueryRowContext(t.Context(), `SELECT dbtx.ownership_grant('dbtx.work_ownership',$1,NULL,'owner',interval '1 hour',NULL,true,true,NULL)`, id.String()).Scan(&token)
		require.NoError(t, err)
		if id == allowed {
			require.Equal(t, int64(1), token)
		} else {
			require.Zero(t, token)
		}
	}
	require.NoError(t, tx.Rollback())
}
