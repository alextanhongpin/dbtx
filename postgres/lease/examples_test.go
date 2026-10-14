package lease_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/lease"
	_ "github.com/lib/pq"
)

func ExampleNew() {
	// Opening the pool and constructing the locker do not connect or install DDL.
	db, err := sql.Open("postgres", "")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	_, err = lease.New(lease.Config{DB: dbtx.New(db), Namespace: "billing"})
	fmt.Println(err == nil)
	// Output: true
}

func ExampleLocker_RunInFencedTx() {
	db, err := sql.Open("postgres", "postgres://localhost/app?sslmode=disable")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	atomic := dbtx.New(db)
	locker, err := lease.New(lease.Config{DB: atomic, Namespace: "billing"})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = locker.RunInFencedTx(ctx, "account:123", func(txCtx context.Context, held lease.Lease) error {
		_, err := atomic.DBTx(txCtx).ExecContext(txCtx,
			`UPDATE accounts SET last_fence=$2 WHERE id=$1`, "123", int64(held.Token()))
		return err
	})
	if err != nil {
		var outcome *lease.TxOutcomeError
		if errors.As(err, &outcome) && outcome.Outcome == lease.TxUnknown {
			// Reconcile a durable operation receipt before retrying.
		}
	}
}

func ExampleLocker_Activity() {
	db, err := sql.Open("postgres", "postgres://localhost/app?sslmode=disable")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	atomic := dbtx.New(db)
	locker, err := lease.New(lease.Config{DB: atomic, Namespace: "reports"})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Real applications compute or call external APIs here; those operations
	// require provider-supported idempotency or resource-side fencing.
	compute := func(ctx context.Context) (string, error) { return "report", context.Cause(ctx) }
	err = locker.Activity(ctx, "report:daily", func(activityCtx context.Context, held lease.Lease) error {
		report, err := compute(activityCtx)
		if err != nil {
			return err
		}
		return held.RunInFencedTx(activityCtx, func(txCtx context.Context) error {
			_, err := atomic.DBTx(txCtx).ExecContext(txCtx,
				`UPDATE reports SET payload=$2, last_fence=$3 WHERE id=$1`, "daily", report, int64(held.Token()))
			return err
		})
	})
	_ = err // Handle outcome/reconciliation according to the application contract.
}

func ExampleLocker_TryActivity() {
	db, err := sql.Open("postgres", "postgres://localhost/app?sslmode=disable")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	locker, err := lease.New(lease.Config{DB: dbtx.New(db), Namespace: "maintenance"})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = locker.TryActivity(ctx, "cleanup", func(ctx context.Context, held lease.Lease) error {
		return context.Cause(ctx) // Replace with idempotent/fenced application work.
	})
	if errors.Is(err, lease.ErrContended) {
		// Another node owns the resource; skip this scheduled tick.
		return
	}
	_ = err
}
