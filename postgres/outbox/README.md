# Postgres Outbox

A PostgreSQL-backed outbox implementation for reliable, transactional event publishing built on [dbtx](https://github.com/alextanhongpin/dbtx).

The outbox pattern lets you write business data and domain events in the same database transaction. Events are stored in `dbtx.outbox` and later dispatched by workers. The package handles visibility, retries, and a dead-letter emulation without external dependencies.

## Use cases

* **Transactional outbox** – enqueue domain events inside the same transaction as your aggregate writes. If the transaction rolls back, no event is emitted.
* **At-least-once delivery** – workers `Dequeue` visible messages, process them, and acknowledge. Failures are automatically requeued with a backoff.
* **Retry with limits** – configure `max_retry` per message. Once the retry count is exceeded the message is no longer returned by `Count`/`Dequeue`.
* **Delayed visibility** – set `VisibleAt` to schedule future processing.
* **Dead-letter queue emulation** – return a `Nack{Skip:true}` to stop further retries by setting `max_retry = -1`.
* **Idempotent processing** – use `AggregateID` / `AggregateType` to correlate events and avoid duplicates in your consumer.

## Schema

```sql
create schema if not exists dbtx;

create table if not exists dbtx.outbox
 (
  id             uuid not null default uuidv7(),
  aggregate_id   text not null check (aggregate_id <> ''),
  aggregate_type text not null check (aggregate_type <> ''),
  type           text not null check (type <> ''),
  payload        jsonb not null default '{}',
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now(),
  last_error     text not null default '',
  max_retry      int not null default 0,
  retry_count    int not null default 0 check (retry_count >= 0),
  visible_at     timestamptz not null default now(),

  primary key (id)
);

create index if not exists dbtx_outbox_visible_at on dbtx.outbox(visible_at);
```

`max_retry = 0` means unlimited retries. A message is visible only when `visible_at <= now()` and `retry_count < max_retry` (or unlimited).

## Install

```bash
go get github.com/alextanhongpin/dbtx/postgres/outbox
```

## Migration

The schema is embedded in the package. Run it once at startup:

```go
import "github.com/alextanhongpin/dbtx/postgres/outbox"

o := outbox.New(db)
err := o.Migrate(ctx)
```

`Migrate` executes the embedded `internal/schema.sql`.

## Quick start

```go
import (
    "context"
    "encoding/json"
    "time"
    "github.com/alextanhongpin/dbtx/postgres/outbox"
)

o := outbox.New(db)

// Enqueue inside the same transaction as your business write
err := o.RunInTx(ctx, func(ctx context.Context) error {
    // ... write aggregates ...

    _, err := o.Enqueue(ctx, outbox.EnqueueParams{
        AggregateID:   "order-123",
        AggregateType: "Order",
        Type:          "OrderCreated",
        Payload:       json.RawMessage(`{"id":"order-123"}`),
        MaxRetry:      5,
        // VisibleAt: time.Now().Add(1 * time.Minute), // optional delay
    })
    return err
})
```

### Dequeue worker

```go
go func() {
    for {
        err := o.Dequeue(ctx, func(ctx context.Context, msg outbox.Message) (*outbox.Nack, error) {
            // ctx is a dbtx transaction context
            return publish(msg)
        })
        if err == outbox.ErrEOQ {
            time.Sleep(100 * time.Millisecond)
            continue
        }
        if err != nil {
            log.Printf("dequeue error: %v", err)
        }
    }
}()
```

### Handling a specific message

```go
err := o.Handle(ctx, id, func(ctx context.Context, msg outbox.Message) (*outbox.Nack, error) {
    if err := process(msg); err != nil {
        return &outbox.Nack{
            Error:   err.Error(),
            Timeout: 2 * time.Second, // backoff
        }, nil
    }
    return nil, nil // delete on success
})
```

### Nack semantics

Return `nil` to acknowledge and delete the message.

Return `*outbox.Nack` to requeue:

```go
type Nack struct {
    Error   string
    Skip    bool        // set max_retry = -1, emulate DLQ
    Timeout time.Duration // time until next visibility
}
```

The message is updated with `retry_count + 1`, `last_error`, and `visible_at = now() + Timeout`. If `Skip` is true, `max_retry` is set to `-1` so the message will never become visible again.

### Inspection

```go
n, err := o.Count(ctx) // visible, retryable messages
```

## API highlights

* `New(db *sql.DB) *Outbox` – create a wrapper around a `*sql.DB`.
* `Migrate(ctx)` – create `dbtx.outbox`.
* `Enqueue(ctx, EnqueueParams) (uuid.UUID, error)` – insert a new event. Must run in a transaction.
* `Dequeue(ctx, fn)` – atomically peek the next visible message with `FOR UPDATE SKIP LOCKED`, increment `retry_count`, and hand it to `fn`. On `Nack` the row is requeued.
* `Handle(ctx, id, fn)` – same as `Dequeue` but for a known message id.
* `Count(ctx) (int64, error)` – number of visible, retryable messages.

Messages are returned as `outbox.Message`, which mirrors `postgres.DbtxOutbox`:

```go
type Message struct {
    ID            uuid.UUID
    AggregateID   string
    AggregateType string
    Type          string
    Payload       json.RawMessage
    CreatedAt     time.Time
    UpdatedAt     time.Time
    LastError     string
    MaxRetry      int32
    RetryCount    int32
    VisibleAt     time.Time
}
```

Errors:

* `outbox.ErrNotFound` – message not found in `Handle`.
* `outbox.ErrEOQ` – end of queue, no visible messages in `Dequeue`.

All methods use `dbtx` transaction helpers, so they can be composed with `RunInTx`/`RunInTx2`. The `Peek` and `Find` queries use `FOR UPDATE SKIP LOCKED` for safe concurrent workers.

## Notes

* Keep processing idempotent – the same message may be redelivered if a worker crashes after processing but before commit.
* `max_retry = 0` means infinite retries; set a positive value to bound attempts.
* Delayed publishing is achieved by setting `VisibleAt` in `EnqueueParams`.
* The package expects PostgreSQL with `uuidv7()` support.
