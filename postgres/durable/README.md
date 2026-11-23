# postgres/durable

Status: **experimental.** Every package below is implemented and tested
against PostgreSQL, but the API may still change. It replaces the former
standalone `postgres/lease`, `idempotent`, `jobs`, `outbox`, `inbox`, `lock` and
`cache` modules, which were removed from this repository; their tagged versions
remain available from the Go module proxy. Known gaps against those modules are
listed in [TODO](TODO.md#parity-gaps-with-the-removed-modules).

One module for durable work on PostgreSQL, built on one shared lease engine,
one fencing-token sequence (resource leases in `lease` count tokens per
namespace and key instead) and one error vocabulary:

| Package | What it does |
| --- | --- |
| `lease` | Renewable fenced leases on a resource: `Activity` (long or external work, heartbeats), `RunInFencedTx` (short guarded transaction), `Try*` variants |
| `txlock` | Transaction-scoped advisory locks on string, int64 or pair keys: `WithLocks`/`WithKeys` (sorted to prevent deadlocks), `Lock`/`TryLock` inside an existing transaction; a timeout in a joined transaction leaves it usable |
| `keys` | Synchronous request idempotency (`Idempotency-Key` headers): `Do`, or `Begin` + `Complete`/`Fail`/`Abort`/`Release`; `MaxAttempts` with an exhausted state, backoff with `BackoffError` (retry-after), checkpointed multi-step requests with `DoSteps` |
| `jobs` | Durable executions: idempotent `Submit` inside your transaction, workers with `ModeTx` or `ModeActivity`, checkpoints, `RunStep` for idempotent remote calls, retries, cancel, compensation, drain on shutdown, admin and janitor |
| `outbox` | Transactional event publishing; unordered topics, or ordered topics with a gapless sequence per aggregate and head-of-line policies (`HeadBlock`, `HeadPark`, `HeadStop`) |
| `inbox` | Apply incoming messages once: `ModeDedup`, `ModeSequenced` (buffers out-of-order messages and drains in order), `ModeVersioned` (newer wins); `Enqueue` hands a message to a job |
| `schedule` | Cron and interval schedules that submit one job per tick, however many schedulers run |
| `cache` | Typed cache with stampede protection, stale-while-revalidate and versioned writes; key-value operations (`LoadOrStore`, `StoreOnce`, `CompareAndSwap`, `CompareAndDelete`, `LoadAndDelete`, `Expire`, `TTL`, `Exists`) and the `Func`/`Idempotent` memoizers |
| `durabletest` | Test helpers: isolated database per test, pause gates for fault injection, recording publisher |

Docs, in `docs/`: [learning guide](docs/learning-guide.md) (start here),
[design](docs/design.md), [decisions](docs/decisions.md),
[failure modes](docs/failure-modes.md), [scenarios](docs/scenarios.md).
Plan and follow-ups: [TODO](TODO.md).

## Requirements and setup

- Go 1.27+, `database/sql` with any PostgreSQL driver (tests use `lib/pq`).
- PostgreSQL 16+. On 17+, transaction timeouts also set `transaction_timeout`.
- One writable PostgreSQL authority. After promoting a replica or restoring a
  backup, run `SELECT durable.advance_fencing();` before workers resume, so no
  fencing token is ever reissued.

```bash
go get github.com/alextanhongpin/dbtx/postgres/durable
```

Apply [repository/schema.sql](repository/schema.sql) through your migrations.
It is exported as the string `repository.Schema`, and as
`repository.Migrations` (an `embed.FS` holding `migrations/0001_init.sql`) for
migration tools. Everything lives in the PostgreSQL schema `durable`, so it
never collides with the `dbtx.*` tables of the removed standalone modules.
Applying the schema twice is safe. Constructors never run DDL.

Every component takes the same `*dbtx.DB` your repositories use, so its writes
join the transaction in your context:

```go
import (
    "database/sql"

    _ "github.com/lib/pq"

    "github.com/alextanhongpin/dbtx"
    "github.com/alextanhongpin/dbtx/postgres/durable"            // errors, Permanent, RetryAfter, Observer
    "github.com/alextanhongpin/dbtx/postgres/durable/jobs"       // and cache, inbox, keys, lease, outbox, schedule, txlock
    "github.com/alextanhongpin/dbtx/postgres/durable/repository" // the schema
)

sqlDB, err := sql.Open("postgres", dsn)
_, err = sqlDB.ExecContext(ctx, repository.Schema) // or run repository.Migrations
atomic := dbtx.New(sqlDB)
// Writes join a transaction started with atomic.RunInTx(ctx, func(txCtx context.Context) error {...});
// your own queries use atomic.DBTx(txCtx).
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

## Usage

One snippet per package; `atomic` is the `*dbtx.DB` from setup and `ctx` a
`context.Context`. More scenarios are in [example_test.go](example_test.go).
Constructors return `(value, error)`; errors are omitted below where they are
only checked.

### jobs

```go
client, err := jobs.NewClient(jobs.Config{DB: atomic, Kinds: []string{"order.fulfil"}})

var res jobs.SubmitResult
err = atomic.RunInTx(ctx, func(txCtx context.Context) error {
    if _, err := atomic.DBTx(txCtx).ExecContext(txCtx, `INSERT INTO orders (id) VALUES ($1)`, 123); err != nil {
        return err
    }
    var err error
    res, err = client.Submit(txCtx, jobs.SubmitRequest{Kind: "order.fulfil", Key: "order:123", Payload: order})
    return err
})
// res.Created == false: the key was submitted before and res.ID is that execution.
// Same key with a different payload: durable.ErrRequestMismatch.

worker, err := jobs.NewWorker(jobs.WorkerConfig{DB: atomic})
err = worker.Register("order.fulfil", func(ctx context.Context, x *jobs.Execution) (jobs.Result, error) {
    var o Order
    if err := x.Decode(&o); err != nil { // the submitted payload
        return jobs.Result{}, durable.Permanent(err)
    }
    // RunStep saves build's request before the first call, then calls with the same
    // idempotency key (<execution id>:charge) on every attempt; once the response is
    // saved, later attempts return it without calling.
    charge, err := x.RunStep(ctx, "charge",
        func(ctx context.Context) (ChargeRequest, error) { return ChargeRequest{OrderID: o.ID}, nil },
        func(ctx context.Context, idempotencyKey string, req ChargeRequest) (Charge, error) {
            return payments.Charge(ctx, idempotencyKey, req)
        })
    if err != nil {
        return jobs.Result{}, err // retried with backoff; durable.Permanent(err) fails at once
    }
    return jobs.Done(charge), nil // stored as the output
}, jobs.KindOptions{Mode: jobs.ModeActivity})

runCtx, stop := context.WithCancel(ctx)
done := make(chan error, 1)
go func() { done <- worker.Run(runCtx) }()

job, err := client.Await(ctx, res.ID, 200*time.Millisecond) // polls until succeeded, failed or cancelled
// job.Status, job.Output, job.LastError, job.Attempts

// Shutdown: cancel Run's context. Run stops claiming, lets in-flight work finish
// for DrainTimeout (25s), hands unfinished activity work back without spending an
// attempt, and returns (ErrShutdownTimeout if a handler ignores cancellation).
stop()
err = <-done

// Cleanup: archive finished executions after a day; keys keep deduplicating for
// Config.KeyRetention (7 days) and are answered from the archive.
go (&jobs.Janitor{Client: client, Retention: 24 * time.Hour, Archive: true}).Run(ctx)
```

Retries wait for the backoff and are picked up at the next poll
(`WorkerConfig.PollInterval`, 1s). `ModeTx` runs each handler call inside a
transaction that also records the outcome (no `RunStep` there); return
`jobs.Next(name, data)` from either mode to checkpoint and be called again, and
use `x.Finalize(ctx, fn)` in activity mode to commit business writes with
completion. `client.Cancel`, `Get` and `GetByKey` inspect and stop executions.

### keys

```go
k, err := keys.New(keys.Config{DB: atomic}) // Retention 24h, MaxAttempts 10

http.HandleFunc("POST /payments", func(w http.ResponseWriter, r *http.Request) {
    body, _ := io.ReadAll(r.Body)
    resp, err := k.Do(r.Context(),
        keys.Request{Scope: "t1:payments.create", Key: r.Header.Get("Idempotency-Key"), Body: body},
        func(txCtx context.Context) (keys.Response, error) {
            // Writes through atomic.DBTx(txCtx) commit with the stored response.
            return keys.Response{Status: http.StatusCreated, Body: json.RawMessage(`{"id":"pay_1"}`)}, nil
        })
    switch {
    case errors.Is(err, durable.ErrRequestMismatch): // same key, different body
        http.Error(w, "idempotency key reused", http.StatusUnprocessableEntity)
    case errors.Is(err, durable.ErrInFlight): // a duplicate is running now
        http.Error(w, "request in progress", http.StatusConflict)
    case err != nil:
        http.Error(w, err.Error(), http.StatusInternalServerError)
    default: // first run or replay
        w.WriteHeader(resp.Status)
        w.Write(resp.Body)
    }
})
```

`Request.Body` and `Response.Body` must be JSON. They are stored as `jsonb`, so
bodies are compared ignoring key order and whitespace, and a replayed body is
equal as JSON but not byte for byte. `Do` returns fn's error after a failed
attempt, `*keys.BackoffError` (`keys.ErrBackoff`) while a retry waits, and
`keys.ErrExhausted` after `MaxAttempts`. `Begin` gives the same steps by hand,
and `DoSteps` checkpoints multi-step requests.

### cache

```go
c, err := cache.New(cache.Config{DB: atomic, Prefix: "profiles:"})

// T is inferred from fn. Twenty concurrent misses run fn once.
p, err := c.GetOrCompute(ctx, "user:1", cache.Options{TTL: time.Minute, StaleFor: 10 * time.Minute},
    func(ctx context.Context) (Profile, error) { return loadProfile(ctx, 1) })

err = atomic.RunInTx(ctx, func(txCtx context.Context) error {
    // ... update the user ...
    return c.InvalidateTx(txCtx, "user:1") // removed only if the transaction commits
})
```

Within `StaleFor` after `TTL`, the caller that takes the compute lease
recomputes (synchronously, in its own call) while concurrent callers get the
stale value at once. Without a stale value they wait (`OnContention`,
`MaxWait`). `Get`, `Set`, `Delete` and the key-value operations take a
`ttl, staleFor` pair; `cache.Func` and `cache.Idempotent` wrap a function.

### outbox

```go
ob, err := outbox.New(outbox.Config{DB: atomic, Topics: map[string]outbox.TopicConfig{
    "order.events": {Ordered: true, OnHeadFailure: outbox.HeadPark},
    "notify":       {MaxAttempts: 3},
}})

// Publish requires a transaction (unless Config.AllowNoTx), so the event commits
// with the business change. Ordered topics need an AggregateKey; each aggregate
// gets a gapless Seq starting at 1.
err = atomic.RunInTx(ctx, func(txCtx context.Context) error {
    _, err := ob.Publish(txCtx, outbox.Event{Topic: "order.events", Type: "order.paid", AggregateKey: "order:123", Payload: evt})
    return err
})

// One relay serves every topic of ob; run as many relays as you like.
relay, err := outbox.NewRelay(ob, outbox.RelayConfig{
    Publisher: outbox.PublisherFunc(func(ctx context.Context, msgs []outbox.Message) (int, error) {
        // msgs share one topic. Return how many, from the start of msgs, the
        // broker accepted; with an error the rest are retried.
        return len(msgs), nil
    }),
    // Or Handler: func(ctx context.Context, m outbox.Message) error {...} to consume
    // in-process; its writes through ctx commit with the acknowledgement.
})
go relay.Run(ctx)
```

A failing event is retried with `TopicConfig.Backoff` (default 1s doubling to
1h) until `MaxAttempts`, then dead-lettered (`DeadLetters`, `Requeue`,
`Count`). On ordered topics the head-of-line policy decides what a failing
head does.

### inbox

```go
ib, err := inbox.New(inbox.Config{DB: atomic})

// For each delivery (m is the outbox.Message the broker carried):
res, err := ib.Receive(ctx, inbox.ModeSequenced, inbox.Message{
    Source: m.Topic, MessageID: m.ID, AggregateKey: m.AggregateKey, Seq: m.Seq,
    Type: m.Type, Payload: m.Payload, IsGap: m.IsGap,
}, func(txCtx context.Context, msg inbox.Message) error {
    // Apply through atomic.DBTx(txCtx); it commits with the inbox record.
    return nil
})
// err == nil: acknowledge the broker. res is inbox.Applied, Duplicate, Buffered
// (an earlier Seq is missing; applied by a later drain) or GapSkipped.
```

The mode is chosen per call: `ModeDedup` (on `Source` + `MessageID`),
`ModeSequenced` (per `Source` + `AggregateKey`, `Seq` from 1) or
`ModeVersioned` (`Version`, newer wins). `Enqueue` stores a message and submits
a job for it in one transaction (needs `Config.Jobs`).

### lease

```go
locker, err := lease.New(lease.Config{DB: atomic, Namespace: "reports"}) // Namespace is required

err = locker.TryActivity(ctx, "report-generator", func(ctx context.Context, l lease.Lease) error {
    // Heartbeats every TTL/3 while fn runs; ctx is cancelled if the lease is lost.
    // ... long work ...
    return l.RunInFencedTx(ctx, func(txCtx context.Context) error {
        // Admitted only while this lease is valid; successors wait for it to commit.
        // Store l.Token() with resource writes that happen outside this transaction.
        return nil
    })
})
// errors.Is(err, durable.ErrContended): someone else holds it. Activity waits instead.
```

`Acquire`/`TryAcquire` return a `lease.Lease` that does not renew on its own
(call `Renew`, then `Release`); a stale lease gets `durable.ErrLeaseLost` from
`RunInFencedTx`. `Locker.RunInFencedTx` is a short guarded transaction without
a long activity. Tokens increase per namespace and key.

### txlock

```go
locks := txlock.New(atomic)
// Keys are sorted and de-duplicated, so callers locking overlapping sets in
// any order cannot deadlock. Inside an existing transaction the locks join it.
err = locks.WithLocks(ctx, []string{"account:a", "account:b"}, txlock.Options{Wait: 5 * time.Second},
    func(txCtx context.Context) error { return nil })
```

`TryWithLocks` returns `durable.ErrContended` instead of waiting; `Lock` and
`TryLock` lock inside a transaction you already hold; `WithKeys` takes
`txlock.StringKey`, `Int64Key` or `PairKey` keys.

### schedule

```go
sched, err := schedule.New(schedule.Config{DB: atomic, Jobs: client}) // Kind must be one of client's Kinds
err = sched.Register(ctx, schedule.Schedule{
    Name: "nightly-report", Spec: "0 2 * * *", TimeZone: "Asia/Singapore", Kind: "report.build",
    Missed: schedule.MissedLatest, Overlap: schedule.OverlapSkip,
})
go sched.Run(ctx) // any number of schedulers; each tick becomes one job
err = sched.Pause(ctx, "nightly-report") // Resume, Delete
```

`Spec` is a five-field cron expression, `@hourly`/`@daily`/`@weekly`/`@monthly`/`@yearly`,
or `@every <duration>` (for example `@every 15m`).

### Errors

Sentinels live in package `durable` (`lease` re-exports its own); match them
with `errors.Is`.

| Error | Meaning |
| --- | --- |
| `ErrRequestMismatch` | Key reused with a different body or payload |
| `ErrInFlight` | `keys`: a request with this key is running |
| `ErrContended` | `Try*` calls: another owner holds the lease or lock |
| `ErrLeaseLost` | The lease expired or was taken over; the write was rejected |
| `ErrLeaseBusy` | Another lifecycle call on the same lease is in progress |
| `ErrStaleToken` | A fenced resource write carried an older token |
| `ErrNotInTx` | A call that needs a transaction in ctx got none |
| `ErrUnknownKind`, `ErrUnknownTopic` | Kind or topic not registered |
| `ErrNotFound`, `ErrInvalidArgument`, `ErrInvalidConfig`, `ErrCancelled` | As named |

Handler errors: return `durable.Permanent(err)` to stop retrying, or
`durable.RetryAfter(err, d)` to honour a remote `Retry-After`. Everything else
is retried with exponential backoff and jitter until `MaxAttempts`.

## Defaults

| Setting | Default |
| --- | --- |
| Lease / job lease / relay lease | 30s; heartbeat every TTL/3; local deadline at request start + TTL − TTL/6 |
| Job attempts | 10, backoff 1s doubling to 1h with up to 25% jitter; workers poll every 1s |
| Job drain on shutdown | 25s, then unfinished activity-mode work is handed back without spending an attempt |
| Idempotency key retention | 24h for `keys`, 7 days for job keys |
| `keys` attempts | 10 per key (a crashed holder's attempt counts; `Release` gives one back), no backoff unless `Backoff` is set or the error carries `durable.RetryAfter` |
| Outbox | 10 attempts per event or stream head, backoff 1s doubling to 1h, batch 100 per topic, `HeadBlock`, advance per event; publish 10s, outcome writes bounded by `RecordTimeout` (default `RequestTimeout`, 5s) |
| Scheduler | missed runs `MissedLatest`, overlap `OverlapSkip` |
| Cache | compute lease 30s, renewed every 10s while `fn` runs; waiting callers poll every 50ms until the value appears or the lease ends (`MaxWait` bounds the wait) |

Unregistered job kinds and outbox topics are rejected, so a typo cannot create
work nobody processes.

## Operations

- `jobs.Client`: `ListFailed`, `Requeue`, `RequeueFailed`, `OldestPayloadVersions`,
  `Purge`, `Archive`, `PurgeArchive`, `PurgeKeys`; run a `jobs.Janitor` for
  periodic cleanup. `Purge` keeps an execution until its idempotency key
  (`Config.KeyRetention`) expires; `Janitor{Archive: true}` instead moves
  finished executions to `durable.executions_archive` after `Retention`, and
  resubmissions, `Get` and `GetByKey` are answered from the archive.
- `jobs.Config` limits input: `MaxPayloadBytes` (1 MiB) and `MaxKeyLength`
  (255 bytes, key and scope); negative disables a limit.
- `jobs.WorkerConfig.ReapGrace` (default `max(LeaseDuration, FinalizeTimeout)`): a final attempt's
  expired lease is left alone this long before the reaper fails it, so a slow
  owner nobody reclaimed can still record success.
- `outbox.Outbox`: `Stream`, `Resume`, `Skip` (gap marker), `Replay`,
  `PurgePublished`, `DeadLetters`, `Requeue`, `Count` (pending, running and
  dead events per topic).
- `outbox.Relay` publishes to a broker through a `Publisher`, or consumes
  in-process through a `Handler` whose writes commit in the same transaction
  as the acknowledgement. A panicking publisher or handler counts as a failed
  attempt; the relay keeps running.
- `inbox.Inbox`: `Gaps`, `SkipGap`, `Drain`, `Parked`, `Unpark`, `Discard`,
  `Job`, `PurgeMessages`. A buffered sequenced message that fails to apply
  during a drain has its attempts and last error stored, emits
  `EventStreamBlocked`, and after `Config.MaxAttempts` (default 10) is parked
  (`EventStreamParked`); drains then stop at it until `Unpark` (retry) or
  `Discard` (skip). `Drain`/`SkipGap`/`Unpark`/`Discard` return an
  `*inbox.DrainError` (match `inbox.ErrParked`). Dedup messages are not
  retried by the inbox: `Receive` leaves retries to the broker, and `Enqueue`
  hands them to jobs, whose `ListFailed`/`Requeue` are the dead-letter tools
  (`Inbox.Job` finds a message's job).
- `keys.Keys.Purge(ctx, limit)` and `cache.Cache.Purge(ctx, limit)` delete up to
  `limit` expired rows per call and return how many; call them periodically.
- Pass a `durable.Observer` (for example `durable.SlogObserver{}`) to see lease
  losses, retries, failures, blocked streams and inbox gaps.

## Queries

All SQL lives in [repository/queries](repository/queries) and is generated
with sqlc v1.31.1 into `repository/postgres` (checked in; do not edit). After
changing `schema.sql` or a query, regenerate and copy `schema.sql` over
`migrations/0001_init.sql`; until the first release there is a single
migration:

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
[failure modes](docs/failure-modes.md#test-coverage).

## Limits

- No exactly-once external effects, and no atomicity across databases.
- The shared lease functions build their SQL dynamically (`EXECUTE format`),
  so a missing column surfaces when the function runs; the contract test and
  `durable.lease_contract` catch that early.
- `Await` polls; there is no `LISTEN/NOTIFY` wakeup yet.
- The cache table is UNLOGGED: it is emptied by a crash and not replicated.
