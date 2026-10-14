# PostgreSQL lease integration examples

These are client-side Go integration scenarios for the implemented `postgres/lease` module. Application tables, clients, and reconciliation functions are illustrative; snippets are not standalone programs. See the [module README](../postgres/lease/README.md) and [compiled usage examples](../postgres/lease/examples_test.go) for the shipped API. Schema installation happens through application migrations before startup.

The central contract is: **activities can overlap after lease expiry; guarded PostgreSQL commits reject expired owners.** A lease does not stop a paused process or undo an external request. Use the scenarios below to decide where coordination, atomic ownership checks, and logical-operation deduplication belong.

## Setup once, share across services

```go
import (
    "context"
    "database/sql"
    "errors"
    "fmt"
    "time"

    "github.com/alextanhongpin/dbtx"
    "github.com/alextanhongpin/dbtx/postgres/lease"
)

// db is an opened and pinged *sql.DB owned by the application.
atomic := dbtx.New(db)
locker, err := lease.New(lease.Config{
    DB:                 atomic,
    Namespace:          "billing",
    LeaseDuration:      30 * time.Second,
    TransactionTimeout: 2 * time.Second,
})
if err != nil {
    return err
}
```

Passing the same `*dbtx.DB` to the locker and repositories makes database identity and transaction-context ID explicit. All cooperating services use the same PostgreSQL database, namespace, and resource-key convention. Construction does not install schema or start a worker. Imports above are a shared reference; individual snippets use only the imports they need.

## 1. Short database update: RunInFencedTx

```go
func ReserveInventory(
    ctx context.Context,
    locker *lease.Locker,
    atomic *dbtx.DB,
    sku string,
    quantity int,
) error {
    if quantity <= 0 {
        return errors.New("quantity must be positive")
    }
    return locker.RunInFencedTx(ctx, "inventory:"+sku,
        func(txCtx context.Context, held lease.Lease) error {
            result, err := atomic.DBTx(txCtx).ExecContext(txCtx, `
                UPDATE inventory
                SET available = available - $2, last_fence = $3
                WHERE sku = $1 AND available >= $2
            `, sku, quantity, int64(held.Token()))
            if err != nil {
                return err
            }
            changed, err := result.RowsAffected()
            if err != nil {
                return err
            }
            if changed != 1 {
                return errors.New("inventory missing or insufficient")
            }
            return nil
        },
    )
}
```

Ownership validation and this update run in the same short transaction. The helper commits on nil and rolls back on error or panic. Repositories must use the callback's `txCtx`. `last_fence` is recorded for auditing here; the helper's locked ownership check supplies enforcement. PostgreSQL tokens fit positive `int64`, so the conversion is valid under the module contract.

## 2. External payment: Activity, then fenced finalization

```go
type PaymentGateway interface {
    Charge(context.Context, ChargeRequest) (Receipt, error)
}

type ChargeRequest struct {
    OrderID        string
    AmountCents    int64
    IdempotencyKey string
}

type Receipt struct { ID string }

func ChargeOrder(
    ctx context.Context,
    locker *lease.Locker,
    atomic *dbtx.DB,
    gateway PaymentGateway,
    orderID string,
    amountCents int64,
) error {
    return locker.Activity(ctx, "order:"+orderID,
        func(activityCtx context.Context, held lease.Lease) error {
            receipt, err := gateway.Charge(activityCtx, ChargeRequest{
                OrderID:        orderID,
                AmountCents:    amountCents,
                IdempotencyKey: "charge-order:" + orderID,
            })
            if err != nil {
                return err
            }

            // Validate this existing lease; do not reacquire.
            return held.RunInFencedTx(activityCtx,
                func(txCtx context.Context) error {
                    result, err := atomic.DBTx(txCtx).ExecContext(txCtx, `
                        UPDATE orders
                        SET payment_id = $2, paid_at = CURRENT_TIMESTAMP,
                            last_fence = $3
                        WHERE id = $1
                    `, orderID, receipt.ID, int64(held.Token()))
                    if err != nil {
                        return err
                    }
                    changed, err := result.RowsAffected()
                    if err != nil {
                        return err
                    }
                    if changed != 1 {
                        return errors.New("order missing")
                    }
                    return nil
                },
            )
        },
    )
}
```

The payment call holds no transaction or database connection. `Activity` renews ownership automatically. The finalization transaction is brief and rejects stale ownership before invoking its callback.

The payment gateway in this example supports idempotency, not fencing. Its key identifies a logical payment and stays the same across lease generations. If the charge succeeds but ownership is lost before finalization, reconciliation or a retry with the same idempotency key retrieves the payment result. A lease cannot atomically commit an external charge and a PostgreSQL update.

The order amount must come from validated application data; the same idempotency key must not be reused for different payment intents.

## 3. External service that enforces fencing

```go
type FencedWriter interface {
    // Atomically rejects a token older than the watermark for resourceKey.
    // Also deduplicates operationID separately from the fencing token.
    Put(ctx context.Context, resourceKey string, token uint64,
        operationID string, payload []byte) error
}

func PublishDocument(
    ctx context.Context,
    locker *lease.Locker,
    writer FencedWriter,
    documentID string,
    revisionID string,
    payload []byte,
) error {
    key := "document:" + documentID
    return locker.Activity(ctx, key,
        func(activityCtx context.Context, held lease.Lease) error {
            return writer.Put(activityCtx, key, held.Token(), revisionID, payload)
        },
    )
}
```

This receiving service implements the fencing contract explicitly. It compares tokens for the same authority, namespace, and key, permits the intended equal-token operations, and deduplicates retries by operation ID. Ordinary HTTP APIs and object stores do not acquire this behavior merely because a token is sent in a header.

A watermark rejects an older token only after a higher token has been accepted. It does not independently know that a lease expired. A remote check of current ownership followed by a separate write also has a race; strict active-ownership protection requires an atomic resource-side protocol, such as the same PostgreSQL transaction guard.

## 4. Scheduled maintenance: skip when another worker is active

```go
func RefreshReport(
    ctx context.Context,
    locker *lease.Locker,
    compute func(context.Context) ([]byte, error),
    save func(context.Context, []byte) error,
) (completed bool, err error) {
    err = locker.TryActivity(ctx, "report:daily",
        func(activityCtx context.Context, held lease.Lease) error {
            // Potentially slow computation runs outside a transaction.
            report, err := compute(activityCtx)
            if err != nil {
                return err
            }
            return held.RunInFencedTx(activityCtx,
                func(txCtx context.Context) error {
                    // save joins txCtx through the same dbtx.DB.
                    return save(txCtx, report)
                },
            )
        },
    )
    if errors.Is(err, lease.ErrContended) {
        return false, nil
    }
    return err == nil, err
}
```

Try acquisition skips immediately on contention. With ordinary acquisition, use the caller's context deadline to bound waiting and total execution. A separate acquisition-only waiting budget would need another API option if applications require it.

The same distinction applies to manual acquisition and short transactions:

```go
held, err := locker.TryAcquire(ctx, "document:123")
// On ErrContended, held is nil; no renewal or cleanup is needed.
// On success, manage and release held as in the manual example below.

err = locker.TryRunInFencedTx(ctx, "inventory:sku-123",
    func(txCtx context.Context, held lease.Lease) error {
        return updateInventory(txCtx, held.Token())
    },
)
// On ErrContended, updateInventory is never called.
```

Use `Acquire`, `Activity`, or `RunInFencedTx` to wait instead. Immediate methods still make a PostgreSQL request and can encounter pool/network latency or errors; they do not wait for a competing lease or coordination row lock.

## 5. Manual acquisition for a custom lifecycle

```go
func OneManualOperation(
    ctx context.Context,
    locker *lease.Locker,
    writer FencedWriter,
    payload []byte,
) (err error) {
    held, err := locker.Acquire(ctx, "document:123",
        lease.WithLeaseDuration(10*time.Second),
    )
    if err != nil {
        return err
    }
    defer func() {
        cleanupCtx, cancel := context.WithTimeout(
            context.WithoutCancel(ctx), 2*time.Second,
        )
        defer cancel()
        err = errors.Join(err, held.Release(cleanupCtx))
    }()

    // No automatic renewal with Acquire. Keep this operation short.
    operationCtx, cancel := context.WithTimeout(ctx, time.Second)
    defer cancel()
    if err := writer.Put(operationCtx, "document:123", held.Token(),
        "revision:42", payload); err != nil {
        return err
    }
    select {
    case <-held.Done():
        return held.Err()
    default:
        return nil
    }
}
```

Manual users can call `held.Renew(ctx)` and observe `held.Done()`/`held.Err()`, but own renewal scheduling and operation cancellation. `Activity` is the default for work that needs heartbeats. Checking `Done` is an observation, not an atomic resource guard; the writer above still enforces fencing.

## 6. Stop-the-world pause: stale work resumes after takeover

```go
func BuildSnapshot(
    ctx context.Context,
    locker *lease.Locker,
    atomic *dbtx.DB,
    snapshotID string,
    compute func(context.Context) ([]byte, error),
) error {
    return locker.Activity(ctx, "snapshot:"+snapshotID,
        func(activityCtx context.Context, held lease.Lease) error {
            result, err := compute(activityCtx)
            if err != nil {
                return err
            }

            // compute may return after a long pause, even if ownership expired.
            // The database guard, rather than a local check, protects publication.
            return held.RunInFencedTx(activityCtx,
                func(txCtx context.Context) error {
                    _, err := atomic.DBTx(txCtx).ExecContext(txCtx, `
                        INSERT INTO snapshots (id, payload, last_fence)
                        VALUES ($1, $2, $3)
                        ON CONFLICT (id) DO UPDATE
                        SET payload = EXCLUDED.payload,
                            last_fence = EXCLUDED.last_fence
                    `, snapshotID, result, int64(held.Token()))
                    return err
                },
            )
        },
    )
}
```

Consider a 30-second lease:

| Event | Node A | Node B |
| --- | --- | --- |
| A acquires | Token 41; starts computing | Cannot acquire the same key |
| A pauses for longer than the lease | Renewal and local cancellation processing stop | Acquires token 42 after database expiry |
| B publishes | Still paused | Guarded transaction writes its result |
| A resumes | Computation may continue; its guard rejects publication | Remains the current owner or has released |

Even if B has not acquired, an expired A must fail the guard. Even if B has already released, A's old token must fail. A may see a cancellation cause or `ErrLeaseLost`, depending on which detection wins; neither permits publication. Never reacquire a new lease merely to publish A's old result without revalidating the input/revision that produced it.

If A had already entered a valid fenced transaction before pausing, the row lock delays B's takeover until commit/rollback. PostgreSQL-side transaction/statement/idle-transaction timeouts must bound that case; a client timer cannot execute while the client is paused. Expiry alone does not release a transaction row lock.

## 7. Multiple nodes: contention is not durable deduplication

Each replica constructs its own pool, `dbtx.DB`, and locker using the same PostgreSQL authority and namespace. Node IDs may appear in logs; do not put node IDs in a shared resource key.

```go
// Executed by every node receiving the same event.
func ApplyEvent(
    ctx context.Context,
    locker *lease.Locker,
    atomic *dbtx.DB,
    eventID string,
    accountID string,
    delta int64,
) error {
    return locker.RunInFencedTx(ctx, "account:"+accountID,
        func(txCtx context.Context, held lease.Lease) error {
            // applied_events.event_id has a unique constraint.
            inserted, err := atomic.DBTx(txCtx).ExecContext(txCtx, `
                INSERT INTO applied_events (event_id, account_id, delta)
                VALUES ($1, $2, $3)
                ON CONFLICT (event_id) DO NOTHING
            `, eventID, accountID, delta)
            if err != nil {
                return err
            }
            count, err := inserted.RowsAffected()
            if err != nil {
                return err
            }
            if count == 0 {
                var recordedAccount string
                var recordedDelta int64
                err := atomic.DBTx(txCtx).QueryRowContext(txCtx, `
                    SELECT account_id, delta FROM applied_events WHERE event_id = $1
                `, eventID).Scan(&recordedAccount, &recordedDelta)
                if err != nil {
                    return err
                }
                if recordedAccount != accountID || recordedDelta != delta {
                    return errors.New("event ID reused with different payload")
                }
                return nil
            }
            updated, err := atomic.DBTx(txCtx).ExecContext(txCtx, `
                UPDATE accounts
                SET balance = balance + $2, last_fence = $3
                WHERE id = $1
            `, accountID, delta, int64(held.Token()))
            if err != nil {
                return err
            }
            count, err = updated.RowsAffected()
            if err != nil {
                return err
            }
            if count != 1 {
                return errors.New("account missing")
            }
            return nil
        },
    )
}
```

The receipt and effect commit together. Node A applies the event; waiting node B can acquire afterward but finds the receipt and performs no second update. A lease alone would only serialize two executions. Stable event IDs, unique constraints, and matching-payload validation handle duplicate deliveries and ambiguous commit retries.

For periodic work, `TryActivity` only suppresses overlapping runs: a node arriving after release can run again. If a report should complete once per date, use a stable key such as `report:2026-10-14` and a durable completion record checked/finalized under the guard. If all nodes must eventually perform distinct work, use waiting acquisition or a durable job queue rather than treating contention as successful completion.

## 8. Network partition, database outage, or exhausted pool

```go
err := BuildSnapshot(ctx, locker, atomic, snapshotID, compute)
switch {
case err == nil:
    // The guarded publication succeeded.
case errors.Is(err, lease.ErrLeaseLost):
    // Discard this attempt's output; a fresh attempt must reread its inputs.
case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
    // Caller cancellation or an expired operation budget; inspect causes/outcomes.
default:
    // Record the wrapped database/network failure and use bounded retry policy.
}
```

If renewal cannot reach PostgreSQL, the conservative local deadline is not extended. The activity context is cancelled at that deadline once the process can run; a late heartbeat response cannot revive the lease. Another node may take over, and old guarded writes must fail. Until expiry, other nodes may remain contended even after A loses connectivity.

This switch describes failure categories, not permission to replay effects. Check transaction outcomes and durable operation receipts before retrying. An external request may have succeeded despite a timeout. A successful finalization followed by lost ownership/cleanup failure may also return an error after its effect committed.

Keep transaction durations bounded and leave pool capacity for renewal. A pool fully occupied by long transactions can cause otherwise healthy activities to lose leases. Do not move coordination reads to a lagging replica or use node-local time as the authoritative expiry check.

## 9. Cancellation, shutdown, panic, and process death

```go
// stop is called by the application's shutdown handler.
workCtx, stop := context.WithCancel(context.Background())
defer stop()

err := locker.Activity(workCtx, "export:123",
    func(activityCtx context.Context, held lease.Lease) error {
        for {
            if err := context.Cause(activityCtx); err != nil {
                return err
            }
            finished, err := exportNextChunk(activityCtx)
            if err != nil {
                return err
            }
            if finished {
                return nil
            }
        }
    },
)
```

`exportNextChunk` is an application function that must honor context and use resource enforcement/idempotency for its effects. Context checks help stop work but cannot make the next external call atomic with lease validation.

The application stops admitting new work, cancels activities, waits for callbacks to exit, and only then closes its pool. The helper attempts bounded release using a cleanup context that survives cancellation. A callback ignoring cancellation can keep running; the helper cannot force it to stop. Avoid detached goroutines that retain a lease after their callback returns; join child work before returning.

On callback panic, the helper attempts cleanup and re-panics; transaction work rolls back. On process death, defers do not run: a raw/activity lease expires, while open PostgreSQL transactions must terminate through disconnect or server-side timeout before row locks are freed. Expiry enables recovery, not automatic re-execution. Use a durable job/inbox/outbox mechanism when work must survive process loss.

## 10. Commit succeeded, rollback occurred, or result is unknown

The implemented outcome API distinguishes effects of one transaction:

```go
err := locker.RunInFencedTx(ctx, "order:"+orderID, updateOrder)
if err != nil {
    var outcome *lease.TxOutcomeError
    if errors.As(err, &outcome) {
        switch outcome.Outcome {
        case lease.TxCommitted:
            // Commit was confirmed; cleanup failed. Do not replay the update.
            return reconcileCleanup(ctx, orderID)
        case lease.TxUnknown:
            // COMMIT response was lost. Query a durable operation receipt.
            return reconcileOperation(ctx, operationID)
        case lease.TxRolledBack:
            // Confirmed no transaction effect; apply bounded retry policy
            // only if the underlying error is retryable.
        }
    }
    return err
}
```

`updateOrder`, `reconcileCleanup`, and `reconcileOperation` are application functions. The outcome refers to one transaction, not the whole activity or its external effects. A callback can also fail before any transaction begins. An existing-lease finalization can commit successfully and later its outer activity can fail; use a durable receipt as the final source of truth.

Never automatically retry an entire callback solely because it returned a non-nil error. A transaction helper must not retry user callbacks internally after an unknown outcome. Cleanup retries use the original ownership ID and must not clear a successor's lease.

## 11. Long workflows: checkpoint, resume, and publish an outbox event

```go
return locker.Activity(ctx, "workflow:"+workflowID,
    func(activityCtx context.Context, held lease.Lease) error {
        state, err := loadWorkflow(activityCtx, workflowID)
        if err != nil {
            return err
        }
        result, err := runExternalStep(activityCtx,
            "workflow:"+workflowID+":step:"+state.StepID)
        if err != nil {
            return err
        }
        return held.RunInFencedTx(activityCtx,
            func(txCtx context.Context) error {
                // Verify the expected step/revision before updating it.
                if err := checkpoint(txCtx, workflowID, state.Revision, result); err != nil {
                    return err
                }
                return enqueueOutbox(txCtx, workflowID, state.StepID, result)
            },
        )
    },
)
```

These application repositories share the locker's `dbtx.DB`; checkpoint and outbox insertion commit together. The external step's logical idempotency key remains stable when a successor resumes the workflow. Revision checks reject results based on obsolete inputs. The outbox publisher performs delivery afterward with its own at-least-once/deduplication contract.

Lease tokens increase per acquisition, not per workflow step; several checkpoints by the same owner use the same token. After takeover, load the durable checkpoint again. Do not hold one transaction open for the workflow or infer durable workflow completion from lease release.

## 12. Multiple resources, tenants, and nested acquisition

```go
// IDs are validated canonical IDs; encode arbitrary segments unambiguously.
key := fmt.Sprintf("tenant:%s:account:%s", tenantID, accountID)
return locker.RunInFencedTx(ctx, key, updateAccount)
```

Different keys can run concurrently. Services protecting the same resource must use identical canonical keys and namespace. Tenant scoping prevents accidental contention but does not replace application authorization. A different namespace or database means a different coordination domain; tokens across keys/domains are not comparable.

Do not nest `locker.Activity`/`RunInFencedTx` for the same key; waiting reacquisition waits on yourself and try reacquisition returns contention. Use `held.RunInFencedTx` for an existing activity. Do not nest fenced transactions or invoke activities from a transaction callback; the helpers reject an existing transaction context.

Atomic multi-key acquisition is outside v1. For a two-account invariant, choose one shared invariant key used by all relevant writers and perform both updates in one short transaction, or use PostgreSQL row locking with a consistent ordering. Locking account A alone does not protect account B; introducing a transfer-only key does not serialize unrelated writers that ignore it. Consistent ordering helps avoid deadlocks but is not a multi-key acquisition guarantee.

Ordinary SQL can bypass the lease table. Every writer participating in an invariant must follow the same guard protocol; retain SQL constraints and row-level safeguards for invariants that should hold regardless of client behavior. A locker configured with one database cannot atomically guard writes through a repository connected to another.

## 13. Object storage: conditional writes instead of assumed fencing

```go
type VersionedStore interface {
    Read(context.Context, string) (body []byte, version string, err error)
    PutIfVersion(context.Context, string, string, []byte) error
}

return locker.Activity(ctx, "object:"+objectKey,
    func(activityCtx context.Context, held lease.Lease) error {
        body, version, err := store.Read(activityCtx, objectKey)
        if err != nil {
            return err
        }
        next, err := transform(body)
        if err != nil {
            return err
        }
        return store.PutIfVersion(activityCtx, objectKey, version, next)
    },
)
```

Here `store` satisfies the illustrative `VersionedStore` contract. A version conflict prevents overwriting a newer object version; a stale worker can still succeed if that version remains current. This does not enforce lease expiry. Use the provider's supported conditional-write protocol and reconcile conflicts by rereading/recomputing rather than blindly retrying stale payloads. Operations spanning object storage and PostgreSQL need a durable reconciliation protocol.

## 14. PostgreSQL failover, restore, and operational limits

Use one writable PostgreSQL authority. Promotion must fence the old primary and preserve acknowledged fencing history under the deployment's durability assumptions. If an asynchronously replicated promotion loses lease/counter commits, previously issued tokens can regress; the library cannot repair that with client cancellation. Do not promise failover-safe fencing independently of the PostgreSQL deployment.

Database restores, namespace deletion, and counter-row cleanup can also reset history. Quiesce workers and reconcile resource watermarks, or introduce an enforced epoch protocol before resuming. Keep generation rows during normal maintenance; an expired lease row is not disposable history. Fail explicitly at positive `bigint` exhaustion rather than wrapping tokens.

PostgreSQL expiry uses its clock; client budgets are conservative estimates. Clock jumps can change takeover timing. Same-database row locking still supplies atomic admission/commit serialization, but external token consumers require their own enforcement. Increasing TTL trades quicker crash recovery for more renewal slack; it never eliminates arbitrary pauses or partitions. Avoid extremely small TTLs and monitor renewal latency, contention, pool saturation, and transaction age without logging ownership credentials or unbounded resource-key labels.

## Scenario verification matrix

| Scenario | Required observable result |
| --- | --- |
| A pauses before finalization; B takes over | A's publication callback does not run; B's result remains. |
| A expires without any successor | A cannot finalize or renew its expired ownership. |
| B acquires and releases before A resumes | A cannot reuse its original lease or clear B's generation. |
| A pauses inside an admitted transaction | B cannot take over until transaction termination; server timeouts bound the pause. |
| Renewal reply arrives after local termination | No callback restart, deadline extension, or lease revival. |
| Multiple nodes receive one logical event | One durable effect and matching receipt; successors can acquire but do not duplicate it. |
| Try acquisition encounters a locked/absent-row race | Defined contention result; no logical contention wait or callback. |
| Release/renew/finalization race | Serialized or explicitly rejected mutations; successor remains intact. |
| Caller cancels while acquisition may have committed | No usable lease/callback returned; owner-qualified cleanup or expiry recovers it. |
| External request succeeds but response is lost | Stable idempotency key and reconciliation recover the result. |
| COMMIT succeeds but response is lost | Unknown outcome is surfaced; receipt lookup prevents blind replay. |
| Confirmed commit followed by release failure | Committed outcome remains distinguishable from rollback. |
| Callback ignores context or spawns children | No claim that cancellation stopped it; children must be joined. |
| Duplicate operation ID has different payload | Application rejects mismatched reuse. |
| Token sent to ordinary API or watermark-only service | No false claim of active-lease validation. |
| Separate keys/namespaces/databases | No implied shared ordering or cross-database atomicity. |
| Primary promotion loses acknowledged history | Deployment limitation is explicit; no assumed monotonicity. |

Use deterministic state/clock control and synchronization when extending the tests for these scenarios. A callback blocking on a channel while heartbeats still run does not simulate stop-the-world. The pause test must prevent renewal and local cancellation handling from progressing while PostgreSQL and another node continue.

## Developer experience findings

1. Both execution callbacks expose the same `Lease`, making `held.Token()` discoverable without context accessors or a token lookup by key.
2. An activity needs `held.RunInFencedTx(ctx, fn)` to finalize using its existing ownership. Calling `locker.RunInFencedTx` on the same key would try to reacquire and wait on itself. This method is part of the lease contract; it preserves the original token and does not release the activity lease.
3. The existing-lease helper needs one lifecycle owner: temporarily suspend automatic renewal, validate and lock ownership in a bounded transaction, then resume renewal after commit/rollback if ownership remains valid. It must handle expiry and cancellation without reviving a terminal lease. Manual lifecycle mutations racing with it must serialize or return a defined error.
4. Use the same `*dbtx.DB` for the locker and repositories. Avoid a constructor with a separate pool and a hidden transaction-context ID.
5. A returned error after commit must distinguish confirmed commit plus cleanup failure from rollback or an unknown commit outcome. The implemented `TxOutcomeError` lets clients inspect this with `errors.As`; never blindly retry all non-nil errors. Activities may also have completed an external effect before returning an error.
6. Use explicit waiting/try method pairs: `Acquire`/`TryAcquire`, `Activity`/`TryActivity`, and `RunInFencedTx`/`TryRunInFencedTx`. Manual acquisition is an advanced escape hatch. Configuration and TTL option names are documented in the module README.
7. Multiple nodes need durable operation IDs and receipts in addition to leases. Raw fencing-token checks, context checks, and per-node keys do not provide exactly-once execution.
8. `TxOutcomeError` and its outcome constants above are implemented. Durable receipts remain an application responsibility for reconciling unknown commits and external effects.

These application scenarios complement the implementation and its tests. They do not supply the application tables, external clients, or durable workflow infrastructure. The scenario matrix includes deployment and application responsibilities beyond the library test suite.
