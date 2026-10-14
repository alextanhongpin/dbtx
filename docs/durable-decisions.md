# Durable module: decisions

Companion to the [design](durable-design.md). Each entry records what was
decided, why, and what follows from it. All entries are decided; D12–D21 were
made while implementing. Open follow-ups are tracked in the
[TODO](../postgres/durable/TODO.md).

## Architecture

### D1. Fresh unified module, not a wrapper

**Decision.** `postgres/durable` is a new implementation, not a composition of
the existing modules.

**Why.** Shared engine code needs one module (nested modules cannot share
`internal`), and one token domain, expiry policy and error vocabulary cannot be
retrofitted onto modules whose contracts differ
([reuse limits](postgres-lease-module-reuse.md)).

**Consequences.** Behaviour proven in `lease`, `idempotent`, `jobs`, `inbox`
and `outbox` is reimplemented; their READMEs and tests are the reference.
Existing modules are not changed or deprecated.

### D2. Separate tables for idempotency, executions and steps

**Decision.** `idempotency_keys`, `executions` and `execution_steps` are
separate. Submission writes the key and the execution in one transaction.

**Why.** Different lifetimes and readers; idempotency is also needed without an
execution (synchronous requests, inbox); execution churn needs its own vacuum
tuning.

**Consequences.** Job keys have status `linked` and point at their execution.
`jobs.Client.Purge` never deletes an execution while its key is remembered, so
a replayed submission never points at a missing row.

### D3. Driver

**Decision.** The library depends only on `database/sql` and root `dbtx`. Tests
use `lib/pq`, as the other modules do. SQLSTATE codes are read through a
`SQLState() string` interface, which both `*pq.Error` (verified) and
`*pgconn.PgError` implement.

**Why.** Switching to pgx was deferred at the user's request; the interface keeps
that a test-only change.

### D4. One module, a package per feature, no central wiring

**Decision.** Packages `lease`, `txlock`, `keys`, `jobs`, `outbox`, `inbox`,
`schedule`, `cache`, `durabletest`; shared code in `internal/engine` (lease
runtime) and `internal/pg` (SQL helpers). Shared types (errors, `Permanent`,
`RetryAfter`, `Backoff`, `Observer`, `Hooks`) live in the root package. Each
package has its own constructor taking `*dbtx.DB`; there is no `System` type.

**Why.** A central wiring type would make the root package import every
feature (and features import the root), and applications rarely need all of
them. Composition happens through exported APIs: the inbox and scheduler take
a `*jobs.Client`.

### D5. Token domains

**Decision.** Resource leases use a per-key generation on rows that are never
purged. Everything else (idempotency keys, executions, outbox streams, cache
versions) draws from the sequence `durable.fencing_seq`.

**Why.** Resource rows are long-lived; executions, keys and messages are
purged, and a sequence never repeats after a key is reused.

### D6. Fencing after failover or restore

**Decision.** `durable.advance_fencing(gap default 1e9)` moves the sequence and
every lease generation forward by a gap. Operators run it after promoting a
replica or restoring a backup, before workers resume. Synchronous replication
is recommended in addition.

**Why.** Asynchronous promotion can lose committed increments and reissue a
token. Advancing every domain past any value that may have been issued is
simpler than a separate epoch column and keeps tokens a single `bigint`.

### D7. Strict expiry

**Decision.** Writes after a claim require matching owner and token **and** an
unexpired lease by the database clock. Exception: while the writer holds the
row lock (a `ModeTx` step, `Finalize`, `RunInFencedTx`), owner and token are
enough, because no successor can claim the row until it commits.

**Why.** One rule everywhere; a worker past its lease may have been paused.
The row-lock exception lets a step run longer than the TTL, as
`postgres/idempotent` allows.

**Consequences.** Differs from `postgres/jobs`, which completes after expiry
when nobody reclaimed the job; here that work is retried instead. One more
exception: marking unordered outbox events published needs only the owner,
because the publish already happened and a matching owner proves no other
relay reclaimed them (D16).

### D8. Two execution modes, stored per execution

**Decision.** `ModeTx` (row-locked step transactions; checkpoints and
completion commit with the step's writes) or `ModeActivity` (outside
transactions, heartbeats, `Finalize` for atomic completion). `RunStep` only
works in activity mode. The mode is fixed at the first claim; retries use the
stored mode even if a later deploy registers the kind differently.

### D9. Schema installation

**Decision.** `repository.Schema` and `repository.Migrations`
(`migrations/0001_init.sql`). Constructors never run DDL. Statuses are `text`
with `CHECK` constraints; the schema applies twice without error.

### D10. Inbox out-of-order messages

**Decision.** In `ModeSequenced`, `seq > last+1` is stored in `inbox_pending`
and acknowledged; buffered messages are applied in order, each in a savepoint,
when the gap fills. A failing buffered message stays buffered (`Drain` retries
it). `Gaps` lists waiting streams; `SkipGap` gives up on the missing range.

**Why.** On Kafka, refusing one message stalls the whole partition.

### D11. Outbox heads and streams are separate tables

**Decision.** Producers upsert `outbox_heads` for the sequence; relays lease
`outbox_streams`.

**Why.** A producer's upsert holds its row lock until commit; sharing that row
with relay heartbeats would let a slow producer cost the relay its lease.

## Policies (previously open questions Q1–Q11)

| # | Question | Decision |
| --- | --- | --- |
| Q1 | Idempotency key after a failure | `keys.Do` stores the response when the error is `durable.Permanent` and the response has a status; any other error (or a panic) releases the key for immediate retry |
| Q2 | Requeue semantics | Grants as many extra attempts as the execution originally had (`max_attempts = attempts + max_attempts`); history stays in `execution_attempts` |
| Q3 | Outbox pointer granularity | Per event by default; `AdvancePerBatch` per topic |
| Q4 | Unregistered kinds and topics | Rejected: `ErrUnknownKind`, `ErrUnknownTopic`. `jobs.Config.Kinds` and `outbox.Config.Topics` are required |
| Q5 | Scheduler defaults | Missed runs `MissedLatest`, overlap `OverlapSkip` |
| Q6 | Cache contention without a stale value | Wait (poll) up to `ComputeTTL`, then compute; `ComputeAnyway` available |
| Q7 | Outbox retention and replay | Published events are kept until `PurgePublished`; `Replay` works only from a retained sequence |
| Q8 | Payload versioning | `payload_version` per execution; `OldestPayloadVersions` reports the oldest unfinished version per kind |
| Q9 | Compensation | `KindOptions.Compensate` submits `<kind>.compensate` in the same transaction that records a final failure (including failures found by the reaper) |
| Q10 | Await mechanism | Polling only for now; `LISTEN/NOTIFY` is a follow-up |
| Q11 | Parity bar | Each package was built against the matching module's README and the failure-mode list; porting those modules' test suites verbatim is a follow-up |

## Made during implementation

### D12. Queries generated with sqlc

**Decision.** All SQL lives in `repository/queries/*.sql`, generated with sqlc
v1.31.1 into `repository/postgres` (checked in), following the repository
convention and the `dbtx-integrate` skill: packages construct
`postgres.New(db.DBTx(ctx))` per operation and never edit generated files.

**Why.** Typed parameters and results, one place to read every query, and no
SQL assembled from Go string constants. (The first implementation used
hand-written SQL; it was replaced.)

### D13. Tests use a DSN, not containers

**Decision.** `durabletest.NewDB` creates and drops a fresh database per test on
the server in `DURABLE_TEST_DSN` (or `DATABASE_URL`); tests skip without it.
The repository's `compose.yaml` database works.

**Why.** `testing/dbtest` pulls in Docker client dependencies; a DSN keeps the
module's dependencies to `dbtx` and `lib/pq`. Using `dbtest` containers is a
possible follow-up.

### D14. No injected clock

**Decision.** Expiry is decided by the database clock and local deadlines by
Go's monotonic clock; tests use short real TTLs plus `Hooks` and
`durabletest.Gate` to create pauses at exact points.

**Why.** A fake Go clock cannot move the database clock, so it could not
exercise the expiry checks that matter.

### D15. Heartbeats wake immediately only after user operations

**Decision.** After a user lifecycle operation (`Finalize`, `RunInFencedTx`,
manual `Renew`) the keeper renews at once; after an automatic heartbeat it
waits TTL/3.

**Why.** An earlier version woke after every operation, so heartbeats ran back
to back (found in review; `TestHeartbeatInterval`).

### D16. Unordered outbox: one rejection costs one attempt

**Decision.** When the publisher accepts a prefix of a batch, the first
rejected event spends an attempt and the rest return to pending without one.
Shutdown or a lost lease never spends attempts or parks a stream head.
`PublishTimeout` is capped at half the lease.

**Why.** Otherwise one poison message at the front of a batch kills every
message behind it.

### D17. Cache invalidation leaves a versioned tombstone

**Decision.** `Delete`/`InvalidateTx` set the value to NULL with a new version
instead of deleting the row; `Purge` removes tombstones later.

**Why.** A computation that started before the invalidation must not store its
stale result afterwards (versioned writes need the newer version to exist).

### D18. Outbox admin operations fence out the current relay

**Decision.** `Resume`, `Skip` and `Replay` give the stream a new token and
clear its owner.

**Why.** A relay holding the stream would otherwise advance past events that
`Replay` just re-queued, losing them.

### D19. Cancellation and outcome precedence in jobs

**Decision.** When an activity handler returns, a successful result is always
recorded, then permanent failures; only then do cancel requests and shutdown
apply. A retry scheduled after a cancel request becomes `cancelled`. In tx
mode, a step rolled back by an attempt timeout or shutdown is recorded in a new
fenced transaction (failure or hand-back) instead of waiting for lease expiry.
Workers only reap expired executions of kinds they registered.

### D20. Per-feature tables, shared lease functions

**Decision.** Each feature keeps its own table. Every ownership change goes
through five PostgreSQL functions that take the table as a `regclass`:
`durable.lease_grant` (claim candidate rows), `lease_renew`, `lease_lock`
(lock and check before writing feature columns), `lease_finish` (end with a new
status, optional `run_at` delay, `last_error`, attempt adjustment) and
`lease_expire` (owner presumed dead). Tables opt in by having fixed column
names: `id`, `status`, `owner_id`, `token`, `lease_expires_at`, `updated_at`,
plus `run_at`, `attempts`, `max_attempts`, `last_error` when used.
`durable.lease_contract` reports missing columns. Features select candidates
themselves, with their own filters and `FOR UPDATE SKIP LOCKED`, and write
their own columns only after `lease_lock` in the same transaction.

Adopted by `executions`, `outbox_events`, `outbox_streams` (held while
`active`; `attempts` counts consecutive head failures) and
`idempotency_keys` (in-flight requests). Resource leases (`leases`) and cache
compute leases keep their own functions because their token domains differ
(D5).

**Why.** The fencing rules (owner, token, unexpired lease, the row-lock
exception) now exist once instead of in dozens of queries; the review had found
drift twice. sqlc cannot parameterize table names but can call a function.
Fixed column names keep calls short and avoid a column-mapping layer.

**Consequences.** The functions use `EXECUTE format(...)`, so they are
re-planned per call (negligible next to a round trip) and a missing column is
only reported when the function runs; `TestLeaseContract` guards that. The
functions return `(id, token, attempts)`; claim queries join the feature table
for immutable columns in the same statement.

### D21. NULL means "no lease"; nullable JSON

**Decision.** Lease and expiry timestamps are NULL when there is no lease
(instead of `'-infinity'`), and every `jsonb` column is generated as `pg.JSON`,
a `[]byte` type that scans and writes NULL.

**Why.** `lib/pq` cannot scan infinite timestamps into `time.Time`, and Go's
`json.RawMessage` cannot scan NULL. NULL is also the honest value: comparisons
with it are false, so an absent lease is never live and never "expired".
