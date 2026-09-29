# Postgres Outbox

A PostgreSQL-backed outbox implementation for reliable, transactional event publishing built on [dbtx](https://github.com/alextanhongpin/dbtx).

The outbox pattern lets you write business data and domain events in the same database transaction. Events are stored in `dbtx.outbox` and later dispatched by workers. The package handles visibility, retries with backoff, and a dead-letter emulation without external dependencies.

## Use cases

* **Transactional outbox** – enqueue domain events inside the same transaction as your aggregate writes. If the transaction rolls back, no event is emitted.
* **At-least-once delivery** – workers `Dequeue` visible messages, process them, and acknowledge by returning `nil`. Failures are requeued with a backoff.
* **Retry with limits** – configure `MaxRetry` per message. Once the retry count reaches it, the message is dead and no longer returned by `Count`/`Dequeue`.
* **Delayed visibility** – set `VisibleAt` to schedule future processing.
* **Dead-letter queue emulation** – return `outbox.Nack(err)` with `Skip` set to `true` to stop further retries. Inspect, requeue and purge dead messages with `DeadLetters`, `Requeue` and `PurgeDead`.
* **Idempotent processing** – use `AggregateID` / `AggregateType` to correlate events and avoid duplicates in your consumer.

## Schema

The schema lives in [`internal/schema.sql`](internal/schema.sql) and is exported as `outbox.Schema`.

`max_retry = 0` means unlimited retries. A message is visible only when `visible_at <= now()` and `retry_count < max_retry` (or unlimited). A message with `max_retry = -1`, or with `retry_count >= max_retry > 0`, is dead.

The package expects PostgreSQL with `uuidv7()` support (18+).

## Install

```bash
go get github.com/alextanhongpin/dbtx/postgres/outbox
```

## Migration

The schema is idempotent. Run it once at startup, or copy it into your migrations:

```go
_, err := db.ExecContext(ctx, outbox.Schema)
```

## Quick start

```go
import (
    "context"
    "encoding/json"

    "github.com/alextanhongpin/dbtx"
    "github.com/alextanhongpin/dbtx/postgres/outbox"
)

repo := outbox.NewRepository(db) // db is a *sql.DB
o := outbox.New(repo)

// Enqueue inside the same transaction as your business write.
err := repo.RunInTx(ctx, func(ctx context.Context) error {
    // ... write aggregates using the tx in ctx, e.g. via dbtx.New(db).DBTx(ctx) ...

    _, err := o.Enqueue(ctx, outbox.EnqueueParams{
        AggregateID:   "order-123",
        AggregateType: "Order",
        Type:          "OrderCreated",
        Payload:       json.RawMessage(`{"id":"order-123"}`),
        MaxRetry:      5,
        // VisibleAt: new(time.Now().Add(time.Minute)), // optional delay
    })
    return err
})
```

Enqueue joins the transaction in `ctx` when it was started by a `dbtx.DB` with the same ID as the repository. Both default to `dbtx.ID`, so any `dbtx.New(db).RunInTx` works. If you changed the ID with `SetID`, set the same ID on the repository. Called without a transaction, Enqueue commits the message on its own, which loses the outbox guarantee.

### Dequeue worker

```go
go func() {
    for {
        err := o.Dequeue(ctx, func(ctx context.Context, msg outbox.Message) error {
            // ctx carries the transaction holding the message lock.
            return publish(msg)
        })
        if errors.Is(err, outbox.ErrEOQ) {
            time.Sleep(100 * time.Millisecond)
            continue
        }
        if nack, ok := errors.AsType[*outbox.NackError](err); ok {
            log.Printf("handler failed, will retry: %v", nack)
            continue
        }
        if err != nil {
            log.Printf("dequeue error: %v", err)
        }
    }
}()
```

The handler runs inside a transaction that holds a row lock on the message for the whole call, so several workers can run concurrently (`FOR UPDATE SKIP LOCKED`). Keep handlers short: a slow broker means a long-running transaction and a held connection.

### Handling a specific message

```go
err := o.Handle(ctx, id, func(ctx context.Context, msg outbox.Message) error {
    return process(msg)
})
```

`Handle` ignores visibility and retry limits, so it can replay dead messages. It returns `ErrNotFound` if the message does not exist, and `ErrLocked` if another worker is processing it.

### Failure semantics

Return `nil` to acknowledge and delete the message.

Return any error to fail it. Writes the handler made through `ctx` are rolled back (it runs in a savepoint), while the failure is still recorded: `retry_count + 1`, `last_error`, and `visible_at = now() + backoff`. The time is computed by the database, so app and database clock skew does not matter.

By default the backoff doubles on each retry from 1s up to 1h, with jitter. Override it with `o.Backoff`. For per-message control, wrap the error with `outbox.Nack`:

```go
nack := outbox.Nack(err)
nack.Timeout = 2 * time.Second // time until next visibility, instead of o.Backoff
nack.Skip = true               // stop retrying: set max_retry = -1, emulating a DLQ
return nack
```

Handler errors are returned from `Dequeue`/`Handle` as a `*NackError`, and `errors.Is` still matches the cause. Any other error means the message was left untouched.

A panic in the handler rolls back the transaction and propagates, without recording a retry.

### Dead letters

```go
dead, err := o.DeadLetters(ctx, 100)                  // oldest first
msg, err := o.Requeue(ctx, id, 5)                     // reset retries, visible now
n, err := o.PurgeDead(ctx, time.Now().Add(-7*24*time.Hour)) // delete old dead messages
```

Dead messages stay in the table until purged. Call `PurgeDead` periodically.

### Inspection

```go
n, err := o.Count(ctx) // visible, retryable messages
```

## API highlights

* `NewRepository(db *sql.DB) *PostgresRepository` – the PostgreSQL `Repository`. It embeds `*dbtx.DB`, so it has `RunInTx`.
* `New(repo Repository) *Outbox` – create the outbox.
* `Enqueue(ctx, EnqueueParams) (uuid.UUID, error)` – insert a new message.
* `Dequeue(ctx, fn)` – lock the next visible message and hand it to `fn`.
* `Handle(ctx, id, fn)` – same as `Dequeue` but for a known message id.
* `Count(ctx) (int64, error)` – number of visible, retryable messages.
* `DeadLetters`, `Requeue`, `PurgeDead` – manage dead messages.

Errors:

* `outbox.ErrEOQ` – end of queue, no visible messages in `Dequeue`.
* `outbox.ErrNotFound` – message not found in `Handle` or `Requeue`.
* `outbox.ErrLocked` – message is being processed by another worker in `Handle`.

## Notes

* Keep processing idempotent – the same message may be redelivered if a worker crashes after publishing but before commit.
* `MaxRetry = 0` means infinite retries; set a positive value to bound attempts.
* Messages are dequeued in `visible_at` order, so retried messages go behind fresh ones.
