# PostgreSQL cache

A typed JSON cache with TTLs, atomic compare/update operations, and leases for
computing missing values. It uses the `dbtx` transaction context.

## Requirements and installation

Requires Go 1.27+ and PostgreSQL. Tests use PostgreSQL 19 beta; see the exact
image below. Start a local database with the [root quick start](../../README.md#run-a-complete-example),
or use your own PostgreSQL DSN.

```bash
go get github.com/alextanhongpin/dbtx/postgres/cache github.com/lib/pq
```

## Complete example

Create an application module with `go mod init example.com/cache-example`, run
`go get` above, and save the following as `main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/cache"
	"github.com/alextanhongpin/dbtx/postgres/cache/repository"
	_ "github.com/lib/pq"
)

type Book struct {
	Title string `json:"title"`
}

func main() {
	ctx := context.Background()
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, repository.Schema); err != nil {
		log.Fatal(err)
	}
	c := cache.New(repository.New(db), cache.WithPrefix("books:"))
	if err := c.Store(ctx, "1", Book{Title: "Go"}, time.Minute); err != nil {
		log.Fatal(err)
	}
	book, err := c.Load[Book](ctx, "1")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(book.Title)
}
```

```bash
export DATABASE_URL='postgres://john:123456@127.0.0.1:5432/dev?sslmode=disable'
go run .
```

Expected output: `Go`.

## Schema and durability

[repository/schema.sql](repository/schema.sql), exported as `repository.Schema`,
creates `dbtx.cache` and the `dbtx.live_cache` view. It can be reapplied. Use the
actual schema rather than copying an older table definition: compute leases
require the `lease` column. There is no `Cache.Migrate` method.

The table is **UNLOGGED**: cache contents are disposable and can be lost after a
database crash; they are not replicated to standbys. Keep authoritative data
elsewhere. Store a zero value with a short TTL for manual negative caching;
[examples_test.go](examples_test.go) shows repository integration.

## Operations

All methods take `ctx` first. Typed reads require a type argument, for example
`c.Load[Book](ctx, key)`; writes infer it from the value.

| Method | Behavior |
| --- | --- |
| `Store(ctx, key, value, ttl)` | Insert or replace. |
| `StoreOnce(ctx, key, value, ttl)` | Write only when absent; otherwise `ErrExists`. |
| `Load[T](ctx, key)` | Read JSON as `T`; missing/expired keys return `ErrNotExist`. |
| `LoadAndDelete[T](ctx, key)` | Read and remove atomically. |
| `LoadOrStore(ctx, key, value, ttl)` | Return `(value, loaded, error)`. |
| `LoadOrCreate[T](ctx, key, fn)` | Lease a miss and compute it; callback returns `(T, time.Duration, error)`. |
| `CompareAndSwap(ctx, key, old, value, ttl)` | Replace if JSON matches; otherwise `ErrConflict`. |
| `CompareAndDelete(ctx, key, old)` | Delete if JSON matches; otherwise `ErrConflict`. |
| `Delete(ctx, key)` | Remove a key. |
| `Exists(ctx, key)` | Check for a live value. |
| `TTL(ctx, key)` | Remaining TTL, `NoExpiration`, or `ErrNotExist`. |
| `Expire(ctx, key, ttl)` | Set a new TTL. |
| `Purge(ctx)` | Delete expired rows across all prefixes; returns count and error. |

`cache.NoExpiration` (zero) means no expiry. Negative TTLs return
`ErrNegativeTTL`. `WithPrefix` adds no separator; include one yourself.
`WithLease` controls `LoadOrCreate`'s compute lease (default 30 seconds), renewed
in the background. Concurrent callers observing that lease get
`ErrRequestInFlight`; retry later. A failed computation releases the lease.

Ordinary operations join a `dbtx` transaction with the repository's ID.
`LoadOrCreate` coordinates its lease outside the caller's transaction so it is
visible to other callers; its compute result is not an atomic business write
in the caller's transaction. The callback still receives that transaction
context: a value computed from uncommitted writes can remain cached even if
the caller rolls back. Compute from committed data when that would be incorrect.
See [cache.go](cache.go) for this boundary.

## Function decorators

Inside your application, with `c` initialized as above:

```go
fetch := func(ctx context.Context, key string) (Book, time.Duration, error) {
    return Book{Title: key}, time.Minute, nil
}
cached := cache.Func(fetch, &cache.FuncConfig[string, Book]{
    Cache: c,
    KeyFn: func(ctx context.Context, key string) (string, error) { return key, nil },
})
book, loaded, err := cached(ctx, "Go")
```

`cache.Idempotent` has the same signature and also compares the request against
the cached request, returning `ErrConflict` on mismatch. Its guarantee lasts
only as long as the cache entry survives; use the durable
[idempotent package](../idempotent/README.md) for durable request records.

## Run the package tests

Requires Go 1.27+ and a running Docker daemon (`docker info`). From the
repository root:

```bash
cd postgres/cache
go test -race -count=1 ./...
go vet ./...
```

Tests start `postgres:19beta3-alpine3.24` on a dynamically assigned port and clean up their
containers. They do not use the root Compose database or `DATABASE_URL`. The
first run needs network access to download dependencies and the image. See the
[root development guide](../../README.md#development-and-verification) for all-module checks.
