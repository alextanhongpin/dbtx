# idempotent

A lightweight PostgreSQL-backed idempotency helper for Go. Wrap any business function with `Idempotent.Func` to guarantee that the same request is executed only once, even under concurrent retries. Subsequent calls with the same key and request payload return the cached result; calls with a different payload for the same key return `ErrRequestConflict`. Keys are scoped per handler, so different handlers can reuse the same key.

## Features

- **PostgreSQL storage** via `dbtx` with a simple schema (`dbtx.idempotency_keys`, keyed by `(scope, key)`).
- **Exact request comparison** using `jsonb` equality, which ignores key order and whitespace, and compares numbers without precision loss.
- **Single round-trip** `LoadOrStore` using `INSERT ... ON CONFLICT DO SELECT`.
- Generic `Func[K,V]` API that works with any request/response types.
- Safe concurrent use – only one caller executes, others wait and load the result.

## Installation

```bash
go get github.com/alextanhongpin/dbtx/postgres/idempotent
```

Requires Go 1.27+ and **PostgreSQL 19+** (for `ON CONFLICT DO SELECT`).

## Setup

Run the bundled schema to create the table:

```go
import "github.com/alextanhongpin/dbtx/postgres/idempotent"

// Schema is embedded in the package
_, err := db.Exec(idempotent.Schema)
```

## Usage

```go
import (
    "context"
    "database/sql"
    "errors"
    "fmt"

    "github.com/alextanhongpin/dbtx/postgres/idempotent"
)

type Request struct { Name string }
type Response struct { Msg string }

func main() {
    db, _ := sql.Open("postgres", dsn)
    repo := idempotent.NewRepository(db)
    idb  := idempotent.New(repo)

    // Wrap your business logic
    idp := idb.Func("greet", func(ctx context.Context, req Request) (*Response, error) {
        // this runs only once per unique key+request
        return &Response{Msg: fmt.Sprintf("hi, %s", req.Name)}, nil
    })

    ctx := context.Background()
    key := "my-op-123"
    req := Request{Name: "alice"}

    res, loaded, err := idp.LoadOrCreate(ctx, key, req)
    if errors.Is(err, idempotent.ErrRequestConflict) {
        // same key with different request
    }
    if err != nil {
        panic(err)
    }
    fmt.Println(res.Msg, "loaded=", loaded) // hi, alice loaded=false on first call

    // Second call returns cached result without re-executing the function
    res2, loaded2, _ := idp.LoadOrCreate(ctx, key, req)
    fmt.Println(res2.Msg, "loaded=", loaded2) // hi, alice loaded=true
}
```

### API

- `New(repo Repository) *Idempotent` – create an Idempotent client. `Repository` is an interface, so you can supply your own implementation.
- `NewRepository(db *sql.DB) *PostgresRepository` – create the PostgreSQL repository backed by `dbtx`.
- `Func[K,V](scope string, fn func(context.Context, K) (V, error)) Handler[K,V]` – returns a handler with `LoadOrCreate(ctx, key, req)`. Keys are unique per `scope`; panics if `scope` is empty.
- `LoadOrCreate(ctx, key, req)` – returns `(V, loaded, error)`. `loaded=true` means the result was read from the DB.
- `Delete(ctx, scope, key)` – removes a stored idempotency record. Returns `ErrNotFound` if the key does not exist.
- `DeleteBefore(ctx, t)` – removes all records created before `t`. Run it periodically to expire old keys.
- `ErrRequestConflict` – returned when the same key is used with a different request payload.

## Behaviour

- **Keys are scoped.** The primary key is `(scope, key)`, so `Func("greet", ...)` and `Func("charge", ...)` can both use key `123` without conflicting. Handlers sharing a scope share its keys, so give each operation its own scope, and don't rename a scope while keys are live.
- **`fn` runs inside a transaction.** The `ctx` passed to `fn` carries the transaction, so database writes made through `dbtx` with that `ctx` commit or roll back together with the idempotency key.
- **Concurrent calls block.** A call with a key that is being processed waits for the first call to finish, holding a database connection while it waits. Keep `fn` short.
- **Errors are not cached.** If `fn` returns an error, the transaction is rolled back and the next call with the same key executes `fn` again.
- **External side effects are not covered.** If `fn` calls an external service and the transaction then fails to commit, the call is not recorded and a retry executes it again. Pass the idempotency key to the external service when it supports one.
- **Request types must be stable.** Adding or renaming a field changes the stored JSON, so in-flight retries across a deploy may return `ErrRequestConflict`.

## Project Structure

- `idempotent.go` – generic wrapper and transactional logic.
- `idempotent_test.go` – integration tests using a test PostgreSQL container.
- `internal/` – SQL schema, sqlc generated code and `Repository` implementation.

## Running Tests

```bash
go test ./...
```

Tests spin up a Postgres container via `dbtest` and apply the embedded schema.

## Contributing

Contributions are welcome. Please open an issue or submit a Pull Request.
