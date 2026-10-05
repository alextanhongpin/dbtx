# Postgres Inbox

A PostgreSQL-backed inbox implementation for idempotent, transactional message consumption built on [dbtx](https://github.com/alextanhongpin/dbtx).

The inbox pattern is the receiving side of the outbox. Incoming messages are stored in `dbtx.inbox`, deduplicated by `(source, message_id)`, and later handled by workers. The handler's writes and the acknowledgement commit in the same transaction, so each message's side effects are applied once even when the broker redelivers it. The package handles deduplication, visibility, retries with backoff, per-aggregate ordering and a dead-letter emulation without external dependencies.

## Use cases

* **Deduplication** – `Enqueue` returns `inbox.ErrExists` for a message that was already received, so a redelivered message is acknowledged without being handled twice.
* **Exactly-once side effects** – the handler's writes and the `done` status commit together. If the handler fails, both are rolled back.
* **Decoupled consumption** – acknowledge the broker as soon as the message is stored, then handle it at your own pace.
* **Retry with limits** – set `MaxAttempts` per message (default 10). Once a message runs out of attempts, it is dead.
* **Delayed visibility** – set `AvailableAt` to schedule future processing.
* **Per-aggregate ordering** – set `Ordered` to handle messages of the same `AggregateID` one at a time, in the order they were received.
* **Dead letters** – wrap the handler error with `inbox.ErrDeadLetter` to stop further retries. Inspect, requeue and purge dead messages with `DeadLetters`, `Requeue` and `Purge`.

## Schema

The schema lives in [`repository/schema.sql`](repository/schema.sql) and is exported as `repository.Schema`. Apply it once, or copy it into your migrations:

```go
_, err := db.ExecContext(ctx, repository.Schema)
```

The table and indexes are created with `if not exists`, but the `dbtx.inbox_status` enum is not: Postgres has no `create type if not exists`, and sqlc cannot parse the `DO` block that would emulate it. Running the schema twice fails on that statement.

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
go get github.com/alextanhongpin/dbtx/postgres/inbox
```

## Quick start

```go
repo := repository.New(db) // db is a *sql.DB
in := inbox.New(repo)

// Store the message when it arrives from the broker.
_, err := in.Enqueue(ctx, inbox.EnqueueParams{
    Source:      "payments-service",           // who sent it
    MessageID:   evt.ID,                       // unique per source
    MessageType: "PaymentCaptured",
    AggregateID: "order-123",                  // optional, for Ordered
    Payload:     jsontext.Value(evt.Data),
    MaxAttempts: 5,                            // optional, defaults to 10
    AvailableAt: time.Now().Add(time.Minute),  // optional delay
})
if err != nil && !errors.Is(err, inbox.ErrExists) {
    return err // do not ack, let the broker redeliver
}
// Stored now, or received before: ack the broker either way.
```

Enqueue joins the transaction in `ctx` when it was started by a `dbtx.DB` with the same ID as the repository, so a handler can enqueue follow-up messages that commit only if it succeeds.

### Dequeue worker

```go
for {
    err := in.Dequeue(ctx, 10, func(ctx context.Context, msg *inbox.Message) error {
        // Write through ctx to join the transaction that acks the message.
        return apply(ctx, msg)
    })
    if errors.Is(err, inbox.ErrEOQ) {
        time.Sleep(100 * time.Millisecond)
        continue
    }
    if err != nil {
        log.Printf("dequeue: %v", err)
    }
}
```

`Dequeue` leases up to `limit` messages to the worker (`FOR UPDATE SKIP LOCKED`), so several workers can run concurrently. It does not keep a transaction open while the batch waits: each message is handled in its own short transaction. Leased messages are hidden for `in.Lease` (default 30s). Each message's lease is renewed when its handler starts, and its row stays locked while the handler runs, so it is not delivered to another worker meanwhile, even if the handler outlives the lease. A message whose lease expires while it waits for its turn in the batch may be taken by another worker, and is then skipped. If a worker crashes, its messages are delivered again once their leases expire. A lease that expires on the final attempt marks the message dead.

### Ordering

```go
in.Ordered = true
```

With `Ordered`, a message is only claimed once every earlier message of the same `AggregateID` is done or dead. A batch then holds at most one message per aggregate. Messages without an `AggregateID` are not ordered. A dead message stops blocking the messages after it, so check dead letters if strict ordering matters.

Two messages of the same aggregate are never handled at the same time, but order follows the messages that are visible when one is claimed. A message whose enqueuing transaction commits after a later message of its aggregate was claimed, or a dead message that is requeued, is handled after the later message.

Without `Ordered`, messages are dequeued in `available_at` order, so retried messages go behind fresh ones.

### Failure semantics

Return `nil` to acknowledge the message. It is marked as `done` in the same transaction as the writes the handler made through `ctx`. If another worker took the message before its turn, the handler is not called and `Dequeue` returns an error wrapping `inbox.ErrLeaseLost`.

Return any error to fail it. Writes the handler made through `ctx` are rolled back, the error is stored in `last_error`, and the message is hidden until its backoff expires. Backoff times are computed by the database, so clock skew between app and database does not matter.

By default the backoff doubles on each attempt from 1s up to 1h, with jitter. Override it with `in.Backoff`.

Wrap the error with `inbox.ErrDeadLetter` to stop retrying:

```go
return fmt.Errorf("%w: invalid payload: %w", inbox.ErrDeadLetter, err)
```

Handler errors are recorded on the message, not returned from `Dequeue`. Use `in.OnError` to log or count them:

```go
in.OnError = func(msg *inbox.Message, err error) {
    log.Printf("inbox: message %s (attempt %d): %v", msg.ID, msg.Attempts, err)
}
```

A failed handler's error and backoff are recorded in the same transaction that rolls back its writes, before the message is released. An error from `Dequeue` other than `ErrEOQ` means the affected messages will be retried once their lease expires.

A panic in the handler rolls back its transaction and propagates. The message is retried once its lease expires. If `ctx` is cancelled or a handler panics, the messages of the batch that were not handled yet are released right away, without using up an attempt.

### Dead letters and cleanup

```go
dead, err := in.DeadLetters(ctx, 100) // oldest first
err = in.Requeue(ctx, dead[0].ID)     // reset attempts, visible now

week := time.Now().Add(-7 * 24 * time.Hour)
n, err := in.Purge(ctx, inbox.StatusDone, week)
n, err = in.Purge(ctx, inbox.StatusDead, week)
```

Done and dead messages stay in the table until purged. Call `Purge` periodically. Purging done messages also forgets their `(source, message_id)`, so keep them for longer than the broker may redeliver.

### Inspection

```go
n, err := in.Count(ctx) // visible, retryable messages
```

## Notes

* Side effects outside the database, such as HTTP calls, are not covered by the transaction. Keep them idempotent: they run again if the handler fails after making them, or if the lease expires.
* `MessageID` must be stable across redeliveries, for example the outbox message ID or the event UUID, not a broker delivery tag.
