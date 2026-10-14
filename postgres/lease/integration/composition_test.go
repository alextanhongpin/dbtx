// Package integration_test verifies public API composition using real PostgreSQL.
// It is a separate test module so lease consumers do not acquire dependencies on
// unrelated durable modules just to use coordination.
package integration_test

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx"
	"github.com/alextanhongpin/dbtx/postgres/idempotent"
	idprepo "github.com/alextanhongpin/dbtx/postgres/idempotent/repository"
	"github.com/alextanhongpin/dbtx/postgres/inbox"
	inrepo "github.com/alextanhongpin/dbtx/postgres/inbox/repository"
	"github.com/alextanhongpin/dbtx/postgres/jobs"
	jobrepo "github.com/alextanhongpin/dbtx/postgres/jobs/repository"
	"github.com/alextanhongpin/dbtx/postgres/lease"
	leaserepo "github.com/alextanhongpin/dbtx/postgres/lease/repository"
	"github.com/alextanhongpin/dbtx/postgres/outbox"
	outrepo "github.com/alextanhongpin/dbtx/postgres/outbox/repository"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	_ "github.com/lib/pq"
)

func TestComposition(t *testing.T) {
	client := dbtest.New(t, dbtest.Options{Image: "postgres:19beta3-alpine3.24", Duration: 5 * time.Minute})
	db := client.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, schema := range []string{leaserepo.Schema, outrepo.Schema, inrepo.Schema, idprepo.Schema, jobrepo.Schema} {
		if _, err := db.ExecContext(ctx, schema); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE integration_effects (
  operation_id text PRIMARY KEY, resource_id text NOT NULL, value text NOT NULL
 )`); err != nil {
		t.Fatal(err)
	}
	unit := dbtx.New(db)
	var sequence atomic.Int64
	unique := func() string { return fmt.Sprint("composition-", sequence.Add(1)) }
	namespaces := make(map[*lease.Locker]string)
	newLocker := func(t *testing.T) *lease.Locker {
		t.Helper()
		namespace := unique()
		l, err := lease.New(lease.Config{DB: unit, Namespace: namespace})
		if err != nil {
			t.Fatal(err)
		}
		namespaces[l] = namespace
		return l
	}
	count := func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	write := func(ctx context.Context, operation, resource string) error {
		_, err := unit.DBTx(ctx).ExecContext(ctx,
			`INSERT INTO integration_effects VALUES ($1, $2, 'applied')`, operation, resource)
		return err
	}
	outRepository := outrepo.New(db)
	publisher := outbox.New(outRepository)
	publisher.RequireTx = true
	inRepository := inrepo.New(db)
	consumer := inbox.New(inRepository)
	idpRepository := idprepo.New(db)
	idp := idempotent.New(idpRepository)
	jobRepository := jobrepo.New(db)
	submitter := jobs.NewSubmitter(jobRepository, jobs.SubmitConfig{})

	t.Run("fenced business write and outbox enqueue commit or rollback together", func(t *testing.T) {
		l := newLocker(t)
		for _, rollback := range []bool{false, true} {
			operation := unique()
			failure := errors.New("abort after enqueue")
			err := l.RunInFencedTx(ctx, "account:123", func(txCtx context.Context, held lease.Lease) error {
				if err := write(txCtx, operation, "account:123"); err != nil {
					return err
				}
				if _, err := publisher.Enqueue(txCtx, outbox.EnqueueParams{
					AggregateType: "Account", AggregateID: operation, EventType: "AccountChanged",
					Payload: jsontext.Value(`{"changed":true}`),
				}); err != nil {
					return err
				}
				if rollback {
					return failure
				}
				return nil
			})
			if rollback {
				if !errors.Is(err, failure) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			expected := 1
			if rollback {
				expected = 0
			}
			if n := count(t, `SELECT count(*) FROM integration_effects WHERE operation_id=$1`, operation); n != expected {
				t.Fatalf("effects=%d expected=%d", n, expected)
			}
			if n := count(t, `SELECT count(*) FROM dbtx.outbox WHERE aggregate_id=$1`, operation); n != expected {
				t.Fatalf("events=%d expected=%d", n, expected)
			}
		}
	})

	t.Run("activity finalization can enqueue an outbox event", func(t *testing.T) {
		l := newLocker(t)
		operation := unique()
		err := l.Activity(ctx, "account:123", func(activityCtx context.Context, held lease.Lease) error {
			if unit.IsTx(activityCtx) {
				t.Error("activity retains transaction")
			}
			return held.RunInFencedTx(activityCtx, func(txCtx context.Context) error {
				if err := write(txCtx, operation, "account:123"); err != nil {
					return err
				}
				_, err := publisher.Enqueue(txCtx, outbox.EnqueueParams{
					AggregateType: "Account", AggregateID: operation, EventType: "AccountChanged",
					Payload: jsontext.Value(`{"changed":true}`),
				})
				return err
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		if n := count(t, `SELECT count(*) FROM dbtx.outbox WHERE aggregate_id=$1`, operation); n != 1 {
			t.Fatal(n)
		}
	})

	t.Run("inbox enqueue joins fenced transaction and retains deduplication", func(t *testing.T) {
		l := newLocker(t)
		for _, rollback := range []bool{false, true} {
			message := unique()
			failure := errors.New("abort after storing message")
			params := inbox.EnqueueParams{Source: "composition", MessageID: message, MessageType: "PaymentCaptured", Payload: jsontext.Value(`{"captured":true}`)}
			err := l.RunInFencedTx(ctx, "order:123", func(txCtx context.Context, held lease.Lease) error {
				if _, err := consumer.Enqueue(txCtx, params); err != nil {
					return err
				}
				if rollback {
					return failure
				}
				return nil
			})
			if rollback {
				if !errors.Is(err, failure) {
					t.Fatal(err)
				}
				if n := count(t, `SELECT count(*) FROM dbtx.inbox WHERE message_id=$1`, message); n != 0 {
					t.Fatal(n)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := consumer.Enqueue(ctx, params); !errors.Is(err, inbox.ErrExists) {
					t.Fatal("duplicate not detected", err)
				}
			}
		}
	})

	t.Run("job submission joins fenced transaction without starting a worker", func(t *testing.T) {
		l := newLocker(t)
		for _, rollback := range []bool{false, true} {
			key := unique()
			failure := errors.New("abort after submit")
			err := l.RunInFencedTx(ctx, "account:123", func(txCtx context.Context, held lease.Lease) error {
				if err := write(txCtx, key, "account:123"); err != nil {
					return err
				}
				_, err := submitter.Submit(txCtx, jobs.SubmitInput{IdempotencyKey: key, Request: json.RawMessage(`{"account":"123"}`)})
				if err != nil {
					return err
				}
				if rollback {
					return failure
				}
				return nil
			})
			if rollback {
				if !errors.Is(err, failure) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			expected := 1
			if rollback {
				expected = 0
			}
			if n := count(t, `SELECT count(*) FROM dbtx.jobs WHERE idempotency_key=$1`, key); n != expected {
				t.Fatalf("jobs=%d expected=%d", n, expected)
			}
			if n := count(t, `SELECT count(*) FROM dbtx.job_keys WHERE idempotency_key=$1`, key); n != expected {
				t.Fatalf("job keys=%d expected=%d", n, expected)
			}
			if n := count(t, `SELECT count(*) FROM integration_effects WHERE operation_id=$1`, key); n != expected {
				t.Fatalf("effects=%d expected=%d", n, expected)
			}
		}
	})

	t.Run("custom IDs compose when repositories share the unit of work", func(t *testing.T) {
		named := dbtx.New(db)
		named.SetID("composition-billing")
		l, err := lease.New(lease.Config{DB: named, Namespace: unique()})
		if err != nil {
			t.Fatal(err)
		}
		repository := outrepo.New(db)
		repository.DB = named
		out := outbox.New(repository)
		out.RequireTx = true
		operation := unique()
		failure := errors.New("abort custom context")
		err = l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, held lease.Lease) error {
			_, err := out.Enqueue(txCtx, outbox.EnqueueParams{AggregateType: "Account", AggregateID: operation, EventType: "Changed", Payload: jsontext.Value(`{}`)})
			if err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		if n := count(t, `SELECT count(*) FROM dbtx.outbox WHERE aggregate_id=$1`, operation); n != 0 {
			t.Fatal("event escaped rollback", n)
		}
	})

	t.Run("mismatched context ID causes independent job submission", func(t *testing.T) {
		l := newLocker(t)
		mismatched := jobrepo.New(db)
		mismatched.SetID("different-transaction")
		producer := jobs.NewSubmitter(mismatched, jobs.SubmitConfig{})
		operation := unique()
		failure := errors.New("abort business transaction")
		err := l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, held lease.Lease) error {
			if err := write(txCtx, operation, "resource"); err != nil {
				return err
			}
			if _, err := producer.Submit(txCtx, jobs.SubmitInput{IdempotencyKey: operation, Request: json.RawMessage(`{}`)}); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		if n := count(t, `SELECT count(*) FROM integration_effects WHERE operation_id=$1`, operation); n != 0 {
			t.Fatal("business effect did not roll back", n)
		}
		if n := count(t, `SELECT count(*) FROM dbtx.jobs WHERE idempotency_key=$1`, operation); n != 1 {
			t.Fatal("expected independent submit for mismatched ID", n)
		}
		wrongOut := outrepo.New(db)
		wrongOut.SetID("different-transaction")
		guarded := outbox.New(wrongOut)
		guarded.RequireTx = true
		err = l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, held lease.Lease) error {
			_, err := guarded.Enqueue(txCtx, outbox.EnqueueParams{AggregateType: "Account", AggregateID: unique(), EventType: "Changed", Payload: jsontext.Value(`{}`)})
			return err
		})
		if !errors.Is(err, outbox.ErrNotInTx) {
			t.Fatal("RequireTx failed to detect wrong wiring", err)
		}
	})

	t.Run("idempotent Do cannot join fenced transaction", func(t *testing.T) {
		l := newLocker(t)
		called := false
		err := l.RunInFencedTx(ctx, "resource", func(txCtx context.Context, held lease.Lease) error {
			_, err := idp.Do(txCtx, unique(), func(context.Context, idempotent.Params) (*idempotent.Result, error) {
				called = true
				return nil, nil
			}, idempotent.Request{Data: []byte(`{}`)})
			return err
		})
		if !errors.Is(err, idempotent.ErrTxInContext) || called {
			t.Fatalf("%v called=%v", err, called)
		}
	})

	t.Run("idempotent response business effect and outbox remain atomic", func(t *testing.T) {
		operation := unique()
		calls := 0
		fn := func(stepCtx context.Context, p idempotent.Params) (*idempotent.Result, error) {
			calls++
			if err := write(stepCtx, operation, "account:123"); err != nil {
				return nil, err
			}
			if _, err := publisher.Enqueue(stepCtx, outbox.EnqueueParams{
				AggregateType: "Account", AggregateID: operation, EventType: "AccountChanged", Payload: jsontext.Value(`{}`),
			}); err != nil {
				return nil, err
			}
			return &idempotent.Result{Response: &idempotent.Response{Status: string(idempotent.StatusCompleted), Data: []byte(`{"ok":true}`)}}, nil
		}
		for range 2 {
			if _, err := idp.Do(ctx, operation, fn, idempotent.Request{Data: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}
		if calls != 1 || count(t, `SELECT count(*) FROM dbtx.outbox WHERE aggregate_id=$1`, operation) != 1 {
			t.Fatalf("handler calls=%d", calls)
		}
	})

	t.Run("outer activity expiry does not fence idempotent inner transaction", func(t *testing.T) {
		l := newLocker(t)
		operation := unique()
		// The outer resource lease and inner request claim are separate authorities.
		err := l.Activity(ctx, "resource", func(activityCtx context.Context, held lease.Lease) error {
			_, err := idp.Do(activityCtx, operation, func(stepCtx context.Context, p idempotent.Params) (*idempotent.Result, error) {
				// Simulate expiry while the independent idempotency row remains owned.
				if _, err := db.ExecContext(ctx, `UPDATE dbtx.leases SET expires_at=clock_timestamp()-interval '1 second' WHERE namespace=$1 AND key='resource'`, namespaces[l]); err != nil {
					return nil, err
				}
				if err := write(stepCtx, operation, "resource"); err != nil {
					return nil, err
				}
				return &idempotent.Result{Response: &idempotent.Response{Status: string(idempotent.StatusCompleted), Data: []byte(`{}`)}}, nil
			}, idempotent.Request{Data: []byte(`{}`)})
			if err != nil {
				return err
			}
			// Force authoritative detection after the inner transaction already committed.
			return held.Renew(activityCtx)
		})
		if !errors.Is(err, lease.ErrLeaseLost) {
			t.Fatal(err)
		}
		if n := count(t, `SELECT count(*) FROM integration_effects WHERE operation_id=$1`, operation); n != 1 {
			t.Fatal("inner effect did not commit", n)
		}
	})

	t.Run("jobs resource guard claim check and receipt compose", func(t *testing.T) {
		l := newLocker(t)
		submitted, err := submitter.Submit(ctx, jobs.SubmitInput{IdempotencyKey: unique(), Request: json.RawMessage(`{"account":"123"}`)})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := jobRepository.FindAvailableJobs(ctx, jobs.FindAvailableJobsParams{WorkerID: "node-A", LeaseSeconds: 30, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var a jobs.Job
		for _, job := range claimed {
			if job.ID == submitted.JobID {
				a = job
			}
		}
		if a.WorkerID == nil {
			t.Fatal("target job not claimed")
		}
		apply := func(job jobs.Job) error {
			return l.RunInFencedTx(ctx, "account:123", func(txCtx context.Context, held lease.Lease) error {
				// Validate the separate job claim and retain its row lock until this
				// transaction commits. This does not complete the job early.
				if _, err := jobRepository.Heartbeat(txCtx, jobs.HeartbeatParams{
					ID: job.ID, WorkerID: *job.WorkerID, FencingToken: job.FencingToken, LeaseSeconds: 30,
				}); err != nil {
					return err
				}
				// Unique logical-operation receipt makes replay after lost completion safe.
				_, err := unit.DBTx(txCtx).ExecContext(txCtx, `
     INSERT INTO integration_effects VALUES ($1, 'account:123', 'applied')
     ON CONFLICT (operation_id) DO NOTHING
    `, job.ID.String())
				return err
			})
		}
		if err := apply(a); err != nil {
			t.Fatal(err)
		}
		// Simulate completion-response loss by replaying before completion.
		if err := apply(a); err != nil {
			t.Fatal(err)
		}
		if n := count(t, `SELECT count(*) FROM integration_effects WHERE operation_id=$1`, a.ID.String()); n != 1 {
			t.Fatal(n)
		}
		// A separate successor takes the durable job claim, independently of resource ownership.
		if _, err := db.ExecContext(ctx, `UPDATE dbtx.jobs SET visible_at=clock_timestamp()-interval '1 second' WHERE id=$1`, a.ID); err != nil {
			t.Fatal(err)
		}
		claimed, err = jobRepository.FindAvailableJobs(ctx, jobs.FindAvailableJobsParams{WorkerID: "node-B", LeaseSeconds: 30, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var b jobs.Job
		for _, job := range claimed {
			if job.ID == a.ID {
				b = job
			}
		}
		if b.WorkerID == nil || b.FencingToken <= a.FencingToken {
			t.Fatal("no job takeover")
		}
		if err := apply(a); !errors.Is(err, jobs.ErrNoRows) {
			t.Fatal("stale job claim was accepted", err)
		}
		if err := apply(b); err != nil {
			t.Fatal(err)
		}
		if _, err := jobRepository.Complete(ctx, jobs.CompleteParams{ID: b.ID, WorkerID: *b.WorkerID, FencingToken: b.FencingToken, Response: json.RawMessage(`{"ok":true}`)}); err != nil {
			t.Fatal(err)
		}
		if n := count(t, `SELECT count(*) FROM integration_effects WHERE operation_id=$1`, a.ID.String()); n != 1 {
			t.Fatal(n)
		}
	})

	t.Run("inbox and outbox dequeue reject nested lease activities", func(t *testing.T) {
		l := newLocker(t)
		// Drain earlier committed messages. Their existing handlers own transactions.
		inObserved := false
		consumer.OnError = func(msg *inbox.Message, err error) {
			inObserved = errors.Is(err, lease.ErrTxInContext)
		}
		if err := consumer.Dequeue(ctx, 100, func(txCtx context.Context, msg *inbox.Message) error {
			return l.Activity(txCtx, "resource", func(context.Context, lease.Lease) error { t.Error("nested activity called"); return nil })
		}); err != nil {
			t.Fatal(err)
		}
		if !inObserved {
			t.Fatal("inbox handler rejection not recorded")
		}
		outObserved := false
		if err := publisher.Dequeue(ctx, 100, func(txCtx context.Context, msg *outbox.Message) error {
			err := l.RunInFencedTx(txCtx, "resource", func(context.Context, lease.Lease) error { t.Error("nested fenced callback called"); return nil })
			outObserved = errors.Is(err, lease.ErrTxInContext)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !outObserved {
			t.Fatal("outbox handler rejection not observed")
		}
	})
}
