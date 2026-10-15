# Composing PostgreSQL leases with durable modules

The lease module coordinates a resource. Idempotent requests, jobs, inbox messages,
and outbox events retain their own durable identities and state machines. This
review checks the existing public APIs rather than migrating any tables.

The executable checks live in the separate
[integration test module](../postgres/lease/integration/composition_test.go), so
ordinary lease users do not acquire dependencies on all the durable modules.

## Compatibility map

| Composition | Works through current APIs? | Guarantee / boundary |
| --- | --- | --- |
| Fenced transaction + outbox enqueue | Yes | Business writes and event insertion commit or roll back together. |
| Activity finalization + outbox enqueue | Yes | External work stays outside the transaction; finalization writes and event insertion are atomic. |
| Fenced transaction + inbox enqueue | Yes | Incoming message persistence can join the transaction; acknowledge the broker after confirmed commit. |
| Fenced transaction + job submission | Yes, with the current sqlc repository | Business writes, job row, and job-key record join the same transaction. Workers run later outside it. |
| Job handler + resource Activity/RunInFencedTx | Yes | Adds coordination across different jobs for one resource; it does not automatically validate the job claim. |
| Idempotent step + outbox enqueue | Yes, independently of generic leases | Request outcome, database effects, and outbox event can commit together. |
| Fenced callback + `idempotent.Do` | No | `Do` rejects the existing transaction with `idempotent.ErrTxInContext`. |
| Inbox/outbox dequeue handler + Activity or fenced helper | No | The handler already runs in a transaction; the lease module returns `lease.ErrTxInContext`. |
| Outer Activity + `idempotent.Do` | Callable, but not an atomic resource guard | The inner request transaction does not validate/lock the outer resource lease. |
| Shared ownership table / replacing module claims | Not implemented | Requires transaction-aware ownership primitives and an explicit migration. |

Enqueue and submit are producer operations; dequeue and worker execution have
different transaction contracts. A lease is not needed merely to enqueue durable
work, and ordinary workers can already run on multiple nodes.

## Dependency versions

The composition test pins `lease/v0.0.1`, `idempotent/v0.0.6`, and
`jobs/v0.0.1`. The current inbox and outbox APIs require published source
revisions newer than their existing tags:

- Inbox: `v0.0.2-0.20261014045754-26e6d0c3309d`; `v0.0.1` lacks the
  current `repository` package.
- Outbox: `v0.0.5-0.20261014045754-26e6d0c3309d`; `v0.0.4` lacks
  `RequireTx`.

These are existing published commits, not new releases created by this review.
Use the integration module's `go.mod` as the exact dependency reference. A local
workspace verifies checkout versions separately; it does not fix an older tagged
API for consumers.

## Wire the same transaction context

```go
atomic := dbtx.New(db)
atomic.SetID("billing")
locker, err := lease.New(lease.Config{DB: atomic, Namespace: "billing"})
if err != nil { return err }

outRepo := outboxrepository.New(db)
outRepo.DB = atomic
publisher := outbox.New(outRepo)
publisher.RequireTx = true

jobRepo := jobsrepository.New(db)
jobRepo.DB = atomic
submitter := jobs.NewSubmitter(jobRepo, jobs.SubmitConfig{})
```

`outboxrepository` and `jobsrepository` alias each module's `repository` import.
Their concrete repositories embed `*dbtx.DB`, allowing setup-time injection of the
shared instance. Do this before sharing clients across goroutines. Align inbox
and idempotent repositories in the same way when needed. Default constructors
also join by the default dbtx ID, but explicit injection preserves custom IDs and
query wrappers.

Use the same actual database and callback context for all repositories. With a
mismatched context ID, an enqueue/submit can execute on the pool and commit
independently. `publisher.RequireTx` detects an outbox enqueue outside its expected
transaction; jobs submission has no corresponding guard, so wiring matters.

## Business change, event, and job in one short transaction

```go
err := locker.RunInFencedTx(ctx, "order:"+orderID,
    func(txCtx context.Context, held lease.Lease) error {
        if err := orders.MarkPaid(txCtx, orderID, receiptID); err != nil {
            return err
        }
        _, err := publisher.Enqueue(txCtx, outbox.EnqueueParams{
            AggregateType: "Order",
            AggregateID: orderID,
            EventType: "OrderPaid",
            Payload: jsontext.Value(eventJSON),
        })
        if err != nil { return err }
        _, err = submitter.Submit(txCtx, jobs.SubmitInput{
            IdempotencyKey: "fulfil-order:" + orderID,
            Request: json.RawMessage(jobJSON),
        })
        return err
    },
)
```

Here `orders` uses the shared dbtx instance, and event/job JSON is validated
application data. Commit makes all three changes visible; rollback removes all
three, including a newly created job-key record. Job submission still compares
payloads for repeated keys. An existing job returned from `Submit` predates this
transaction and is not undone by its rollback.

This does not deduplicate a repeated business change or outbox event. Reconcile
unknown commits using a stable operation receipt, or use the existing idempotent
transaction path when request-response replay is the primary requirement.

For external payment work, use `locker.Activity`, perform the idempotent provider
call, and put the block above inside `held.RunInFencedTx`. Do not acquire the same
key again. Provider success can precede failed finalization; reconcile that gap.

## Jobs: guard resource ownership and job ownership separately

A job token orders claims of that job. A resource lease token orders acquisitions
of that resource. Two different jobs can have token 1 while targeting the same
account; do not use job tokens as a common per-account fencing sequence.

```go
handler := func(jobCtx context.Context, job jobs.Job) (json.RawMessage, error) {
    if job.WorkerID == nil { return nil, errors.New("job is not claimed") }
    accountID, err := accountFromJob(job.Request)
    if err != nil { return nil, jobs.Permanent(err) }
    err = locker.Activity(jobCtx, "account:"+accountID,
        func(activityCtx context.Context, held lease.Lease) error {
            // Optional external work here needs stable idempotency keyed by
            // the logical job/operation, rather than the resource lease token.
            return held.RunInFencedTx(activityCtx, func(txCtx context.Context) error {
                // This repository operation joins txCtx. It checks the current
                // job owner/token and locks the job row until this transaction ends.
                _, err := jobRepo.Heartbeat(txCtx, jobs.HeartbeatParams{
                    ID: job.ID,
                    WorkerID: *job.WorkerID,
                    FencingToken: job.FencingToken,
                    LeaseSeconds: 30,
                })
                if err != nil { return err }
                return applyJobOnce(txCtx, job.ID.String(), job.Request)
            })
        },
    )
    if err != nil { return nil, err }
    return json.RawMessage(`{"applied":true}`), nil
}
```

`accountFromJob` validates routing data. `applyJobOnce` must store a stable operation
receipt, reject mismatched payload reuse, and apply business writes in the same
transaction. Configure the worker's lease duration consistently with the heartbeat
above. Every resource-guarded job transaction follows the same lock order: resource
lease row, then job claim row, then application rows.

The built-in worker still records completion after the handler returns. A crash
in that gap can redeliver the job; the operation receipt prevents repeating its
database effect. Do not manually call `Complete` inside the handler and then ask
the worker to complete the same claim again.

This heartbeat is an atomic job-ownership guard for the database write phase,
using the concrete repository's transaction-aware implementation. It retains jobs'
existing policy: an expired but unreclaimed job claim is still accepted. It is
not strict generic lease expiry, and it cannot protect an earlier external call.
A fresh resource lease alone would not stop a stale job attempt from publishing.

Under contention, a waiting Activity uses worker capacity while both heartbeat
loops run. A TryActivity returns `ErrContended`; the job handler should return a
retryable error (optionally `jobs.RetryAfter`) unless skipping is the intended
terminal outcome. Returning nil on contention would mark the job successful
without doing its work. Resource leases do not introduce FIFO ordering between
jobs. Validate application revisions when a delayed job could carry obsolete input.

## Idempotent requests: keep the existing atomic outcome path

```go
response, err := idp.Do(ctx, requestKey,
    func(stepCtx context.Context, p idempotent.Params) (*idempotent.Result, error) {
        if err := orders.Apply(stepCtx, p.Request.Data); err != nil { return nil, err }
        if _, err := publisher.Enqueue(stepCtx, event); err != nil { return nil, err }
        return &idempotent.Result{
            Response: &idempotent.Response{
                Status: string(idempotent.StatusCompleted),
                Data: []byte(`{"ok":true}`),
            },
        }, nil
    },
    request,
)
```

Idempotent owns this short transaction. Response replay suppresses repeated handler
execution; adding a generic resource Activity around it would not make the inner
transaction check that resource's owner/token/expiry. The integration test forces
outer resource expiry, commits the inner idempotent transaction, then detects
lease loss: the outer call errors even though the inner effect is durable.

Current public APIs cannot atomically combine both independent guards through
nested execution wrappers. A future explicit `held` guard that joins a caller-owned
transaction would be required, with lock ordering, transaction ownership, and
timeout semantics designed deliberately. Do not detach context to evade transaction
checks or import generated queries to reconstruct private ownership credentials.

## Inbox and outbox handlers already own their transactions

Enqueue can compose with fenced transactions. Existing `Dequeue` callbacks cannot
start another Activity or fenced transaction, because they retain the message row
lock and handler transaction. Wrapping a long external call in a nested Activity
would not release that enclosing transaction.

When a lease error is returned from a handler, the modules roll back handler
writes and record a retry/dead-letter transition. Handler errors are not necessarily
returned by `Dequeue`: inbox reports them through `OnError`, while an outbox handler
needs its own logging/metrics. Returning nil after ignoring the rejection would
acknowledge work that was never performed.

Use ordinary concurrent dequeue workers for short transactional processing. Long
external work requires a separately designed execution path that preserves the
message's durable claim and acknowledgement, or a durable handoff to jobs/outbox.
The inbox/outbox semantics are not replaced by resource leases.

## Verified behavior and remaining boundaries

The integration suite checks committed resource contents and queue/request state,
not only lease metadata:

- Fenced business writes and outbox events commit/roll back together.
- Activity finalization can atomically enqueue an outbox event.
- Inbox enqueue joins the transaction and retains message-ID deduplication.
- Job submission and its idempotency key commit/roll back with business writes.
- Custom context IDs work when repositories share the unit of work; mismatched
  IDs can make submission commit independently, while outbox RequireTx rejects
  that wiring.
- Jobs can guard a resource and their own claim together; stale job claims fail
  despite obtaining a fresh resource lease, and operation receipts survive replay.
- Idempotent response replay still executes its handler once and emits one event.
- Nested transaction-owning execution APIs reject composition before callbacks.
- An outer Activity does not automatically fence an inner idempotent transaction.

Schema installation, broker acknowledgement timing, provider idempotency, receipt
retention, queue ordering, and PostgreSQL failover history remain application or
existing-module responsibilities. Shared lease storage and runtime refactoring
are not part of these checks. See the
[shared-table limitations](postgres-lease-module-reuse.md) for those future options.
