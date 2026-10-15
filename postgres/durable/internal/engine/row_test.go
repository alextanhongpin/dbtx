package engine_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/durable/durabletest"
	"github.com/alextanhongpin/dbtx/postgres/durable/internal/engine"
	"github.com/alextanhongpin/dbtx/postgres/durable/repository/postgres"
)

// Every table that uses the shared lease functions must have the lease columns.
func TestLeaseContract(t *testing.T) {
	q := postgres.New(durabletest.NewDB(t))
	for table, full := range map[string]bool{
		"durable.executions":       true,
		"durable.outbox_events":    true,
		"durable.outbox_streams":   false,
		"durable.idempotency_keys": false,
	} {
		missing, err := q.LeaseContract(t.Context(), postgres.LeaseContractParams{Tbl: table, FullContract: full})
		if err != nil || len(missing) != 0 {
			t.Errorf("%s: missing %v (%v)", table, missing, err)
		}
	}
	missing, _ := q.LeaseContract(t.Context(), postgres.LeaseContractParams{Tbl: "durable.schedules", FullContract: false})
	if len(missing) == 0 {
		t.Error("contract check accepts a table without lease columns")
	}
}

// The same transitions behave identically on any conforming table; here a
// throwaway table proves the functions are not tied to the durable tables.
func TestSharedLeaseTransitions(t *testing.T) {
	ctx := t.Context()
	db := durabletest.NewDB(t)
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE work (
			id uuid PRIMARY KEY, status text NOT NULL, owner_id text, token bigint NOT NULL DEFAULT 0,
			lease_expires_at timestamptz, updated_at timestamptz NOT NULL DEFAULT now(),
			run_at timestamptz NOT NULL DEFAULT now(), attempts integer NOT NULL DEFAULT 0,
			max_attempts integer NOT NULL DEFAULT 2, last_error text)`); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewV7()
	if _, err := db.ExecContext(ctx, `INSERT INTO work (id, status) VALUES ($1, 'ready')`, id); err != nil {
		t.Fatal(err)
	}
	q := postgres.New(db)
	grant := func(owner string) (int64, bool) {
		var token int64
		err := db.QueryRowContext(ctx, `SELECT token FROM durable.lease_grant('work', ARRAY[$1::uuid], $2, 200000, 'ready', 'busy', true)`,
			id, owner).Scan(&token)
		return token, err == nil
	}
	tokA, ok := grant("A")
	if !ok {
		t.Fatal("first grant failed")
	}
	if _, ok := grant("B"); ok {
		t.Fatal("granted a held row")
	}
	a := engine.Row{Table: "work", Running: "busy", ID: id, Owner: "A", Token: tokA}
	if ok, err := a.Renew(ctx, q, 200*time.Millisecond); !ok || err != nil {
		t.Fatalf("renew: %v %v", ok, err)
	}
	stale := a
	stale.Token--
	if ok, _ := stale.Renew(ctx, q, time.Second); ok {
		t.Fatal("renewed with a stale token")
	}
	time.Sleep(250 * time.Millisecond) // A's lease expires
	tokB, ok := grant("B")
	if !ok || tokB <= tokA {
		t.Fatalf("takeover: %d %v", tokB, ok)
	}
	if ok, _ := a.Finish(ctx, q, engine.Finish{Status: "done", RequireUnexpired: true}); ok {
		t.Fatal("expired owner finished the row")
	}
	b := engine.Row{Table: "work", Running: "busy", ID: id, Owner: "B", Token: tokB}
	msg := "boom"
	if ok, err := b.Finish(ctx, q, engine.Finish{Status: "ready", RequireUnexpired: true,
		Delay: engine.Ptr(time.Duration(0)), LastError: &msg, AttemptDelta: -1}); !ok || err != nil {
		t.Fatalf("finish: %v %v", ok, err)
	}
	var (
		status   string
		attempts int
		lastErr  string
		owner    *string
	)
	_ = db.QueryRowContext(ctx, `SELECT status, attempts, last_error, owner_id FROM work`).Scan(&status, &attempts, &lastErr, &owner)
	if status != "ready" || attempts != 1 || lastErr != "boom" || owner != nil {
		t.Fatalf("status=%s attempts=%d last_error=%s owner=%v", status, attempts, lastErr, owner)
	}
	tokC, _ := grant("C")
	c := engine.Row{Table: "work", Running: "busy", ID: id, Owner: "C", Token: tokC}
	time.Sleep(250 * time.Millisecond)
	if ok, err := c.Expire(context.Background(), q, "dead", "lease expired"); !ok || err != nil {
		t.Fatalf("expire: %v %v", ok, err)
	}
	if _, ok := grant("D"); ok {
		t.Fatal("granted a dead row")
	}
}
