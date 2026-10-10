# PostgreSQL JSON scanning

`jsonb.New(db)` wraps a `*sql.DB` and decodes a **single JSON/JSONB column per
row** into a Go value. `QueryContext[T]` returns a slice; `QueryRowContext[T]`
returns one value or `sql.ErrNoRows`. Use JSON tags to match SQL object keys.

## Requirements and installation

Requires Go 1.27+ and PostgreSQL. No package-owned schema needs migration.

```bash
go get github.com/alextanhongpin/dbtx/postgres/jsonb github.com/lib/pq
```

## Complete example

In a new application module (`go mod init example.com/jsonb-example`), install
the dependencies above and save this as `main.go`. Start PostgreSQL using the
[root quick start](../../README.md#run-a-complete-example) or provide your own DSN.

```go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	"github.com/alextanhongpin/dbtx/postgres/jsonb"
	_ "github.com/lib/pq"
)

type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
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
	j := jsonb.New(db)
	user, err := j.QueryRowContext[User](ctx,
		"SELECT jsonb_build_object('id', $1::int, 'name', $2::text)", 1, "Alice")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d %s\n", user.ID, user.Name)
}
```

```bash
export DATABASE_URL='postgres://john:123456@127.0.0.1:5432/dev?sslmode=disable'
go run .
```

Expected output: `1 Alice`.

## Joining multiple tables

With application-owned `users(id, name)` and `orders(id, user_id)` tables:

```sql
SELECT jsonb_build_object('user', to_jsonb(u), 'order', to_jsonb(o))
FROM users u
LEFT JOIN orders o ON u.id = o.user_id;
```

Decode each row with `QueryContext[Joined]`, where `Joined` has fields tagged
`json:"user"` and `json:"order"`. A pointer order field can represent `NULL` on
an unmatched left join. To aggregate all rows into one JSON array, use
`jsonb_agg(jsonb_build_object(...))` with `QueryRowContext[[]Joined]`; wrap the
aggregate in `coalesce(..., '[]'::jsonb)` if you want an empty array on no rows.

Column/key mismatches are logged with `slog`; they are not strict schema errors.
Malformed JSON or incompatible types return decoding errors. The wrapper holds
a `*sql.DB`, so its queries do not automatically join a `dbtx` context transaction.
See [jsonb_test.go](jsonb_test.go) for table setup and aggregate examples.

## Run the package tests

Requires Go 1.27+, a C compiler for `-race`, and a running Docker daemon
(`docker info`). From the repository root:

```bash
cd postgres/jsonb
go test -race -count=1 ./...
go vet ./...
```

Tests start `postgres:19beta3-alpine3.24` on a dynamically assigned port, apply
their own schema, and clean up containers. The first run needs network access
for dependencies and the image. Compose and `DATABASE_URL` are not used by the
tests. See the [root guide](../../README.md#development-and-verification) for
checks across all modules.
