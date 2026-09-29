# idempotent

A lightweight PostgreSQL-backed idempotency helper for Go. Wrap any business function with `Idempotent.Func` to guarantee that the same request is executed only once, even under concurrent retries. Subsequent calls with the same key and request payload return the cached result; calls with a different payload for the same key return `ErrRequestConflict`.

## Features

- **PostgreSQL storage** via `dbtx` with a simple schema (`dbtx.idempotency_keys`).
- **Deterministic request hashing** using `xxh3` on JSON-ordered payloads.
- **Transactional LoadOrStore** and **atomic Update** to avoid race conditions.
- Generic `Func[K,V]` API that works with any request/response types.
- Safe concurrent use – only one caller stores, others load.

## Installation

```bash
go get github.com/alextanhongpin/dbtx/postgres/idempotent
```

Requires Go 1.24+ and a PostgreSQL database.

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
    "fmt"

    "github.com/alextanhongpin/dbtx/postgres/idempotent"
    "github.com/alextanhongpin/dbtx/postgres/idempotent/internal"
)

type Request struct { Name string }
type Response struct { Msg string }

func main() {
    db, _ := sql.Open("postgres", dsn)
    repo := idempotent.NewRepository(db)
    idb  := idempotent.New(repo)

    // Wrap your business logic
    idp := idb.Func(func(ctx context.Context, req Request) (*Response, error) {
        // this runs only once per unique key+request
        return &Response{Msg: fmt.Sprintf("hi, %s", req.Name)}, nil
    })

    ctx := context.Background()
    key := "my-op-123"
    req := Request{Name: "alice"}

    res, loaded, err := idp.LoadOrCreate(ctx, key, req)
    if err != nil {
        if errors.Is(err, idempotent.ErrRequestConflict) {
            // same key with different request
        }
        panic(err)
    }
    fmt.Println(res.Msg, "loaded=", loaded) // hi, alice loaded=false on first call

    // Second call returns cached result without re-executing the function
    res2, loaded2, _ := idp.LoadOrCreate(ctx, key, req)
    fmt.Println(res2.Msg, "loaded=", loaded2) // hi, alice loaded=true
}
```

### API

- `New(repo repository) *Idempotent` – create an Idempotent client.
- `NewRepository(db *sql.DB) *Repository` – create a repository backed by `dbtx`.
- `Func[K,V](fn func(context.Context, K) (V, error)) idempotent[K,V]` – returns a handler with `LoadOrCreate(ctx, key, req)`.
- `LoadOrCreate(ctx, key, req)` – returns `(V, loaded, error)`. `loaded=true` means result was read from DB.
- `Delete(ctx, key)` – removes a stored idempotency record.
- `ErrRequestConflict` – returned when the same key is used with a different request payload.

## Project Structure

- `idempotent.go` – generic wrapper, hashing and transactional logic.
- `idempotent_test.go` – integration tests using a test PostgreSQL container.
- `internal/` – SQL schema, sqlc generated code and `Repository` implementation.
- `go.mod`, `go.sum` – module dependencies.

## Running Tests

```bash
go test ./...
```

Tests spin up a Postgres container via `dbtest` and apply the embedded schema.

## Contributing

Contributions are welcome. Please open an issue or submit a Pull Request.
