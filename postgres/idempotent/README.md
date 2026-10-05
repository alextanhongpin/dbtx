# idempotent

A PostgreSQL-backed idempotency helper for Go, built on [dbtx](https://github.com/alextanhongpin/dbtx). `Idempotent.Do` runs a function at most once to completion per idempotency key, even with concurrent retries and crashed workers. Later calls with the same key and request get the stored response. Calls with a different request for the same key return `ErrRequestMismatch`.

Work is protected by a **lease** and a **fencing token**. Long tasks can be split into **checkpointed steps**, so a retry continues from the last finished step instead of starting over.

## Features

- **Exactly-once completion** for work inside the database: each step's writes commit in the same transaction as the idempotency record.
- **Leases with fencing tokens.** When a worker crashes, another worker takes over after the lease expires. Writes from the crashed worker are rejected.
- **Checkpoints** for multi-step tasks, so a retry continues from the last finished step.
- **Cached outcomes.** Both `completed` and `failed` responses are stored and returned to later callers.
- **Bounded retries with backoff.** Handler errors release the key for retry, with optional exponential backoff. The key fails once `MaxAttempts` is used up.
- **Exact request comparison** using `jsonb` equality, which ignores key order and whitespace.

## Install

```bash
go get github.com/alextanhongpin/dbtx/postgres/idempotent
```

Requires Go 1.27+ (for the standard library `uuid` and `encoding/json/jsontext` packages). Tests run against PostgreSQL 19.

## Schema

The schema lives in [`repository/schema.sql`](repository/schema.sql) and is exported as `repository.Schema`:

```go
_, err := db.ExecContext(ctx, repository.Schema)
```

> **Note:** `create type` has no `if not exists`, so the schema cannot be run twice yet. Copy it into your migrations instead of running it at every startup.

A key moves through these statuses:

```
  Claim
    │
    ▼
in_progress ──Response{completed}──▶ completed
  │     ▲   └─Response{failed}─────▶ failed
  │     │
  │     │ Claim (attempts left, backoff elapsed)
  ▼     │
retryable

handler error or invalid result: in_progress ──▶ retryable (or failed on the last attempt)
worker crash, lease expired:     in_progress ──Claim──▶ in_progress (new fencing token)
```

`completed` and `failed` are final. A key stays until `expires_at` (`TTL`, 24 hours by default, extended on each claim and outcome) and is then removed by `Purge`.

## Quick start

```go
import (
    "context"
    "database/sql"
    "errors"

    "github.com/alextanhongpin/dbtx/postgres/idempotent"
    "github.com/alextanhongpin/dbtx/postgres/idempotent/repository"
)

db, _ := sql.Open("postgres", dsn)
idp := idempotent.New(repository.New(db))

fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
    // ctx carries the transaction. Writes made through dbtx with this ctx
    // commit together with the idempotency key.
    return &idempotent.Result{
        Response: &idempotent.Response{
            Status: string(idempotent.StatusCompleted),
            Data:   []byte(`{"msg": "hi, alice"}`),
        },
    }, nil
}

req := idempotent.Request{Data: []byte(`{"name": "alice"}`)}
res, err := idp.Do(ctx, "my-op-123", fn, req)
switch {
case errors.Is(err, idempotent.ErrRequestMismatch):
    // 422: key reused with a different request.
case errors.Is(err, idempotent.ErrRequestInFlight):
    // 409: another worker holds the key; retry later.
case errors.Is(err, idempotent.ErrBackoff):
    // 429: the last attempt failed recently; retry later.
case errors.Is(err, idempotent.ErrMaxAttempts):
    // The key ran out of attempts.
case err != nil:
    // Handler or database error. The key is released for retry.
}

// A second call returns the stored response without running fn.
res, err = idp.Do(ctx, "my-op-123", fn, req)
```

`Request.Data` and `Response.Data` must be valid JSON. They are stored as `jsonb`.

## Checkpoints

A handler returns **exactly one** of `Checkpoint` or `Response`:

- **`Checkpoint`** saves progress and commits the step. `Do` calls the handler again with the new checkpoint.
- **`Response`** ends the run, with `Status` set to `completed` or `failed`.

The first call gets the checkpoint `started`. After a takeover or retry, the handler gets the last saved checkpoint:

```go
fn := func(ctx context.Context, p idempotent.Params) (*idempotent.Result, error) {
    switch p.Checkpoint.Name {
    case "started":
        // Step 1: reserve stock.
        return &idempotent.Result{
            Checkpoint: &idempotent.Checkpoint{Name: "reserved", Data: []byte(`{"reservation": 42}`)},
        }, nil
    case "reserved":
        // Step 2: create the order using p.Checkpoint.Data.
        return &idempotent.Result{
            Response: &idempotent.Response{
                Status: string(idempotent.StatusCompleted),
                Data:   []byte(`{"order": 7}`),
            },
        }, nil
    }
    return nil, fmt.Errorf("unknown checkpoint: %s", p.Checkpoint.Name)
}
```

A result with neither or both fields set returns `ErrInvalidResult` and releases the key.

Each saved checkpoint appends the one it replaces to the `checkpoint_logs` column, as `{"name", "data"}` objects with the oldest first. The column records the steps a key has passed through, which helps when debugging.

## How it works

1. **Claim.** The `dbtx.claim` function (it requires `READ COMMITTED`) reads the row and returns an outcome. A finished key returns the stored response; a mismatched request, a live lease or an unexpired backoff returns `ErrRequestMismatch`, `ErrRequestInFlight` or `ErrBackoff`. A key whose lease expired with no attempts left is marked `failed` and returns `ErrMaxAttempts`. Otherwise an `INSERT … ON CONFLICT DO UPDATE` creates the key or takes it over (attempts + 1), with a new fencing token from the `dbtx.idempotency_fencing_token_seq` sequence. Tokens never repeat, even after a key is purged and claimed again, and they increase for a key over time. If it loses a race, the function reads the row again.
2. **Step.** Each handler call runs in its own transaction:
   - `Lock` updates the row, which locks it for the transaction and extends the lease. The update checks the fencing token, so a worker that lost the key fails here.
   - The handler runs with the transaction in `ctx`.
   - `Checkpoint`, `Ack` (completed) or `Fail` (failed) writes the outcome, checking the fencing token again.
3. **Release.** On any error, `Nack` marks the key `retryable` with `retry_after` set to `BaseBackoff * 2^(attempts-1)`, capped at `MaxBackoff`. On the last attempt it marks the key `failed` instead, and later calls get the stored failed response. If the worker was fenced out, `Nack` matches no row and nothing changes.

While a step holds the row lock, a concurrent `Claim` waits until the step commits. It then sees either a fresh lease or a finished key, so a step that runs longer than the lease is never taken over. No heartbeat is needed. The lease only matters when a worker crashes: its transaction rolls back, the lock is released, and another worker can take over once the lease expires.

## API

- `New(repo Repository) *Idempotent` creates a client. `Repository` is an interface, so you can supply your own implementation.
- `repository.New(db *sql.DB) *repository.Repository` creates the PostgreSQL repository backed by `dbtx`.
- `Idempotent.Lease` is the lease length (default `DefaultLease`, 30s). Each step extends it.
- `Idempotent.MaxAttempts` caps claims per key (default `DefaultMaxAttempts`, 10).
- `Idempotent.TTL` is how long a key is kept (default `DefaultTTL`, 24h).
- `Idempotent.BaseBackoff` and `Idempotent.MaxBackoff` set the retry backoff after a handler error. A zero `MaxBackoff` disables it.
- `Do(ctx, key, fn, req) (*Response, error)` runs `fn` or returns the stored response.
- `repository.Repository.Purge(ctx)` deletes up to 1000 expired keys and returns the number deleted. Keys whose lease is still live are never deleted. Run it periodically until it returns 0.

Errors:

| Error | Meaning |
|---|---|
| `ErrRequestMismatch` | The key was used with a different request. |
| `ErrRequestInFlight` | Another worker holds a live lease on the key. |
| `ErrBackoff` | The last attempt failed and the backoff has not elapsed. |
| `ErrMaxAttempts` | A crashed worker used the last attempt; the key is now `failed`. The message includes the last error. |
| `ErrInvalidResult` | The handler returned neither or both of `Checkpoint` and `Response`. |
| `ErrClaimed` | The fencing token no longer matches; another worker owns the key. |
| `ErrNotFound` | The key does not exist. |

## Behaviour

- **Keys are global.** There is no scope; prefix keys yourself (for example `charge:<id>`) if different operations can share a key.
- **Handler errors are retried, failed responses are not.** Return an error for transient failures. Return `Response{Status: failed}` for permanent ones, which are stored and returned to later callers.
- **External side effects are not covered.** If a step calls an external service and its transaction then fails to commit, a retry calls the service again. Pass the idempotency key, or the fencing token, to services that support it.
- **Each step holds a connection and a row lock** for as long as the handler runs. Keep steps short, and split long work into checkpoints.
- **Request encoding must be stable.** If the JSON for the same logical request changes (for example a renamed field after a deploy), retries return `ErrRequestMismatch`.

## Project structure

- `idempotent.go`: `Do`, claim handling and the step transaction.
- `idempotent_test.go`: integration tests against a PostgreSQL container.
- `repository/`: schema, queries, sqlc-generated code and the PostgreSQL implementation of `idempotent.Repository`.

## Running tests

```bash
go test ./...
```

Tests start a PostgreSQL container via `dbtest` and apply the embedded schema.

## Contributing

Contributions are welcome. Please open an issue or submit a Pull Request.
