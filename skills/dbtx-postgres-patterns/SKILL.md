---
name: dbtx-postgres-patterns
description: Integrate dbtx PostgreSQL modules into Go applications for transactional outbox, inbox, request idempotency, jobs, leases, advisory locks, schedules, caching (postgres/durable), or typed SQL helpers. Use when a consuming project needs one of these dbtx features.
---

# Integrate PostgreSQL patterns

Choose only the requested pattern. Inspect existing migrations, transaction boundaries, database driver, worker lifecycle, and dependency versions. These modules use database/sql; verify driver and Go compatibility before adding dependencies. Each postgres/<name> directory is a separate module under github.com/alextanhongpin/dbtx.

Use selected-version source, tests, and feature README together; the root README has older APIs. Paths below are relative to the dbtx checkout. When installed elsewhere, locate the selected module using go list -m -json or go mod download -json rather than assuming the checkout is present.

## postgres/durable

Outbox, inbox, request idempotency, jobs, leases, transaction locks, schedules and cache live in one module, github.com/alextanhongpin/dbtx/postgres/durable, with packages lease, txlock, keys, jobs, outbox, inbox, schedule and cache. Read postgres/durable/README.md, docs/learning-guide.md and example_test.go first; the module is experimental, so check the selected version's API. Its TODO.md lists known gaps against the removed standalone modules; check them against the requested feature.

Apply repository.Schema (or the numbered files in repository.Migrations) through the application's migration tooling. Everything lives in the PostgreSQL schema `durable`, the schema can be reapplied, and constructors never run DDL. Requires PostgreSQL 16+. After a failover or restore, run `SELECT durable.advance_fencing();` before workers resume. Do not edit generated repository/postgres files.

Every component takes the application's *dbtx.DB in its Config and joins the transaction in the context. Atomic participants must use the same database and matching dbtx context ID, resolving DBTx with the callback context. Test atomicity rather than assuming it.

- Jobs: jobs.NewClient(jobs.Config{DB, Kinds}) and Submit inside the business transaction; jobs.NewWorker plus Register per kind with KindOptions (ModeTx for database-only steps, ModeActivity for remote work with RunStep). Run the worker and a jobs.Janitor in the application lifecycle. Return durable.Permanent(err) to stop retrying and durable.RetryAfter(err, d) to honour a remote delay. Handlers must respect cancellation and tolerate at-least-once execution.
- Outbox: outbox.New with registered Topics; Publish requires a transaction by default. Run an outbox.NewRelay with a Publisher outside transactions. Delivery is at least once; for ordered topics pick a head policy (HeadBlock, HeadPark, HeadStop) and consume with inbox ModeSequenced for strict order.
- Inbox: inbox.New, then Receive(ctx, mode, msg, apply) with ModeDedup, ModeSequenced or ModeVersioned. apply's writes commit with the receipt. Use Enqueue to hand a message to a job. Test duplicate receipt, out-of-order delivery and apply rollback.
- Request idempotency: keys.New, then Do(ctx, req, fn) or Begin followed by Complete/Fail/Release on the claim. Preserve scope, key and request-body equality (ErrRequestMismatch). For multi-step work with checkpoints, submit a job instead.
- Leases: lease.New; Activity for long or external work with heartbeats, RunInFencedTx for a short guarded transaction. Resource writes from activities must carry the fencing token.
- Transaction locks: txlock.New(db).WithLock or WithLocks (sorted, so no deadlock), with Options for waiting or trying. Locks release when the transaction ends.
- Cache: cache.New, then cache.GetOrCompute for stampede protection, stale-while-revalidate and versioned writes. The cache table is UNLOGGED.
- Schedules: schedule.New and Run submit one job per tick across any number of schedulers.

Pass a durable.Observer (for example durable.SlogObserver{}) to surface lease losses, retries, blocked streams and inbox gaps.

## Utilities

- Typed SQL: inspect postgres/dbt/dbt.go and README. Pass context-selected DBTX to compiled SQL[P,R] queries. Preserve an existing query layer unless replacing it is requested.
- JSONB: inspect postgres/jsonb/jsonb.go and README. Check whether the selected API accepts a raw pool or a transaction-compatible interface before using it in an atomic operation.
- Constraint errors: postgres/violations currently recognizes *pq.Error. Verify actual driver error types before using its predicates, especially with pgx through database/sql.

## Verify

Apply migrations to an isolated database. Test business writes and enqueued records rolling back together, duplicates, retries, lease expiry, and cancellation for the selected feature. Use real independent connections for concurrency guarantees; durable tests need DURABLE_TEST_DSN. Run checks per changed module; root go test ./... excludes nested modules. Report migration application and runtime verification separately, including unavailable database checks.
