---
name: dbtx-postgres-patterns
description: Integrate dbtx PostgreSQL modules into Go applications for transactional outbox, inbox, durable idempotency, jobs, advisory locks, caching, or typed SQL helpers. Use when a consuming project needs one of these dbtx features.
---

# Integrate PostgreSQL patterns

Choose only the requested pattern. Inspect existing migrations, transaction boundaries, database driver, worker lifecycle, and dependency versions. These modules use database/sql; verify driver and Go compatibility before adding dependencies. Each postgres/<feature> is a separate module under github.com/alextanhongpin/dbtx.

Use selected-version source, tests, and feature README together; the root README has older APIs. Paths below are relative to the dbtx checkout. When installed elsewhere, locate the selected module using go list -m -json or go mod download -json rather than assuming the checkout is present.

## Database-backed services

Outbox, inbox, idempotent, cache, and jobs have concrete implementations at postgres/<feature>/repository/repository.go. Inspect constructors and embedded Schema from repository/schema.sql. Adapt the full schema, including types, functions, indexes, and constraints, to application migration tooling. Constructors and module installation do not run migrations. Do not edit generated repository/postgres files.

Atomic participants must use the same database and matching dbtx context ID, resolving DBTx with the callback context. Independent pools need distinct IDs configured before use. Matching IDs alone do not prove correct pool wiring; test atomicity.

### Outbox

Inspect postgres/outbox/outbox.go, README, schema, and tests. Current wiring uses the repository, not a bare dbtx unit of work:

```go
repo := outboxrepo.New(db) // import postgres/outbox/repository as outboxrepo
ob := outbox.New(repo)
ob.RequireTx = true
err := repo.RunInTx(ctx, func(txCtx context.Context) error {
    if err := writeBusinessData(txCtx); err != nil {
        return err
    }
    _, err := ob.Enqueue(txCtx, outbox.EnqueueParams{
        AggregateID: "order-123",
        AggregateType: "order",
        EventType: "order.created",
        Payload: payload,
    })
    return err
})
```

Here db is the application's *sql.DB, payload is encoding/json/jsontext.Value in this checkout, and writeBusinessData must join the same transaction. Check types against the selected version. RequireTx prevents accidental independent enqueue commits.

Run Dequeue(ctx, limit, handler) outside a repository transaction; otherwise it returns ErrTxInContext. The handler receives its acknowledgement transaction context. Treat ErrEOQ as an empty queue with the application's polling delay. Handler errors are recorded on messages rather than necessarily returned by Dequeue; monitor retries and dead letters.

Delivery is at least once and ordering is not guaranteed, even within an aggregate. External publishing may succeed before acknowledgement fails, so downstream consumers must tolerate duplicates. Old root examples using outbox.New(atomic), Create, or LoadAndDelete do not describe this API.

### Inbox

Read postgres/inbox/inbox.go, README, schema, and tests for CreateParams, source/message identity, duplicate handling, and Ordered aggregate processing. Verify constructors before wiring inbox.New(inboxrepo.New(db)). Dequeue starts outside a transaction; handler writes and acknowledgement share the callback transaction. Test duplicate receipt and handler rollback. Use Ordered only when required and backed by appropriate aggregate identifiers.

### Durable idempotency

Read postgres/idempotent/idempotent.go, README, and schema before wiring Do. Preserve request equality, fencing tokens, checkpoints, retry backoff, lease ownership, and TTL retention. Its entry point rejects transaction-bearing contexts; handler database work uses the supplied context. Inspect the actual handler/result types rather than guessing callback signatures. External effects still require downstream idempotency or fencing.

### Jobs

Read postgres/jobs/submit.go, worker.go, janitor.go, and schema. Wire submitter, worker, and cleanup into application lifecycle. Handlers must respect cancellation and tolerate at-least-once execution. Use job IDs for downstream idempotency or supported fencing tokens. Use package helpers for permanent errors and explicit retry delays. Verify graceful shutdown and lease recovery.

## Utilities

- Advisory locks: inspect postgres/lock/lock.go and key.go. TryLock(txCtx, key) returns (bool, error), takes no DBTX argument, and returns ErrLockOutsideTx outside a transaction. Use NamedTryLock/NamedLock for custom IDs. Use built-in string, int64, or supported Pair keys; inspect conversion behavior before passing named types. Locks release at transaction end. Test contention with independent connections.
- Cache: inspect postgres/cache/cache.go, func.go, README, and schema. Construct its repository before cache.New. Preserve prefix separators, typed encoding, and error handling. Zero TTL means no expiration; negative TTL is rejected. Inspect LoadOrCreate lease and transaction behavior before placing it in a business transaction. Cache idempotency has different retention/recovery semantics from the durable idempotent module.
- Typed SQL: inspect postgres/dbt/dbt.go and README. Pass context-selected DBTX to compiled SQL[P,R] queries. Preserve an existing query layer unless replacing it is requested.
- JSONB: inspect postgres/jsonb/jsonb.go and README. Check whether the selected API accepts a raw pool or a transaction-compatible interface before using it in an atomic operation.
- Constraint errors: postgres/violations currently recognizes *pq.Error. Verify actual driver error types before using its predicates, especially with pgx through database/sql.

## Verify

Apply migrations to an isolated database. Test business writes and enqueued records rolling back together, duplicates, retries, lease expiry, and cancellation for the selected feature. Use real independent connections for concurrency guarantees. Run checks per changed module; root go test ./... excludes nested modules. Report migration application and runtime verification separately, including unavailable database checks.
