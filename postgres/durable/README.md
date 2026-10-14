# postgres/durable

Status: **experimental.** Every package below is implemented and tested
against PostgreSQL, but the API may still change. The existing `postgres/lease`,
`idempotent`, `jobs`, `outbox`, `inbox`, `lock` and `cache` modules are
unaffected and remain the stable options.

One module for durable work on PostgreSQL, built on one shared lease engine,
one fencing-token sequence and one error vocabulary:

| Package | What it does |
| --- | --- |
| `lease` | Renewable fenced leases on a resource: `Activity` (long or external work, heartbeats), `RunInFencedTx` (short guarded transaction), `Try*` variants |
| `txlock` | Transaction-scoped advisory locks; `WithLocks` sorts keys to prevent deadlocks |
| `keys` | Synchronous request idempotency (`Idempotency-Key` headers): `Do`, or `Begin` + `Complete`/`Fail`/`Release` |
| `jobs` | Durable executions: idempotent `Submit` inside your transaction, workers with `ModeTx` or `ModeActivity`, checkpoints, `RunStep` for idempotent remote calls, retries, cancel, compensation, drain on shutdown, admin and janitor |
| `outbox` | Transactional event publishing; unordered topics, or ordered topics with a gapless sequence per aggregate and head-of-line policies (`HeadBlock`, `HeadPark`, `HeadStop`) |
| `inbox` | Apply incoming messages once: `ModeDedup`, `ModeSequenced` (buffers out-of-order messages and drains in order), `ModeVersioned` (newer wins); `Enqueue` hands a message to a job |
| `schedule` | Cron and interval schedules that submit one job per tick, however many schedulers run |
| `cache` | Typed cache with stampede protection, stale-while-revalidate and versioned writes |
| `durabletest` | Test helpers: isolated database per test, pause gates for fault injection, recording publisher |

Design and rationale: [design](../../docs/durable-design.md),
[decisions](../../docs/durable-decisions.md),
[failure modes](../../docs/durable-failure-modes.md),
[scenarios](../../docs/durable-scenarios.md), [TODO](TODO.md).

## Requirements and setup

- Go 1.27+, `database/sql` with any PostgreSQL driver (tests use `lib/pq`).
- PostgreSQL 16+. On 17+, transaction timeouts also set `transaction_timeout`.
- One writable PostgreSQL authority. After promoting a replica or restoring a
  backup, run `SELECT durable.advance_fencing();` before workers resume, so no
  fencing token is ever reissued.

```bash
go get github.com/alextanhongpin/dbtx/postgres/durable
```

Apply [repository/schema.sql](repository/schema.sql) (exported as
`repository.Schema`, and as numbered files in `repository.Migrations`) through
your migrations. Everything lives in the PostgreSQL schema `durable`, so it
never collides with the existing `dbtx.*` tables. Applying the schema twice is
safe. Constructors never run DDL.

Every component takes the same `*dbtx.DB` your repositories use, so its writes
join the transaction in your context:

```go
atomic := dbtx.New(sqlDB)
client, err := jobs.NewClient(jobs.Config{DB: atomic, Kinds: []string{"order.fulfil"}})
```

## Guarantees

| Scope | Guarantee |
| --- | --- |
| Acceptance | A request or submission with the same scope and key is accepted once within the retention window; a different body returns `ErrRequestMismatch` |
| Local effects | Effectively once when they commit with the outcome: `ModeTx` steps, `Execution.Finalize`, `keys.Do`, inbox `Receive`, `lease.RunInFencedTx` |
| Remote effects | At least once; once in effect when the remote API honours the `RunStep` idempotency key within its retention window |
| Outbox delivery | At least once. Ordered topics are in order on the relay side best-effort; consume them with `inbox.ModeSequenced` for strict order without duplicates |
| Leases | Never two holders inside a fenced transaction; activities can overlap after expiry, so resource writes must carry the fencing token |

Each feature keeps its own table, but every ownership change (grant, renew,
lock, finish, expire) goes through five shared PostgreSQL functions,
`durable.lease_*`, which work on any table with the lease columns (`id`,
`status`, `owner_id`, `token`, `lease_expires_at`, `updated_at`, plus `run_at`,
`attempts`, `max_attempts`, `last_error` when used). Writes after a claim check
owner, token and an unexpired lease; inside a held row lock, owner and token
are enough. A worker that was paused past its lease cannot complete,
checkpoint, advance a stream or store a response. `durable.lease_contract(table)`
lists any lease columns a table is missing.

## Quick reference

Examples for each package are in [example_test.go](example_test.go); they
compile with the package. Highlights:

```go
// Jobs: submit with your writes; handle with an idempotent remote step.
err := atomic.RunInTx(ctx, func(txCtx context.Context) error {
    // ... business writes through atomic.DBTx(txCtx) ...
    _, err := client.Submit(txCtx, jobs.SubmitRequest{Kind: "order.fulfil", Key: "order:123", Payload: order})
    return err
})

worker, _ := jobs.NewWorker(jobs.WorkerConfig{DB: atomic})
worker.Register("order.fulfil", func(ctx context.Context, x *jobs.Execution) (jobs.Result, error) {
    charge, err := x.RunStep(ctx, "charge", buildChargeRequest, payments.Charge)
    if err != nil {
        return jobs.Result{}, err // retried; return durable.Permanent(err) to stop
    }
    return jobs.Done(charge), nil
}, jobs.KindOptions{Mode: jobs.ModeActivity})
go worker.Run(ctx)

// Outbox: an ordered topic, published with the business change.
ob, _ := outbox.New(outbox.Config{DB: atomic, Topics: map[string]outbox.TopicConfig{
    "order.events": {Ordered: true},
}})
_, err = ob.Publish(txCtx, outbox.Event{Topic: "order.events", Type: "order.paid", AggregateKey: "order:123", Payload: evt})
```

Handler errors: return `durable.Permanent(err)` to stop retrying, or
`durable.RetryAfter(err, d)` to honour a remote `Retry-After`. Everything else
is retried with exponential backoff and jitter until `MaxAttempts`.

## Defaults

| Setting | Default |
| --- | --- |
| Lease / job lease / relay lease | 30s; heartbeat every TTL/3; local deadline at request start + TTL − TTL/6 |
| Job attempts | 10, backoff 1s doubling to 1h with up to 25% jitter |
| Job drain on shutdown | 25s, then unfinished activity-mode work is handed back without spending an attempt |
| Idempotency key retention | 24h for `keys`, 7 days for job keys |
| Outbox | 10 attempts per event or stream head, batch 100, `HeadBlock`, advance per event |
| Scheduler | missed runs `MissedLatest`, overlap `OverlapSkip` |
| Cache | compute lease 30s; waiting callers poll every 50ms, then compute themselves |

Unregistered job kinds and outbox topics are rejected, so a typo cannot create
work nobody processes.

## Operations

- `jobs.Client`: `ListFailed`, `Requeue`, `RequeueFailed`, `OldestPayloadVersions`,
  `Purge`, `PurgeKeys`; run a `jobs.Janitor` for periodic cleanup.
- `outbox.Outbox`: `Stream`, `Resume`, `Skip` (gap marker), `Replay`,
  `PurgePublished`, `DeadLetters`, `Requeue`.
- `inbox.Inbox`: `Gaps`, `SkipGap`, `Drain`, `PurgeMessages`.
- `keys.Keys.Purge`, `cache.Cache.Purge`.
- Pass a `durable.Observer` (for example `durable.SlogObserver{}`) to see lease
  losses, retries, failures, blocked streams and inbox gaps.

## Queries

All SQL lives in [repository/queries](repository/queries) and is generated
with sqlc v1.31.1 into `repository/postgres` (checked in; do not edit). After
changing `schema.sql` or a query, regenerate and copy the schema into a new
migration file:

```bash
go tool sqlc generate -f postgres/durable/repository/sqlc.yaml   # from the repository root
# or: make sqlc
```

Packages build queries per operation with `postgres.New(db.DBTx(ctx))`, so
they join the caller's `dbtx` transaction. In the database, a NULL lease or
expiry time means "no lease"; nullable `jsonb` values scan through `pg.JSON`.

## Testing

Tests need a PostgreSQL server where the user may create databases; each test
gets its own database. Without `DURABLE_TEST_DSN` they are skipped.

```bash
docker compose up -d db   # from the repository root
cd postgres/durable
DURABLE_TEST_DSN='postgres://john:123456@127.0.0.1:5432/dev?sslmode=disable' \
    go test -race -count=1 ./...
go vet ./...
```

`durabletest.Gate` blocks a hook to simulate a stop-the-world pause at an
exact point; `durable.Hooks` names the points (before heartbeat, before
complete, after a remote call, after publish before advance, between steps).
The failure-mode tests use them; see
[failure modes](../../docs/durable-failure-modes.md#test-coverage).

## Limits

- No exactly-once external effects, and no atomicity across databases.
- The shared lease functions build their SQL dynamically (`EXECUTE format`),
  so a missing column surfaces when the function runs; the contract test and
  `durable.lease_contract` catch that early.
- `Await` polls; there is no `LISTEN/NOTIFY` wakeup yet.
- The cache table is UNLOGGED: it is emptied by a crash and not replicated.
