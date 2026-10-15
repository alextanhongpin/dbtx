# Shared PostgreSQL ownership

`postgres/lease/ownership/schema.sql` is the source for shared ownership SQL.
The standalone lease repository uses it directly; the retry lifecycle bundles
the same SQL for jobs, inbox, outbox and idempotency. Each module keeps its own
rows, columns, indexes, Go API and transaction boundaries.

The functions operate on a table or an automatically updatable view exposing
`id`, `owner_id`, `token`, and `lease_expires_at`. The retry facade's aliases
`retry_key`, `lease_owner`, and `fencing_token` are also accepted. Optional
`namespace` supports resource composite keys; `status` supports domain guards.
Keys retain their native PostgreSQL types and values are bound parameters.

| Function | Responsibility |
| --- | --- |
| `ownership_grant` | Lock without waiting, reject live ownership, assign owner/expiry and advance a fencing generation where required. |
| `ownership_lock` | Lock the original row and check owner/token/status; optionally require unexpired ownership after acquiring the lock. |
| `ownership_renew` | Apply the same guard and extend expiry. |
| `ownership_release` | Apply the same guard and clear ownership on resource rows. |

These functions use the caller's transaction and invoker permissions. They do
not start transactions, commit, or acquire another ownership record. Locks last
until transaction end. Security-invoker views preserve underlying row security;
worker roles need view permissions as well as existing table permissions.

Resource leases require unexpired ownership for renewal/fenced admission and
retain per-resource generations permanently. Jobs keep per-job generations;
messages keep their existing owner guard. Idempotency retains its sequence,
payload comparison and atomic insert-or-claim protocol. That protocol remains
local because an absent row and result replay are domain decisions. Its retry,
renewal and checkpoint guards use the shared ownership functions.

Legacy retry workers may finalize or renew expired ownership when the original
owner/token still match. Sharing SQL preserves this policy. Resource leases
continue to reject expiry even without a successor.

Domain constraints can couple ownership with status and completion fields.
The shared retry transition checks ownership with `ownership_lock`, then clears
ownership and records the outcome in one UPDATE. Calling `ownership_release`
before updating status would violate these immediate constraints.

## Installation and generation

New installations apply their repository's `Schema`. Existing installations
stop workers and apply `repository.OwnershipMigrations` in one transaction,
then deploy new callers. For retry modules, `repository.Migrations` installs
the same upgrade. Migrations add functions/views without changing stored rows
or token histories, and may be applied repeatedly. Constructors execute no DDL.

For standalone leases, manual SQL installation is: this `schema.sql`, then
`lease/repository/schema.sql`, then `lease/repository/ownership.sql`.
The worker role additionally needs:

```sql
GRANT SELECT, UPDATE ON dbtx.resource_ownership TO app_worker;
```

From the repository root, `go generate ./postgres/retry` distributes ownership
and retry SQL to independently released modules. Generated bundles avoid a new
runtime dependency on the lease module. Bundle tests compare copies to this
source. Run race tests and vet in the root and each affected nested module.

The PostgreSQL tests cover strict/permissive expiry, expiry while waiting for a
row lock, stale owner/token rejection, rollback, composite keys, sequence
history after deletion, row security, and upgrades with live claims.
