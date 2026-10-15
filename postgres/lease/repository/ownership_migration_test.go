package repository

import (
	"database/sql"
	"testing"

	"github.com/alextanhongpin/dbtx/postgres/lease/repository/postgres"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	_ "github.com/lib/pq"
)

func TestSharedOwnershipMigrationPreservesResourceHistory(t *testing.T) {
	client := dbtest.New(t, dbtest.Options{Image: "postgres:17.4", Hook: func(dsn string) error {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = db.Exec(baseSchema)
		return err
	}})
	db := client.DB(t)
	_, err := db.ExecContext(t.Context(), `INSERT INTO dbtx.leases VALUES('scope','key','old',7,clock_timestamp()+interval '1 hour')`)
	if err != nil {
		t.Fatal(err)
	}
	var before, after []byte
	query := `SELECT to_jsonb(t) FROM dbtx.leases t WHERE namespace='scope' AND key='key'`
	if err := db.QueryRowContext(t.Context(), query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(t.Context(), OwnershipMigrations+OwnershipMigrations)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), query).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("migration changed persisted ownership")
	}
	q := postgres.New(db)
	_, err = q.ReleaseLease(t.Context(), postgres.ReleaseLeaseParams{Namespace: "scope", Key: "key", OwnerID: sql.NullString{String: "old", Valid: true}, Token: 7})
	if err != nil {
		t.Fatal(err)
	}
	token, err := q.AcquireLease(t.Context(), postgres.AcquireLeaseParams{Namespace: "scope", Key: "key", OwnerID: "new", TtlUs: 1000000})
	if err != nil {
		t.Fatal(err)
	}
	if token != 8 {
		t.Fatalf("token=%d, want 8", token)
	}
	renewed, err := q.RenewLease(t.Context(), postgres.RenewLeaseParams{Namespace: "scope", Key: "key", OwnerID: "old", Token: 7, TtlUs: 1000000})
	if err != nil {
		t.Fatal(err)
	}
	if renewed {
		t.Fatal("stale owner renewed successor")
	}
}
