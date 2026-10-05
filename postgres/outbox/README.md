# Postgres Outbox

A PostgreSQL-backed outbox implementation for reliable, transactional event publishing built on [dbtx](https://github.com/alextanhongpin/dbtx).

The outbox pattern lets you write business data and domain events in the same database transaction. Events are stored in `dbtx.outbox` and later dispatched by workers. The package handles visibility, retries with backoff, and a dead-letter emulation without external dependencies.

## Use cases

* **Transactional outbox** – enqueue domain events inside the same transaction as your aggregate writes. If the transaction rolls back, no event is emitted.
* **At-least-once delivery** – workers `Dequeue` visible messages, process them, and acknowledge by returning `nil`. Failures are retried with a backoff.
* **Retry with limits** – set `MaxAttempts` per message (default 10). Once a message runs out of attempts, it is dead.
* **Delayed visibility** – set `AvailableAt` to schedule future processing.
* **Dead letters** – wrap the handler error with `outbox.ErrDeadLetter` to stop further retries. Inspect, requeue and purge dead messages with `DeadLetters`, `Requeue` and `Purge`.

## Schema

The schema lives in [`repository/schema.sql`](repository/schema.sql) and is exported as `repository.Schema`. It is idempotent, so you can run it at startup or copy it into your migrations:

```go
_, err := db.ExecContext(ctx, repository.Schema)
```

A message moves through these statuses:

```
pending ──Dequeue──▶ processing ──nil──▶ done
   ▲                     │
   └──error, attempts────┤
      left               └──error, no attempts left / ErrDeadLetter──▶ dead
```

The package expects PostgreSQL with `uuidv7()` support (18+).

## Install

```bash
go get github.com/alextanhongpin/dbtx/postgres/outbox
```

## Quick start

```go
repo := repository.New(db) // db is a *sql.DB
o := outbox.New(repo)

// Enqueue inside the same transaction as your business write.
err := repo.RunInTx(ctx, func(ctx context.Context) error {
    // ... write aggregates using the tx in ctx ...

    _, err := o.Enqueue(ctx, outbox.EnqueueParams{
        AggregateID:   "order-123",
        AggregateType: "Order",
        EventType:     "OrderCreated",
        Payload:       jsontext.Value(`{"id":"order-123"}`),
        MaxAttempts:   5,                          // optional, defaults to 10
        AvailableAt:   time.Now().Add(time.Minute), // optional delay
    })
    return err
})
```

Enqueue joins the transaction in `ctx` when it was started by a `dbtx.DB` with the same ID as the repository. Both default to `dbtx.ID`, so any `dbtx.New(db).RunInTx` works. Called without a transaction, Enqueue commits the message on its own, which loses the outbox guarantee. Set `o.RequireTx = true` to make it return `outbox.ErrNotInTx` instead.

The transaction is found by ID only. If your application uses several databases, give each `dbtx.DB` its own ID with `SetID`, and the outbox repository the ID of the database that holds `dbtx.outbox`. Otherwise Enqueue may join a transaction of another database.

### Dequeue worker

```go
for {
    err := o.Dequeue(ctx, 10, func(ctx context.Context, msg *outbox.Message) error {
        return publish(ctx, msg)
    })
    if errors.Is(err, outbox.ErrEOQ) {
        time.Sleep(100 * time.Millisecond)
        continue
    }
    if err != nil {
        log.Printf("dequeue: %v", err)
    }
}
```

`Dequeue` leases up to `limit` messages to the worker (`FOR UPDATE SKIP LOCKED`), so several workers can run concurrently. It does not keep a transaction open while the batch waits: each message is handled in its own short transaction. Leased messages are hidden for `o.Lease` (default 30s). Each message's lease is renewed when its handler starts, and its row stays locked while the handler runs, so it is not delivered to another worker meanwhile, even if the handler outlives the lease. A message whose lease expires while it waits for its turn in the batch may be taken by another worker, and is then skipped. If a worker crashes, its messages are delivered again once their leases expire.

### Failure semantics

Return `nil` to acknowledge the message. It is marked as `done` in the same transaction as the writes the handler made through `ctx`. If another worker took the message before its turn, the handler is not called and `Dequeue` returns an error wrapping `ErrLeaseExpired`.

Return any error to fail it. Writes the handler made through `ctx` are rolled back, the error is stored in `last_error`, and the message is hidden until its backoff expires. Backoff times are computed by the database, so clock skew between app and database does not matter.

By default the backoff doubles on each attempt from 1s up to 1h, with jitter. Override it with `o.Backoff`.

Wrap the error with `outbox.ErrDeadLetter` to stop retrying:

```go
return fmt.Errorf("%w: invalid payload: %w", outbox.ErrDeadLetter, err)
```

Handler errors are recorded on the message, not returned from `Dequeue`. A failed handler's error and backoff are recorded in the same transaction that rolls back its writes, before the message is released. An error from `Dequeue` other than `ErrEOQ` means the affected messages will be retried once their lease expires.

A panic in the handler rolls back its transaction and propagates. The message is retried once its lease expires. If `ctx` is cancelled or a handler panics, the messages of the batch that were not handled yet are released right away, without using up an attempt.

### Ordering

The outbox does not guarantee delivery order, not even for messages of the same aggregate:

* Several workers handle messages at the same time, so two messages of an aggregate can be published concurrently and arrive in either order.
* A failed message is retried after its backoff, behind messages enqueued after it.
* Messages are polled in `available_at` order. `available_at` defaults to the start of the enqueuing transaction, not its commit, so a message from a long transaction can be delivered after a message that was enqueued later in another transaction, even if both committed before either was polled.

Consumers must tolerate reordering, for example by giving each event the aggregate's version and ignoring events older than the version they have already applied. Ordering on the receiving side, such as the inbox's `Ordered` mode, cannot restore an order that was lost before the message arrived.

### Dead letters and cleanup

```go
dead, err := o.DeadLetters(ctx, 100) // oldest first
err = o.Requeue(ctx, dead[0].ID)     // reset attempts, visible now

week := time.Now().Add(-7 * 24 * time.Hour)
n, err := o.Purge(ctx, outbox.StatusDone, week)
n, err = o.Purge(ctx, outbox.StatusDead, week)
```

Done and dead messages stay in the table until purged. Call `Purge` periodically.

### Inspection

```go
n, err := o.Count(ctx) // visible, retryable messages
```

## Notes

* Keep processing idempotent – the same message may be redelivered if a worker crashes after publishing but before commit, or if its lease expires.
* Messages are not delivered in order, not even per aggregate. See [Ordering](#ordering).
