# PostgreSQL fenced lease implementation plan

Source: [shared design conversation](https://chatgpt.com/s/t_6acefb00cddc8191b1ccd767901254bc).

Status: implemented in `postgres/lease`; the durable-module reuse options remain future work. See the [module README](../postgres/lease/README.md) for the shipped API, defaults, and verification commands.

See [integration examples](postgres-lease-examples.md) for client code and developer experience findings.

See [module reuse and shared-table limitations](postgres-lease-module-reuse.md) for compatibility with idempotent, jobs, outbox, and inbox.

## Scope and repository fit

Build a PostgreSQL-only fenced lease library for coordination across services. PostgreSQL is the sole coordination and token authority. External APIs and object storage remain supported activity workloads; coordination stays in PostgreSQL. Deliver the activity lifecycle, PostgreSQL lease storage, and short fenced transactions together.

Keep the existing `postgres/lock` advisory-lock API intact. It provides transaction-scoped serialization and cannot implement renewable leases with fencing without changing its contract. Keep distributed locking out of the root `dbtx` transaction API.

Follow the repository's separate-module convention:

```text
postgres/lease/             # separate module: PostgreSQL leases and execution helpers
  internal/                 # lease state machine, clock, private storage contract
  repository/
    schema.sql
    queries/
    sqlc.yaml
    postgres/               # generated queries
```

Use `postgres/jobs/worker.go` as a reference for conservative deadlines and heartbeat cancellation, and cache lease queries as a reference for owner-conditional updates. Do not refactor those modules into the new library as part of this work.

## API and behavioral decisions

Use two explicit execution paths: `Activity` for work outside a transaction, and PostgreSQL-specific `RunInFencedTx` for short database work. Replace the conversation's generic `WithLock` name with `Activity`. Expose observable termination causes and an explicit lease in both execution callbacks:

```go
// Locker is the concrete PostgreSQL coordinator, constructed by New(Config).
func (l *Locker) Acquire(context.Context, string, ...LockOption) (Lease, error)
func (l *Locker) TryAcquire(context.Context, string, ...LockOption) (Lease, error)

type Lease interface {
    Token() uint64
    Done() <-chan struct{}
    Err() error
    Renew(context.Context) error
    Release(context.Context) error
    RunInFencedTx(context.Context, func(context.Context) error) error
}

func (l *Locker) Activity(
    ctx context.Context,
    key string,
    fn func(context.Context, Lease) error,
    opts ...LockOption,
) error

func (l *Locker) TryActivity(
    ctx context.Context,
    key string,
    fn func(context.Context, Lease) error,
    opts ...LockOption,
) error
```

The PostgreSQL locker additionally exposes:

```go
func (l *Locker) RunInFencedTx(
    ctx context.Context,
    key string,
    fn func(context.Context, Lease) error,
    opts ...LockOption,
) error

func (l *Locker) TryRunInFencedTx(
    ctx context.Context,
    key string,
    fn func(context.Context, Lease) error,
    opts ...LockOption,
) error
```

`locker.RunInFencedTx` acquires and releases a new lease. `held.RunInFencedTx` validates an already acquired lease and commits a short transaction under that ownership; it neither reacquires nor releases the lease. Use the latter for activity finalization, with the same fencing token exposed by `held.Token()`.

| Execution path | Intended work | Transaction lifetime |
| --- | --- | --- |
| `locker.Activity` | External APIs, object storage, long computation, multi-step work | Acquire/renew/release use brief coordination transactions; no transaction spans the callback. |
| `locker.RunInFencedTx` | Short SQL reads/writes that can commit together | The helper acquires a lease, validates it under a row lock, runs the callback, and commits its protected writes in one short business transaction. |

Choose `RunInFencedTx` over `RunInLock`: the name states that the callback runs in a transaction with enforced ownership. Both paths coordinate ownership through the same namespace/key. Activity callbacks can overlap after expiry; guarded transaction commits are serialized. The callback transaction context is passed to repositories through `dbtx`.

The library cannot stop a paused worker or revoke an external request. After a stop-the-world pause, a worker may continue its activity even though PostgreSQL has granted ownership to a successor. `held.RunInFencedTx` rejects expired/mismatched ownership before invoking its callback, including expiry without a successor and takeover followed by release. Context cancellation and `Done` checks are cooperative observations, not resource protection.

An external fencing watermark rejects an older token only after accepting a newer token; it does not independently validate lease expiry. Provider idempotency, conditional object writes, and PostgreSQL ownership guards have different contracts. Integration examples must state which one applies rather than promise exclusive activity execution or exactly-once effects.

`Activity` rejects a transaction-bearing context through the PostgreSQL backend's context validation, before acquisition or callback execution. It never wraps the callback in `RunInTx`, retains a database connection, or holds a row/advisory lock across external work. The caller must invoke it outside an enclosing transaction as well; detaching context cannot close a transaction the caller already holds. An activity may explicitly perform brief database transactions between external steps, but its lease alone does not fence those writes. Finalization must validate ownership atomically when required. Do not silently make a separately acquired `RunInFencedTx` reentrant on the activity's key.

The PostgreSQL locker uses validated value configuration through `New(Config)`, receiving the same `*dbtx.DB` used by application repositories so pool identity and transaction-context ID are explicit. Acquisition options specify TTL; contention behavior is explicit in the method name, with no try-acquire option.

| Waiting method | Immediate method | Result on contention |
| --- | --- | --- |
| `Acquire` | `TryAcquire` | Waiting retries with bounded jitter until acquired or the context ends; immediate returns `ErrContended`. |
| `Activity` | `TryActivity` | Same acquisition behavior; callbacks run only after successful acquisition. |
| `RunInFencedTx` | `TryRunInFencedTx` | Same acquisition behavior; callbacks run only after acquisition and transactional ownership validation. |

Immediate methods make one acquisition attempt and do not retry logical contention. Use non-waiting PostgreSQL row-lock acquisition (including the first-row initialization race) so a contested coordination row also returns `ErrContended` rather than waiting for its owner. They still incur database, pool, and network latency bounded by context; transport/database failures remain distinct errors. After acquisition, try execution helpers retain the ordinary callback, renewal, transaction, and cleanup contracts. `held.RunInFencedTx` validates existing ownership and does not acquire again, so it has no try-acquisition variant.

Waiting methods stop on context cancellation/deadline or a non-contention error. The caller's context bounds both waiting and subsequent execution; a separate acquisition-only waiting budget remains a possible future option. No fairness, reentrancy, or multi-key atomic acquisition in v1. Keys are nonempty strings scoped by namespace; store complete keys rather than advisory-lock hashes.

Every successful acquisition returns a positive fencing token from PostgreSQL. Both callbacks receive their acquired `Lease`, so clients read `lease.Token()` consistently. Do not expose a latest-token lookup as proof of ownership: a stale client must never substitute its successor's token. Resource-side enforcement remains required for external writes. Token ordering is scoped to the same namespace/key and PostgreSQL authority; database recovery must preserve that history. No backend capability negotiation is needed.

Each acquisition gets a cryptographically random ownership ID. This ID authorizes renew/release; the fencing token orders ownership generations and is not a secret. Do not expose ownership credentials in logs.

Define distinguishable errors for contention, lease loss, local release, invalid options. Preserve underlying context and transport errors with wrapping. Lease methods are safe to call concurrently; release and renewal serialize their local state transitions.

`Done` closes once on release, detected loss, or the conservative local expiry deadline. `Err` reports the terminal cause after closure. Raw `Acquire` and `TryAcquire` do not start heartbeat renewal; they do own deadline monitoring. Cancellation of the acquisition context after a successful return does not release the lease. Manual users must renew and release explicitly.

## Phase 1: Core lifecycle and deterministic tests

Implement contracts, configuration, error semantics, and the lease state machine using a private PostgreSQL storage contract. Use an internal controllable clock and a fake storage implementation to test timing without real sleeps.

`Activity` must:

1. Acquire before invoking the callback; never invoke it after failed acquisition.
2. Derive a callback context with a cancellation cause and cancel it when ownership becomes uncertain or lost.
3. Renew approximately every TTL/3, with bounded request deadlines and retry backoff inside the remaining lease budget.
4. Measure each acquisition/renewal budget from before the request, subtract a TTL/6 safety margin, and reject responses that arrive after the safe budget expires. Bound ownership uncertainty even when a renewal call stalls.
5. Treat authoritative owner mismatch as immediate loss. Transient transport errors do not extend the deadline; once the conservative deadline passes the lease becomes terminal and a late success cannot revive it.
6. Invoke the callback synchronously. If it ignores cancellation, keep waiting for it to return; do not claim it has stopped or return while it continues in a hidden goroutine. Renewal stops after terminal loss.
7. Stop and join renewal work before cleanup. Release on a separate bounded context that survives caller cancellation. Preserve callback, ownership-loss, and cleanup failures using `errors.Join` where applicable.
8. Attempt bounded cleanup on panic, then re-panic. A callback returning nil after observed lease loss must not make `Activity` return success.

Verify that external work can remain blocked while acquisition/renewal transactions finish and database connections return to the pool. Reject an enclosing transaction before starting work. Add an example with an external API call and a stable logical-operation idempotency key, with no database transaction around that call.

Release becomes locally terminal immediately. Repeated release after confirmed success is harmless; an uncertain remote release can be retried with the same ownership ID. No operation may resurrect a terminal local lease. An ambiguous acquisition returns no usable lease; best-effort owner-qualified cleanup or TTL expiry bounds its remote effects.

Exit criterion: lifecycle tests pass under race detection, including renewal/release races, stalled responses, parent cancellation, callback panic, and callback success racing with loss.

Test every waiting/try pair: waiting succeeds after release or returns context cancellation; try returns `ErrContended` without invoking a callback, returning a lease, or starting renewal. Successful try calls follow the same lifecycle as waiting calls.

Add controlled pause tests that stop renewal and local cancellation handling while PostgreSQL and a second node progress. Merely blocking the user callback while heartbeat goroutines keep running does not simulate a process pause. Include late renewal success, expired ownership without takeover, and a successor that has already released when the original worker resumes.

## Phase 2: PostgreSQL leases

Create a persistent `dbtx.leases` table with `(namespace, key)` primary key, nullable owner ID, positive `bigint` fencing generation, and expiry timestamp. Public tokens use `uint64`, but the PostgreSQL backend validates the positive signed-bigint range and fails on exhaustion.

Acquisition uses a short backend-owned transaction to initialize and lock the key row, inspect availability, increment the generation, assign the new owner, and commit. Increment only for successful new acquisitions; renewal never changes the token. Keep rows after release so reacquisition cannot reset generations. Namespace deletion, database replacement, and restores require an explicit fencing policy.

Evaluate expiry with the database clock after acquiring the row lock. Avoid a timestamp captured before lock waiting that could grant or renew a lease using stale time. Renew matches namespace, key, ownership ID, token, and an unexpired lease; release matches the same ownership generation and clears ownership. Stale calls cannot mutate a successor lease. Bound lock waits through request context and transaction settings.

Coordination operations must use the pool and independent transactions rather than accidentally joining the caller's business transaction. Constructor dependencies remain caller-owned; schema installation is explicit, never a hidden constructor action. Embed schema and check in sqlc-generated code following existing repository patterns.

Exit criterion: container-backed tests prove single-winner acquisition, independent-key concurrency, expiry takeover, increasing tokens, stale renewal/release rejection, cancellation during row contention, and ambiguous-response recovery.

Include try-acquisition tests against both an active lease and a row held by a fenced transaction, plus concurrent first acquisition of a previously absent key. Verify no logical contention retry or waiting on the contested coordination row.

## Phase 3: Atomic PostgreSQL resource protection

Provide `locker.RunInFencedTx(ctx, key, fn, opts...)`, integrated with `dbtx`. Acquire the lease using the same backend and namespace as `Activity`, then open a fresh business transaction. Verify the active owner/token/expiry while locking the lease row, execute the callback's protected writes in that transaction, and retain the row lock until commit. Release the lease after commit or rollback using bounded cleanup; preserve cleanup errors separately from the already committed outcome. Do not automatically retry the callback after an ambiguous commit.

Require a fresh helper-owned transaction in v1; reject an existing transaction context to avoid returning before the actual outer commit. Check expiry after obtaining the row lock. While this transaction holds the lease row, successors cannot acquire it. Use a finite configurable transaction timeout, database statement/lock timeouts, and rollback on error or panic. No external network calls or long computation inside this transaction; use `Activity` for that work.

This path does not run heartbeat renewal against the row it holds. A transaction admitted with a valid lease remains serialized until commit/rollback because takeover must acquire the same row lock, even if wall-clock TTL passes during the transaction. Its transaction deadline bounds that lock lifetime. Cancellation remains cooperative in Go; database timeouts bound database work and idle transactions. Keep this transaction contract separate from `Activity`'s renewable lease deadline and loss handling.

For `held.RunInFencedTx` during an activity, coordinate with the activity's renewal owner: temporarily suspend heartbeat requests during the short guarded transaction, then resume only if the lease remains valid after commit/rollback. Never revive a locally terminal lease. Serialize or explicitly reject concurrent lifecycle mutations; do not let a renewal request block on a row held by its own finalization transaction. Tests must cover finalization racing with renewal, expiry, release, and cancellation.

Define an inspectable typed outcome error for confirmed commit followed by cleanup failure versus rollback or an unknown commit result. Client code must not infer rollback from every non-nil error or blindly retry potentially committed work.

If coordination and protected data live in different databases, this helper cannot provide atomic protection. The target resource must instead maintain and atomically enforce its own fencing watermark. Specify whether equal-token writes are permitted for multiple operations by one owner and handle duplicate requests separately. Reject older generations; token comparison is not an exactly-once mechanism.

Add an example that pauses owner A, lets B take over, then demonstrates A's write rejection. Also prove serialization when A already holds the fenced transaction and B attempts takeover.

Exit criterion: tests verify actual resource contents, including stale ownership at admission, commit races, rollback, transaction timeout, and takeover waiting until commit. Also prove mutual exclusion between `Activity` and `RunInFencedTx` for the same key, and that the transaction helper starts no heartbeat goroutine.

## Resource integrations and migration rules

- PostgreSQL: use `RunInFencedTx` for short database-only work where lease and resource share a database; use `Activity` for long work and external calls. Activity finalization needs its own short atomic ownership guard rather than an unguarded write after a lease check.
- Object storage: provide an example using conditional version/ETag writes. Describe the preconditions it enforces; a Go lease token is not automatically validated by the store.
- External APIs: use stable logical-operation idempotency keys when supported, or an enforcing gateway. Context cancellation cannot revoke an already accepted request.
- Multiple nodes: use identical resource keys/namespace and one writable PostgreSQL authority. Leases serialize ownership rather than deduplicate sequential executions; record logical-operation receipts atomically with effects. Preserve database constraints and reject reused operation IDs with mismatched payloads.
- Database replacement/failover: preserve acknowledged fencing history when moving or promoting a PostgreSQL database, and fence the old primary. An asynchronous promotion that loses committed counter increments can invalidate monotonicity. Independent databases with separate counters are separate authorities and must not coordinate the same live resource.
- Disaster recovery: restoring old coordination state can regress tokens. Require quiescing workers and reconciling resource watermarks or advancing an enforced epoch before resuming. Preserve counter rows during ordinary maintenance.

## Delivery and verification

The first release complements the existing durable modules; it does not migrate their schemas or replace their state machines. Prefer evaluating shared lifecycle code with module-owned rows before a shared ownership table. Any later shared-table integration needs caller-transaction storage primitives, atomic domain transitions, retention/fencing-history policy, and explicit expiry-policy compatibility. The public execution wrappers alone are insufficient; details and acceptance criteria are in the module reuse document.

Implement as reviewable changes in this order within `postgres/lease`: contracts and Activity lifecycle; PostgreSQL schema/storage; RunInFencedTx and examples for both execution paths. The release includes all three phases.

For the new module run `go test -race -count=1 ./...` and `go vet ./...`. Root tests do not visit nested modules. Wire local module replacements in a temporary test workspace so verification exercises checkout versions, and explicitly include the new module in automation. Run sqlc generation and check that generated code is reproducible.

Document defaults, timing assumptions, ownership, cleanup behavior, PostgreSQL guarantees, and examples in package READMEs. Add the new module to the root module table. Do not claim exactly-once execution or universal protection of arbitrary shared resources.

Use the integration examples' scenario verification matrix as an acceptance checklist: multi-node duplicates, pauses, partitions, process loss, pool exhaustion, late replies, ambiguous commits, cleanup failures, nested acquisition, and resource-side enforcement limits. Test behavior against durable resource contents and operation receipts, not just lease metadata.

The first implementation uses the defaults documented in the module README and exposes `TxOutcomeError` for transaction outcomes. Waiting versus immediate acquisition is selected through the explicit method pairs above. The existing-lease finalization guard is `held.RunInFencedTx`; ownership credentials stay private. Configuration and TTL option names are documented in the module README. The API is implemented; changes to existing durable modules remain outside this release.
