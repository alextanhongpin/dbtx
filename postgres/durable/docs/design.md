# PostgreSQL durable execution module design

Status: implemented (experimental). See the
[module README](../README.md) for usage, and
[TODO](../TODO.md) for what is done and what remains. Where the
implementation differs from the original plan, the
[decisions](decisions.md) record why (D12–D22). The [learning guide](learning-guide.md) explains the
same material from first principles.

Related: [decisions](decisions.md),
[failure modes](failure-modes.md), [usage scenarios](scenarios.md),
[lease plan](../../../docs/distributed-lock-plan.md), and [lease reuse limits](../../../docs/postgres-lease-module-reuse.md).

## Purpose

`postgres/durable` is a single Go module that provides, on one shared engine:

- transaction-scoped locks and renewable fenced resource leases;
- request idempotency (synchronous, replayable responses);
- durable executions (jobs) with retries, checkpoints and idempotent remote steps;
- a transactional outbox with optional per-aggregate ordering;
- an inbox with deduplication, sequence-ordered and version-ordered modes;
- a scheduler, and a cache with stampede protection;
- shared observability, fault-injection hooks and a test harness.

It is a fresh implementation, not a wrapper. The existing `postgres/lease`,
`idempotent`, `jobs`, `outbox`, `inbox`, `lock` and `cache` modules stay as they
are; see [Coexistence](#coexistence-with-existing-modules).

### Why one module instead of reusing the siblings

The [lease reuse document](../../../docs/postgres-lease-module-reuse.md) found that separate
nested modules cannot share an `internal` package, that token domains differ
(per-key generation, per-job counter, global sequence), and that expiry policies
differ (jobs may complete after expiry; leases may not). One module lets every
feature share `internal/engine` with one token domain, one expiry policy, one
error vocabulary and one test harness. The cost is duplicating behaviour that
already works elsewhere; the existing modules' tests are the reference for what
"works" means.

### Non-goals

- Exactly-once external effects. External calls get at-least-once execution plus
  idempotency keys; only effects inside the same PostgreSQL database commit
  atomically with state changes.
- Multi-database atomicity, or coordination across independent PostgreSQL
  authorities.
- A broker. The outbox relay publishes to one; the inbox receives from one.
- A workflow DSL. Executions are handlers with checkpoints and steps, not a graph
  engine.
- Replacing a high-throughput cache such as Redis.

## Platform and conventions

| Topic | Choice |
| --- | --- |
| Module | `github.com/alextanhongpin/dbtx/postgres/durable`, Go 1.27+ |
| Database API | `database/sql` through root `dbtx` (`RunInTx`, `DBTx(ctx)`, context transaction IDs) |
| Driver | Any `database/sql` driver; tests use `lib/pq`. The library itself only depends on `database/sql` (D3). |
| PostgreSQL | 16+; `transaction_timeout` is set on 17+. IDs are UUIDv7 generated in Go |
| Schema | Dedicated PostgreSQL schema `durable`, so tables never collide with the existing `dbtx.*` tables |
| Queries | Generated with sqlc v1.31.1 from `repository/queries/*.sql` into `repository/postgres` (D12); `repository/schema.sql` exported as `Schema` |
| Status columns | `text` with `CHECK` constraints instead of enum types, so the schema can be reapplied (the existing modules cannot) |
| Installation | Constructors never run DDL. Versioned migration files are embedded and exported for the application's migration tool. |
| Clocks | PostgreSQL decides expiry (`clock_timestamp()` after taking the row lock). Go's monotonic clock bounds local deadlines. |

SQLSTATE codes are read through a small interface, so the library imports no
driver. See [decisions](decisions.md#d3-driver).

## Package layout

```text
postgres/durable/               package durable: errors, Permanent/RetryAfter/Backoff,
                                Observer, Hooks (no central wiring type, D4)
  lease/                        resource leases: Activity, RunInFencedTx, Try* variants
  txlock/                       transaction-scoped advisory locks, sorted multi-key
  keys/                         synchronous request idempotency
  jobs/                         client (submit, get, cancel, await, admin), worker,
                                execution (checkpoints, Finalize, RunStep), janitor
  outbox/                       producer API, relay, stream admin
  inbox/                        receive modes, pending buffer, store-and-forward
  schedule/                     cron/interval parser and scheduler feeding jobs
  cache/                        typed cache, compute leases, versioned writes
  durabletest/                  per-test database, pause gate, recording publisher
  internal/engine/              lease keeper: heartbeat, watchdog, supervision
  internal/pg/                  SQL helpers: SQLSTATE, timeouts, IDs, nullable JSON
  repository/
    schema.sql                  full schema, incl. durable.lease_* (exported as Schema)
    migrations/                 0001_init.sql (embed.FS)
    queries/                    sqlc input, one file per feature
    postgres/                   sqlc output (generated, do not edit)
  docs/                         design, decisions, failure modes, scenarios, learning guide
```

Feature packages depend on `internal/engine` and the generated queries; they do
not depend on each other except through documented composition points (for
example the inbox's store-and-forward mode submits a job).

## Building blocks

Every feature is assembled from the same eight primitives, implemented once in
the shared `durable.lease_*` SQL functions, `internal/engine` and the root
package (retry policy):

| # | Primitive | Behaviour |
| --- | --- | --- |
| P1 | Lease | Owner ID, fencing token and expiry on a row; heartbeat every TTL/3; local watchdog at request start + TTL − TTL/6; loss cancels the work context with a cause |
| P2 | Fenced write | Every mutation after a claim goes through `durable.lease_lock` or `lease_finish`: owner, token and an unexpired lease; owner and token suffice while the writer holds the row lock (D7, D20) |
| P3 | Dedup key | `INSERT … ON CONFLICT DO NOTHING`, then read; request equality compared as `jsonb`; stored outcome replayed |
| P4 | Retry state machine | `status`, `attempts` (counted at claim), `max_attempts`, `run_at`, `last_error`; backoff with jitter; error classification |
| P5 | Claim next | The feature selects candidates with `FOR UPDATE SKIP LOCKED`; `durable.lease_grant` takes them in the same statement |
| P6 | Steps | Named checkpoints and remote steps with saved request, stable remote key and saved response |
| P7 | Wakeup | Polling; `LISTEN/NOTIFY` with a polling fallback is a follow-up (Q10) |
| P8 | Retention | Batched purge per table with per-feature windows |

### Lease column contract

Every table whose rows are claimed (executions, outbox events, outbox streams,
in-flight idempotency keys) has these fixed column names, so claim, heartbeat,
finish and expiry are written once as PostgreSQL functions (D20):

```text
id                uuid
status            text         -- feature-specific set, always including the terminal states
owner_id          text         -- random per claim; NULL when nobody owns the row
token             bigint       -- fencing token; see "Fencing tokens"
lease_expires_at  timestamptz  -- NULL means no lease (D21)
updated_at        timestamptz
run_at            timestamptz  -- when used
attempts          int          -- when used; incremented at claim, so crashes consume attempts
max_attempts      int          -- when used
last_error        text         -- when used
```

| Function | Does |
| --- | --- |
| `durable.lease_grant(table, ids, owner, ttl, ready, running, count_attempt)` | Takes candidate rows, gives each a new token, optionally counts an attempt |
| `durable.lease_renew(...)` | Heartbeat: owner, token and an unexpired lease |
| `durable.lease_lock(...)` | Locks the row and checks ownership before the feature writes its own columns in the same transaction |
| `durable.lease_finish(...)` | Ends the lease with a new status; optional `run_at` delay, error and attempt adjustment |
| `durable.lease_expire(...)` | Reaper: takes over rows whose lease expired |

Features choose candidates and write their own columns in sqlc queries; the
functions decide ownership. `durable.lease_contract(table)` lists missing
columns. In Go, `internal/engine.Row` wraps the calls, and the
`internal/engine` keeper owns heartbeats, the local deadline and loss
handling. Resource leases and cache compute leases keep their own functions
because their token domains differ (D5).

### Fencing tokens

| Owner | Token source | Reason |
| --- | --- | --- |
| Resource leases | Per-key generation on a row that is never purged | Matches `postgres/lease`; tokens order owners of one resource |
| Executions, idempotency keys, outbox and inbox streams | Global sequence `durable.fencing_seq` | Rows are purged; a sequence never repeats even after a key is reused (as in `postgres/idempotent`) |

After a failover or restore, `durable.advance_fencing()` moves both domains
past anything that may have been issued (see
[D6](decisions.md#d6-fencing-after-failover-or-restore)). Resource tables written
under a lease store the token and accept `token >= stored` for repeat writes by
the same owner and reject anything older.

### Expiry policy

Strict, everywhere: heartbeat, checkpoint, completion and failure all require a
matching owner and token **and** an unexpired lease, evaluated with the database
clock after taking the row lock. This is the `postgres/lease` policy. The laxer
`postgres/jobs` behaviour (completing after expiry when nobody reclaimed the
job) is intentionally not carried over; the work is retried instead.

## Execution modes

Every executable unit (job, idempotent request, inbox message processed later,
outbox stream) runs in one of two modes, chosen per kind:

| Mode | How work runs | Completion | Use for |
| --- | --- | --- | --- |
| `Tx` | Each step runs in a short transaction that locks the row (fenced), runs the handler with the transaction in `ctx`, and writes a checkpoint or outcome before commit | Atomic with business writes | Database-only work; the pattern proven by `postgres/idempotent` and `inbox` |
| `Activity` | Handler runs outside any transaction; lease heartbeats; work context is cancelled on loss | `exec.Finalize(ctx, fn)` runs `fn` and the completion in one fenced transaction; plain return completes separately | External calls, long computation, remote steps |

In `Tx` mode a held row lock prevents takeover until commit, so no heartbeat runs
during a step, and the step holds a connection for its duration. Keep steps short
and split work with checkpoints. Remote steps (`RunStep`) are only allowed in
`Activity` mode; calling them in `Tx` mode returns `ErrRemoteStepInTx`.

## Tables

All tables live in schema `durable`. Rows marked *lease* carry the lease column contract above.

| Table | Key | Purpose |
| --- | --- | --- |
| `leases` | `(namespace, key)` | Resource leases; generation retained forever |
| `fencing_seq` | sequence | Global fencing tokens; `durable.advance_fencing()` moves it past a failover (D6) |
| `idempotency_keys` | `(scope, key)` | Request JSON, status, short lease for in-flight synchronous work, stored response envelope, optional `execution_id`, `expires_at`, *lease* |
| `executions` | `id` (uuidv7) | Kind, queue, payload, payload version, mode, output, cancel flag, *lease* |
| `execution_steps` | `(execution_id, name)` | Checkpoint data or remote step request/response/remote key, status |
| `execution_attempts` | `(execution_id, attempt)` | Optional audit: owner, token, start, finish, error |
| `outbox_events` | `id`; unique `(topic, aggregate_key, seq)` | Topic, type, headers, payload, `published_at`; *lease* used only by unordered events |
| `outbox_heads` | `(topic, aggregate_key)` | Producer-side `head_seq` counter (row lock serializes producers) |
| `outbox_streams` | `(topic, aggregate_key)` | Relay-side `last_published_seq`, policy state, *lease* |
| `inbox_messages` | `(source, message_id)` | Dedup record, optional aggregate key, seq or version, status, and the `execution_id` of a store-and-forward job |
| `inbox_streams` | `(source, aggregate_key)` | `last_applied_seq` or `last_version`; never purged while the stream lives |
| `inbox_pending` | `(source, aggregate_key, seq)` | Out-of-order messages waiting for a gap to fill |
| `schedules` | `name` | Spec, time zone, kind, input, next tick, missed-run and overlap policies, paused flag |
| `cache` (UNLOGGED) | `key` | Value, version, `expires_at`, `stale_until`, compute owner/token/lease |

## Feature designs

### Locks and leases (`txlock`, `lease`)

- `txlock.WithLock(ctx, key, fn)` and `txlock.WithLocks(ctx, keys, fn)` run `fn`
  in a `dbtx` transaction holding `pg_advisory_xact_lock` for each key, sorted and
  de-duplicated to prevent deadlocks; `Try` variants return `ErrContended`;
  `SET LOCAL lock_timeout` bounds waiting.
- `lease` reproduces the `postgres/lease` contract (Acquire/TryAcquire,
  Activity/TryActivity, RunInFencedTx/TryRunInFencedTx, `held.RunInFencedTx`,
  `TxOutcomeError`) on the shared engine. Its README and tests are the
  specification; deviations need a decision entry.
- `lease.CheckFenced(sql.Result)` turns "0 rows affected" on a fenced resource
  write into `ErrStaleToken`, and the docs define the resource-side token column
  convention.

### Request idempotency (`keys`)

Synchronous API for HTTP or RPC handlers:

```text
claim, err := k.Begin(ctx, keys.Request{Scope, Key, Body})   // InFlightTTL, Retention in keys.Config
  → ErrRequestMismatch | claim.State ∈ {Acquired, InFlight, Completed}
Completed → replay claim.Response (status, headers, body)
InFlight  → caller returns 409 with Retry-After
Acquired  → do work; then, inside the business transaction:
            claim.Complete(txCtx, Response)       fenced by the claim's token
            or claim.Fail(ctx, Response)          store a permanent failure (replayed)
            or claim.Release(ctx)                 transient: allow immediate retry
```

The key row carries its own short lease (`Config.InFlightTTL`), renewed while held, so a request that
crashes does not leave the key in flight forever. `Begin` joins no transaction;
`Complete` must run inside the caller's transaction (it returns `ErrNotInTx`
otherwise). Scope is mandatory, fixing the global-key limitation of
`postgres/idempotent`. Keys can link to an execution for asynchronous APIs.

### Durable executions (`jobs`)

- `Submit(ctx, SubmitRequest)` dedups on `(scope, key)` through `idempotency_keys`
  and inserts the execution in the same transaction. It joins a `dbtx`
  transaction in `ctx` (the repo convention) and offers `RequireTx`.
- Handlers are registered per kind with options: mode, timeout, concurrency,
  retry policy, lease TTL, max attempts, payload decoder.
- `Execution` exposes the job, the lease, `Checkpoint`, `RunStep`, `Finalize`,
  and typed payload access.
- `x.RunStep(ctx, name, build, call)`, a generic method with `Req` and `Resp`
  inferred from `build` and `call` (D22): on first run, builds the
  request once, saves it with remote key `execution_id:name`, calls, saves the
  response; on retry, resends the saved request with the same key; when done,
  returns the saved response without calling.
- Errors: `Permanent(err)`, `RetryAfter(err, d)`, everything else retryable.
  Remote adapters map 409-in-progress and 429 to `RetryAfter`, and 422 key
  mismatch to `Permanent`.
- `Cancel(key)`: pending → cancelled; running → cancel flag observed at the next
  heartbeat, which cancels the work context with `ErrCancelled`; terminal → no-op.
- `Await(ctx, id, interval)` polls (no `NOTIFY` yet) and distinguishes a wait
  timeout (context error) from a failed job (returned with a nil error).
- Compensation: `OnFinalFailure` hook per kind runs as its own execution
  (`kind + ".compensate"`) with the failed execution's steps available.
- Worker: polling, graceful drain that hands unfinished work back (release
  lease, `run_at = now`, attempt not spent) instead of waiting for expiry,
  `Ready()` for probes, `RunOnce` for tests. Workers reap expired final
  attempts of their own kinds; `jobs.Janitor` purges finished executions and
  expired keys.

### Outbox (`outbox`)

- `Publish(txCtx, Event{Topic, AggregateKey, Type, Headers, Payload})` requires a
  transaction by default. Topics are registered with a config; publishing to an
  ordered topic without an aggregate key returns `ErrAggregateKeyRequired`.
- **Unordered events** are queue rows claimed individually.
- **Ordered events** take a gapless sequence by upserting `outbox_heads` in the
  producer transaction (the row lock serializes producers of one aggregate, so
  sequence order equals commit order). The relay claims a **stream** row in
  `outbox_streams`, publishes events with `seq > last_published_seq` in order,
  and advances the pointer with a fenced update. Heads and streams are separate
  tables so a long producer transaction cannot block relay heartbeats.
- Head-of-line policy per topic: `Block` (retry the stream, then mark it
  blocked and alert), `Park` (dead-letter the head event, emit a gap marker,
  continue) or `Stop` (manual resume only).
- `Publisher` contract: `Publish(ctx, []OutboundMessage) (published int, err
  error)` returns the length of the successfully published prefix. Envelope
  fields: message ID (`aggregate_key:seq` or event ID), topic, aggregate key,
  seq, type, headers.
- Admin: inspect a stream, resume, skip with gap marker, replay from a sequence,
  purge published events after retention.

### Inbox (`inbox`)

| Mode | Accepts | Rule |
| --- | --- | --- |
| `Dedup` | Any message with a stable ID | Apply once per `(source, message_id)` |
| `Sequenced` | Messages with aggregate key and producer seq (from an ordered outbox) | Apply only `seq = last + 1`; drop `seq <= last`; buffer `seq > last + 1` in `inbox_pending` and drain when the gap fills; gap markers advance the pointer |
| `Versioned` | Messages with aggregate key and a version or timestamp (third-party webhooks) | Apply only if newer than the stored version; otherwise a duplicate |

`Receive(ctx, mode, msg, apply)` applies in the same transaction as the dedup
record and returns `Applied`, `Duplicate`, `Buffered` or `GapSkipped`. `Enqueue(ctx,
msg, SubmitRequest)` is store-and-forward: dedup plus job submission in one
transaction, so the broker can be acknowledged immediately. Message-ID retention
and stream-position retention are separate; stream rows are never purged while
the stream is live.

### Scheduler (`schedule`)

Each tick submits a job keyed `schedule:<name>@<tick, RFC3339Nano>`, so several scheduler
instances dedup to one job. Missed-run policy (`All`, `Latest`, `None`) and
overlap policy (`Skip`, `Queue`, `Allow`) are per schedule. Time zones use IANA
names. Schedules can be paused.

### Cache (`cache`)

Generic methods `c.Get[T]`, `c.Set` and `c.GetOrCompute` (D22) with TTL plus
stale-while-revalidate: one caller computes under a compute lease while others
get the stale value or wait (configurable). Writes carry the compute token and
are rejected if a newer version exists. `Delete` and `InvalidateTx` leave a
versioned tombstone (D17); `InvalidateTx` runs inside the caller's transaction
so invalidation commits with the change that caused it. The table is UNLOGGED; contents are disposable.

## Cross-cutting

- **Observer**: one interface for lease lost (with cause), retry scheduled,
  permanently failed, stream blocked, inbox gap, claim latency, plus gauges for
  queue depth and oldest-pending age per kind and topic. Default is a no-op;
  an `slog` adapter ships with the module.
- **Hooks**: named fault-injection points (`BeforeHeartbeat`,
  `BeforeComplete`, `BetweenSteps`, `AfterRemoteCall`,
  `AfterPublishBeforeAdvance`) used by tests. They are nil by default.
- **Errors**: one vocabulary across packages (`ErrContended`, `ErrLeaseLost`,
  `ErrStaleToken`, `ErrRequestMismatch`, `ErrInFlight`, `ErrNotInTx`,
  `ErrTxInContext`, `ErrCancelled`, `TxOutcomeError`, ...), all matchable with
  `errors.Is`/`errors.As`.
- **durabletest**: a fresh database per test from `DURABLE_TEST_DSN` (D13),
  `Gate` to pause a hook at an exact point, and a recording publisher. Tests
  assert on resource contents and broker records, not only on metadata.

## Guarantees

| Scope | Guarantee |
| --- | --- |
| Acceptance | A request or submission with the same scope and key is accepted once within the retention window; a different body is rejected |
| Local effects (`Tx` mode or `Finalize`) | Effectively exactly once: business writes commit atomically with the outcome under a fenced, unexpired lease |
| Local effects (plain `Activity` writes) | At least once unless the writes are fenced by the caller |
| Remote effects | At least once; exactly once only if the remote API honours the step's idempotency key within its retention window |
| Outbox delivery | At least once; ordered per aggregate on the relay side best-effort; strictly ordered and deduplicated when consumed through a `Sequenced` inbox |
| Leases | Never two holders admitted to a fenced transaction for one key; activities can overlap after expiry, so resource-side fencing is required |

## Coexistence with existing modules

- The new module uses schema `durable` and new tables; existing `dbtx.*` tables
  and modules are untouched and keep working.
- Each feature reaches parity against the matching module's documented
  behaviour and tests before it is recommended; differences are recorded in
  [decisions](decisions.md).
- A migration guide per module (in-flight work, token history, retention) is a
  separate, later deliverable. No automatic migration is planned.
- The root README module table and the repository skills are updated only when
  a phase ships.
