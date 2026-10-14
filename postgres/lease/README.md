# PostgreSQL leases

`postgres/lease` coordinates renewable PostgreSQL ownership across services and
fences short database transactions. It is a separate Go module; the existing
`postgres/lock` advisory-lock API and durable jobs/idempotent/inbox/outbox modules
are unchanged.

Requires Go 1.27+, PostgreSQL 17+ (`transaction_timeout`), and `database/sql`.
Use one writable PostgreSQL authority with acknowledged fencing history preserved
across failover. Constructors do not install schema or own/close your pool.

## Setup

Apply `repository.Schema` through migrations. It installs the lease table and native resource ownership functions.
Rows retain fencing history permanently; do not purge expired rows. Reapplying
Schema does not migrate an existing table definition.

```go
import (
    "context"
    "errors"
    "time"

    "github.com/alextanhongpin/dbtx"
    "github.com/alextanhongpin/dbtx/postgres/lease"
)

// db is your opened and pinged *sql.DB. Share atomic with your repositories.
atomic := dbtx.New(db)
locker, err := lease.New(lease.Config{
    DB: atomic,
    Namespace: "billing",
    LeaseDuration: 30 * time.Second,
    TransactionTimeout: 2 * time.Second,
})
if err != nil { return err }
```

Keep `atomic`'s transaction ID unchanged after construction. The schema uses
`bigint` generations; `held.Token()` exposes a positive `uint64` that safely
converts to `int64` for PostgreSQL. Tokens order acquisitions of the same
namespace/key, not distinct resources. Never replace your lease's token with a
latest-token lookup.

## Waiting and immediate acquisition

| Waiting | Immediate | Callback |
| --- | --- | --- |
| `Acquire` | `TryAcquire` | Manual ownership; no automatic heartbeat |
| `Activity` | `TryActivity` | Outside a transaction, automatically renewed |
| `RunInFencedTx` | `TryRunInFencedTx` | Short ownership-guarded transaction |

Immediate methods return `ErrContended` without executing callbacks or waiting
for a competing coordination row lock. They still incur pool/network latency
and return transport errors separately. Waiting methods retry contention with
jitter and stop on other errors or context cancellation. The caller's context
bounds waiting and subsequent execution; there is no separate wait-only option.

```go
err := locker.TryActivity(ctx, "report:daily",
    func(activityCtx context.Context, held lease.Lease) error {
        return generateReport(activityCtx, held.Token())
    },
)
if errors.Is(err, lease.ErrContended) {
    return nil // Another node owns this resource; this caller chooses to skip.
}
return err
```

`generateReport` is application code. The resource key must identify the same
resource on every node; adding a node ID defeats shared coordination. A lease
suppresses overlapping active ownership, not sequential duplicate operations.
Use durable receipts/idempotency if each logical operation should commit once.

## Short database work

```go
err := locker.RunInFencedTx(ctx, "account:123",
    func(txCtx context.Context, held lease.Lease) error {
        _, err := atomic.DBTx(txCtx).ExecContext(txCtx, `
            UPDATE accounts SET updated_at = CURRENT_TIMESTAMP WHERE id = $1
        `, "123")
        return err
    },
)
```

The helper checks owner, generation, and expiry after locking the lease row,
runs the callback in a fresh `READ COMMITTED` transaction, then commits/rolls back
before releasing ownership. Use `txCtx` with the same `dbtx.DB` in repositories.
Do not perform external calls or long computation in this callback.

Once admitted, the transaction row lock prevents takeover until commit/rollback,
even if TTL elapses meanwhile. Client deadlines plus PostgreSQL statement, lock,
idle-transaction, and transaction timeouts bound this phase. Existing transaction
contexts for the configured DB or the default dbtx ID are rejected. A transaction
in another unknown context ID cannot be discovered; call outside all enclosing
transactions. Guarding one database does not guard writes in another.

## External work and fenced finalization

```go
err := locker.Activity(ctx, "order:123",
    func(activityCtx context.Context, held lease.Lease) error {
        // No PostgreSQL transaction/connection is held during this call.
        receipt, err := charge(activityCtx, "charge-order:123")
        if err != nil { return err }
        return held.RunInFencedTx(activityCtx, func(txCtx context.Context) error {
            _, err := atomic.DBTx(txCtx).ExecContext(txCtx, `
                UPDATE orders SET payment_id = $2 WHERE id = $1
            `, "123", receipt)
            return err
        })
    },
)
```

`charge` is an application/provider API using a stable logical-operation
idempotency key. `held.RunInFencedTx` validates existing ownership: it does not
reacquire or release the activity's lease. Never call `locker.RunInFencedTx` on
that same key from inside its activity; it would contend with itself.

Activity heartbeats run approximately every TTL/3. Each successful response must
arrive before the existing conservative deadline (request start + TTL minus a
TTL/6 safety margin). Failed renewals do not extend it; late replies cannot revive
a terminal lease. Loss independently cancels the activity context even while a
heartbeat request is in flight. Database drivers must honor contexts so background
requests can drain; the helper joins owned goroutines on return.

Finalization waits for a bounded automatic heartbeat already in flight, then
suppresses heartbeats while its guarded transaction runs. Conflicting manual
`Renew`, `Release`, and finalization calls return `ErrLeaseBusy` rather than
waiting on a possibly reentrant callback. `Token`, `Done`, and `Err` remain
available inside callbacks. Returning from a finalization past the lease budget
can leave the activity terminal even though that transaction committed.

## Pauses, cancellation, and outcomes

Activities can overlap after expiry. A stop-the-world pause prevents heartbeats
and cancellation handling; another node can take over while the old callback
later resumes. An expired/mismatched lease cannot enter fenced finalization,
even when no successor exists or the successor already released. Local context
checks alone cannot enforce this.

External APIs require their own protection. Token watermarks reject an older
token only after accepting a higher one; they do not independently detect expiry.
Idempotency, conditional object writes, and same-database fencing provide different
guarantees. None makes arbitrary external work atomic with a PostgreSQL commit.

On caller cancellation or ownership loss, Activity cancels the callback context
but waits for the callback to return. Join your own child goroutines. On panic,
helpers attempt cleanup and re-panic; guarded database work rolls back. Cleanup
uses a bounded context that survives caller cancellation. Process death relies
on expiry/server transaction termination rather than defers.

Inspect `TxOutcomeError` through `errors.As`:

- `TxCommitted`: COMMIT was confirmed, but automatic release failed. Do not replay.
- `TxRolledBack`: no commit was attempted after the failed admission/callback.
- `TxUnknown`: callback succeeded but COMMIT was not confirmed; reconcile a durable
  operation receipt before retrying.

Pre-acquisition failures do not carry a transaction outcome. An activity's error
can also follow an external effect or a successfully committed checkpoint. There
are no automatic user-callback retries.

## Manual leases and defaults

`Acquire`/`TryAcquire` own an expiry monitor but no renewal loop. Use `Renew(ctx)`
explicitly, and always attempt `Release` on a fresh bounded cleanup context.
`Done` closes once on release, detected loss, or conservative expiry; `Err` records
the first terminal cause. Release is locally terminal before its remote request,
can be retried after uncertain failure, and cannot clear a successor. Repeated
confirmed releases are harmless. Acquisition response loss uses owner-qualified
cleanup; TTL bounds the remote effects if cleanup also fails.

Zero config durations select defaults: lease 30s, transaction 2s, request 5s,
cleanup 2s, retry interval 50ms. The lease minimum is 6ms; other durations must be
at least 1ms. Fenced transaction timeout cannot exceed PostgreSQL's signed-int
millisecond range. Lease requests are also capped at TTL/3. A TTL override is
`lease.WithLeaseDuration(duration)`; practical production TTLs should allow for
network latency, pool contention, and pauses.

The module has no scheduling, durable work discovery, result replay, checkpoint
storage, aggregate ordering, or dead-letter policy. See the design documents:
[integration scenarios](../../docs/postgres-lease-examples.md) and
[module reuse limitations](../../docs/postgres-lease-module-reuse.md).

## Composition with durable modules

See [tested composition patterns](../../docs/postgres-lease-composition.md) for
outbox/inbox enqueue, transactional job submission, resource guards in job
handlers, and transaction nesting limitations. The separate
[integration module](integration/README.md) tests these combinations without
adding durable-module dependencies to the lease library.

## Generate and verify

Queries in [repository/queries/lease.sql](repository/queries/lease.sql) generate
[repository/postgres](repository/postgres) with sqlc v1.31.1. SQL includes the
PostgreSQL functions used for atomic nonwaiting acquisition and post-lock expiry
checks. The temporary advisory gate serializes first-row initialization; a hash
collision can cause brief spurious contention, never multiple ownership.

From this module directory:

```sh
make sqlc                         # sqlc v1.31.1 on PATH
# Or from the repository root: go tool sqlc generate -f postgres/lease/repository/sqlc.yaml
go test -race -count=1 -timeout 180s ./...
go vet ./...
```

Integration tests start PostgreSQL 17.4 through the existing Docker test helper.
They exercise real transactions and resource contents; deterministic clock/fault
tests cover local expiry and delayed replies. Root `go test ./...` does not include
this nested module. The root `make test` and `make sqlc` discover it automatically.
