# Lease reuse across PostgreSQL modules

Status: resource-lease design constraints and future options. The [shared retry
lifecycle](../postgres/retry/README.md) now implements retry transitions over
module-owned rows; it does not introduce a shared resource-lease table.

Related: [lease implementation plan](distributed-lock-plan.md) and [integration scenarios](postgres-lease-examples.md).

## Scope and recommendation

The proposed `postgres/lease` module supplies ownership, renewal, fencing, and short guarded transactions. It does not replace durable requests, queues, message deduplication, or delivery protocols.

Initially keep `postgres/idempotent`, `postgres/jobs`, `postgres/outbox`, and `postgres/inbox` intact. Prefer sharing lifecycle code where contracts match, while keeping ownership fields in each module's own rows. Evaluate a common lease table separately, with explicit atomicity, retention, performance, and migration requirements. Do not force an extra generic lease around operations that already own their work through a module-specific claim.

The existing `postgres/lock` advisory-lock module remains a separate transaction-scoped primitive.

## Behavior that must remain module-specific

| Module | Durable behavior beyond ownership | What a lease alone cannot provide |
| --- | --- | --- |
| `idempotent` | Request comparison, response replay, checkpoints, terminal outcomes, attempts, backoff, retention | Remembering that a logical request completed and replaying its result |
| `jobs` | Durable submission and payload, discovery, scheduling, worker concurrency, attempts, retries, result/archive maintenance | Discovering and retrying abandoned work without another caller supplying it |
| `outbox` | Events persisted with business writes, delivery attempts, delayed visibility, dead letters | Ensuring a committed business change has a durable event available for later publication |
| `inbox` | Message identity deduplication, durable pending work, aggregate ordering, retries, acknowledgement | Remembering redeliveries and committing acknowledgement with database effects |

Two callers may acquire the same lease sequentially and both execute. Sequential ownership is not completion memory or logical-operation deduplication. Activities may also overlap after expiry. Durable receipts and unique constraints remain necessary where repeated effects must be suppressed.

External effects are not atomically covered by PostgreSQL transactions in any of these designs. Retain provider idempotency, reconciliation, and resource-side enforcement where required.

## Existing execution contracts

The current `idempotent` implementation runs each step in a transaction that locks its request row and commits business writes with a checkpoint or final response. It uses a savepoint to roll back handler writes while recording failure/backoff under the same row lock.

The current `inbox` and `outbox` implementations similarly lock each claimed message during its handler transaction. Inbox acknowledges with database effects; outbox publication can still occur externally before the acknowledgement transaction commits, so delivery can repeat. Inbox also enforces optional aggregate ordering, which independent resource leases do not recreate.

The current `jobs` worker runs handlers outside a transaction and renews their claims using heartbeat requests. Its handler's business effects do not commit atomically with job completion. This already resembles `Activity`, with durable discovery and scheduling added.

These contracts overlap with the lease module but are not interchangeable. Moving a transaction-held handler to activity execution permits takeover and overlapping callbacks after expiry, and changes how failure/acknowledgement is recorded. Such a change needs an explicit API contract and tests rather than an internal replacement presented as equivalent.

There is a specific expiry mismatch: current jobs SQL permits heartbeat/completion after expiry if the owner/token still match and the job has not been reclaimed or terminated. The proposed generic lease rejects expired ownership even without a successor. A shared implementation must preserve the existing policy or introduce a deliberate compatibility change. Token scope differs too: idempotent currently uses a sequence across claims, jobs increments per job, and the proposed lease increments per namespace/key. These values cannot be substituted as a common token domain without migration design.

## Option A: shared lifecycle code, module-owned rows

Share validated timing configuration, conservative client deadlines, terminal-state handling, heartbeat scheduling, error wrapping, and relevant contract tests where semantics agree. Private storage adapters retain module-specific claim, renewal, and completion SQL.

Benefits:

- Ownership and domain transitions can remain single-row operations.
- Queue selection and `SKIP LOCKED` behavior retain their existing indexes and query shapes.
- Existing atomic handler/outcome transitions do not require another ownership row.
- Schema migrations and ownership histories stay local to each module.

Limitations:

- Ownership fields and some SQL remain duplicated.
- Different expiry, panic, shutdown, batching, and retention policies prevent one universal lifecycle implementation.
- Separate nested Go modules cannot import another module's `internal` package. Any cross-module runtime reuse needs an intentionally exposed support package or a different module layout; a private abstraction inside `postgres/lease/internal` is not automatically reusable by siblings.

Do not broaden the public API into a generic workflow framework merely to remove repeated fields. Establish concrete reuse through adapters before freezing a cross-module extension contract.

## Option B: shared lease table, separate domain tables

One possible layout is:

```text
dbtx.leases       namespace, key, owner_id, fencing_token, expires_at
dbtx.idempotency  request, response, checkpoint, status, attempts, retry_after
dbtx.jobs         payload, schedule, status, attempts, result
dbtx.outbox       event, delivery status, attempts, available_at
dbtx.inbox        source/message identity, aggregate order, status, attempts
```

The domain tables still exist. A shared lease table centralizes ownership storage, not the durable state machines.

### Atomic integration requirements

| Operation | State that must change in one transaction |
| --- | --- |
| Job claim | Eligible job selection, lease grant/new generation, running status, attempt accounting |
| Idempotency claim | Request comparison/terminal-result decision, grant, attempt and backoff eligibility |
| Idempotency step | Ownership guard, business writes, checkpoint/response or failure transition |
| Inbox completion | Ownership guard, business writes, message acknowledgement |
| Outbox enqueue | Business writes and durable event insertion; no generic lease is required merely to enqueue |
| Outbox delivery completion | Ownership validation and delivery state; the external publication remains outside PostgreSQL atomicity |
| Terminal completion/release | Confirmed domain outcome and appropriate ownership release, without a successor being accidentally cleared |

Acquiring a lease in transaction A and changing job/request state in transaction B leaves failure windows such as "lease acquired but job not claimed" or "claim recorded without usable ownership." A cleanup attempt does not substitute for atomicity.

The public `Activity` and `RunInFencedTx` APIs own their lifecycle and reject enclosing transactions. They are insufficient by themselves for these integrations. A shared-table design needs deliberately scoped storage primitives that participate in a caller-owned transaction without starting an independent transaction or releasing ownership early. Domain modules decide eligibility, outcomes, retries, and attempts; storage operations decide ownership transitions.

All operations must use the same database and a documented lock order across lease/domain/aggregate rows. Claims, completion, cleanup, and maintenance must follow that order to avoid deadlock cycles. A callback must not manually renew or release its lease while holding its guard row. Unknown commit outcomes still require reconciliation.

### Storage and operational limitations

- **Queue discovery:** workers must join or coordinate eligible domain rows with lease rows, skip unavailable resources, and refill batches appropriately. Locking a domain row does not automatically lock its lease. Benchmark contention and starvation; do not assume existing single-table `SKIP LOCKED` queries can be copied unchanged.
- **Initialization:** first acquisition may race on absent lease rows. Preserve try-method contention semantics and atomic domain claims during that race.
- **Retention:** completed messages, job keys, and idempotency records have different purge windows. Generic per-key lease generations must survive ordinary release and maintenance under the proposed fencing contract. Domain-row deletion must not silently erase that history.
- **Growth:** one permanent lease row per transient job/message can grow indefinitely. Resource-key granularity, an alternative durable token authority, or an enforced epoch/history policy must be decided before adopting that mapping. A global sequence may help avoid token reuse but introduces different token and restore assumptions; it is not a complete automatic solution.
- **Namespaces:** explicit module/tenant scopes prevent collisions. Sharing the table does not mean a job key and inbox message key should contend unless that is an intentional resource invariant.
- **Write overhead:** extra lease inserts/updates, joined indexes, and transaction row locks can outweigh the benefit of fewer repeated schema fields. Measure WAL, table/index growth, vacuum pressure, latency, and pool use.
- **Migration:** preserve in-flight owners, token histories, outcomes, and retry rules. Existing and new claim paths must not become independent authorities during rollout. A paused old worker can still resume after a deploy.
- **Database recovery:** failover/restore must preserve the shared authority's acknowledged history and fence old primaries. Central storage does not eliminate the deployment limitations in the lease plan.

## Appropriate composition without wholesale migration

A lease can coordinate a resource shared by several independently queued jobs—for example, multiple job types updating the same account. The job owns durable delivery; the resource lease owns temporary access to the account. Those are two distinct scopes. Both ownership contracts must be checked when needed, and retries must retain stable logical-operation IDs.

Do not invoke `Activity` from the existing inbox/idempotent/outbox transaction callback: it would retain an enclosing transaction and conflict with the proposed activity contract. Long external work needs an explicit specialized execution path that preserves the module's durable claim and completion protocol.

An outbox enqueue can join the short guarded business transaction without replacing the outbox. A resource lease adds admission protection; the outbox adds durable publication intent.

## Acceptance criteria for any later simplification

Before adopting shared runtime code or storage, prove:

1. Claim and completion preserve each module's atomic domain transitions, including failure/savepoint handling.
2. Multi-node polling, batching, aggregate ordering, attempt accounting, and retry/backoff behavior remain correct.
3. Expiry and post-expiry completion policy are preserved or explicitly versioned.
4. Late heartbeat/release calls cannot change a successor's domain state or ownership.
5. Purge, archive, restore, and mixed-version rollout cannot reset a live resource's fencing history.
6. Duplicate request/message/job semantics and payload mismatch detection remain intact throughout retention windows.
7. Unknown commits and external effects still have reconciliation paths.
8. Storage overhead and contention are measured against the existing implementations.

These are future evaluation criteria, not additional scope for the first lease release. No existing module or table is to be removed as part of that release.
