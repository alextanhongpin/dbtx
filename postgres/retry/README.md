# Shared PostgreSQL retry lifecycle

Inbox, outbox, jobs and idempotency keep their own tables and public Go APIs.
They use `dbtx.retry_transition` for ownership and retry updates through
automatically updatable lifecycle views. An update through a view changes the
original row and holds its original row lock until the caller's transaction ends.
There is no shared work table or second ownership record.

## Common state interface

Each `<module>_retry_state` view exposes these columns:

| Column | Meaning |
| --- | --- |
| `retry_key` | The original key in its native type: UUID or text. |
| `status` | The module's existing status enum or text. |
| `attempts`, `max_attempts` | Attempts started and the configured limit. |
| `lease_owner`, `fencing_token` | The existing ownership identity. |
| `retry_at`, `lease_expires_at` | Retry eligibility and lease expiry. |
| `last_error` | Latest failure. |
| `completed_at`, `updated_at` | Lifecycle timestamps. |

The views expose only this lifecycle contract. Original table columns, indexes,
constraints, statuses, fencing-token domains and timestamp rules remain intact.
Jobs and messages still use one physical timestamp for both eligibility and
expiry; the two view aliases do not introduce another source of truth.
Idempotency retains its separate timestamps and per-call attempt limit. Its view
has a NULL `max_attempts`, and Nack supplies the limit explicitly. Messages retain
their existing owner guard; their synthetic `fencing_token` is NULL.

## Shared transitions and local rules

The shared function locks the selected row, rechecks ownership, applies the
transition, and returns the canonical state. Keys and values are bound SQL
parameters; relation and column types come from `regclass` and PostgreSQL's
catalogs. Key comparisons cast the parameter, not the indexed column. State
projection excludes payloads and responses. Typed module adapters return the
original row directly, preserving JSON `null` values.

| Action | Behavior |
| --- | --- |
| `claim` | Check eligibility/attempt budget, increment attempts, assign owner and lease; increment a token where the existing module requires it. |
| `renew` | Extend ownership after matching the original owner/token guard. |
| `nack` | Record failure, clear ownership, and schedule retry or a terminal outcome. |
| `complete`, `fail` | Record the terminal state and completion time. |
| `release` | Return unhandled work and refund its claimed attempt. |
| `requeue` | Reset a dead message's attempts and error. |
| `expire` | Terminate expired work that exhausted its attempts. |

Claim and expiry skip locked rows. Other transitions wait and recheck ownership
after the competing transaction commits. A transition that finds no eligible
row returns no rows, preserving each repository's existing lost-claim errors.
Expiry remains permissive for jobs: a matching owner/token can finalize after
expiry if no successor claimed the job. The function does not add strict expiry.

Module policy is fixed in its typed SQL adapter, rather than accepted from the
Go caller. It maps status names and preserves existing clock, guard and terminal
timestamp rules. Inbox/outbox callers still decide dead-letter classification;
their Nack honors the supplied `dead` flag. Jobs and idempotency enforce
exhaustion in their persisted Nack transition. Idempotency uses the shared,
overflow-capped `dbtx.retry_delay` function for its existing exponential delay.

Selection stays local so it can use each table's indexes and ordering rules.
Inbox retains aggregate selection/locking; idempotency retains payload matching,
response replay, claim creation and checkpoints; jobs retain archive and key
retention queries. Typed adapters write domain responses/TTL in the same
transaction as the shared transition. Handler savepoints, business writes and
Ack/Nack transaction boundaries are unchanged.

Grant, renewal and ownership guards use the shared functions sourced from
[`postgres/lease/ownership`](../lease/ownership/README.md). Resource leases use
the same primitives with strict expiry. Retry workers retain their existing
expiry policies. Outcome and ownership release are updated together to preserve
constraints coupling status, completion and ownership. Idempotency's atomic
insert-or-claim/result-replay protocol remains local.

Advisory locks remain a separate transaction-scoped PostgreSQL primitive.

## Install and upgrade

For a new installation, apply the module's `repository.Schema`. It includes the
shared functions, base tables, lifecycle view and typed adapter. Constructors do
not execute DDL. If copying SQL files into migrations, install
`repository/shared_retry.sql`, then `repository/schema.sql`, then
`repository/retry.sql`.

For an existing installation, stop workers, apply `repository.Migrations` in
one transaction, then deploy the new workers. This additive migration installs
functions and views without rewriting work rows, keys, attempt counters or
ownership histories. It can be applied repeatedly. Do not reapply the full base
schema where its enum creation is not repeatable.

```go
tx, err := db.BeginTx(ctx, nil)
if err != nil {
    return err
}
defer tx.Rollback()
if _, err := tx.ExecContext(ctx, repository.Migrations); err != nil {
    return err
}
return tx.Commit()
```

The functions and views use invoker permissions. A separate worker role needs
`SELECT` and `UPDATE` on its lifecycle view as well as the existing table
permissions, schema usage and function execution. For example:

```sql
GRANT SELECT, UPDATE ON dbtx.jobs_retry_state TO app_worker;
```

Underlying row-level security remains in effect through `security_invoker`
views. The view feature requires PostgreSQL 15+; these modules already require
PostgreSQL 18+ for `uuidv7()`. Migration users need permission to create views
and functions. Roll back application code if necessary; the added functions
and views can remain, and older queries still use the same rows and columns.

## Source, generation and verification

`postgres/retry/schema.sql` owns retry transitions;
`postgres/lease/ownership/schema.sql` owns shared ownership primitives. The
bundle generator copies both into the four independently released modules so
their existing dependency pins remain usable. Generated SQL is distribution
material, not a second implementation. Schema-bundle tests detect drift.

From the repository root:

```sh
go generate ./postgres/retry
sqlc generate -f postgres/inbox/repository/sqlc.yaml
sqlc generate -f postgres/outbox/repository/sqlc.yaml
sqlc generate -f postgres/jobs/repository/sqlc.yaml
sqlc generate -f postgres/idempotent/repository/sqlc.yaml
go test -race ./postgres/retry/...
```

Run `go test -race ./...` and `go vet ./...` inside each of the four nested
modules. Integration tests use temporary PostgreSQL containers. Migration tests
install the legacy schema, seed an active claim, apply the migration twice,
verify unchanged stored state, and exercise retry/takeover with stale-owner
rejection. Shared tests cover attempt limits, backoff, rollback, expiry,
concurrent locks, row security and unusual text keys.

Dynamic SQL and the additional row-read step add per-transition overhead. Queue
selection still uses the original static SQL and indexes; measure throughput
for your workload before treating this refactor as a performance improvement.
