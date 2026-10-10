# dbtest

PostgreSQL integration-test helpers using Docker. `DB(t)` returns a pooled
`*sql.DB`; `Tx(t)` returns a `*sql.DB` backed by `go-txdb`, whose changes roll back
when test cleanup closes it. Neither function returns a `dbtx.DB` or `dbtx.Tx`.

## Requirements and installation

Requires Go 1.27+, a running Docker daemon (`docker info`), and network access
for image/dependency downloads. Register a SQL driver in your test package.
The container uses a dynamically assigned port, so no local PostgreSQL or
Compose setup is needed.

```bash
go get github.com/alextanhongpin/dbtx/testing/dbtest github.com/lib/pq
```

## Complete test example

Create an application module (`go mod init example.com/dbtest-example`), install
the dependencies above, and save this as `database_test.go`:

```go
package example_test

import (
	"database/sql"
	"testing"

	"github.com/alextanhongpin/dbtx/testing/dbtest"
	_ "github.com/lib/pq"
)

func TestMain(m *testing.M) {
	stop := dbtest.Init(dbtest.Options{
		Image: "postgres:17.4",
		Hook: func(dsn string) error {
			db, err := sql.Open("postgres", dsn)
			if err != nil {
				return err
			}
			defer db.Close()
			_, err = db.Exec(`CREATE TABLE users (
                id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
                name text NOT NULL
            )`)
			return err
		},
	})
	defer stop()
	m.Run()
}

func TestInsert(t *testing.T) {
	db := dbtest.Tx(t)
	var name string
	err := db.QueryRowContext(t.Context(),
		"INSERT INTO users (name) VALUES ($1) RETURNING name", "Alice").Scan(&name)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Alice" {
		t.Fatalf("got %q", name)
	}
}
```

Run `go test -v -count=1 ./...` in that application module. Expect `TestInsert` to
pass; the inserted row rolls back at test cleanup. `m.Run()` lets the test
runner preserve the exit status after deferred container cleanup. Do not call
`os.Exit(m.Run())` before running cleanup.

## Isolation and lifecycle

- Call `Init` once from `TestMain` before global `DB`, `Tx`, or `DSN` helpers.
  Initialization failures panic. The returned `func() error` stops the container.
- `DB(t)` opens a pool in the shared container database; committed writes and
  schema changes persist for later tests. Clean up data explicitly where needed.
- `Tx(t)` opens a separate rollback-isolated test connection. It suits CRUD
  checks, but use real pools for independent transactions, locking, and commit
  visibility tests. Sequences are not rolled back by PostgreSQL.
- `dbtest.New(t, opts...)` starts a separate container, automatically cleaned up
  with `t.Cleanup`. Use `client.DB(t)`, `client.Tx(t)`, or `client.DSN()` on it.
- `Hook` runs once after container initialization, before clients are returned.
  Apply migrations there so rollback-isolated tests see a committed schema.
- Wrap a returned pool with `dbtx.New(db)` when testing transaction-aware repositories.

## Options

| Field | Default | Purpose |
| --- | --- | --- |
| `Driver` | `postgres` | Registered `database/sql` driver name. |
| `Image` | `postgres:latest` | Container image; pin it for repeatable tests. |
| `Duration` | 10 minutes | Container expiry safety limit; increase for longer suites. |
| `Hook` | No-op | `func(dsn string) error` for migrations/setup. |

Despite the driver option, the container helper starts PostgreSQL; this is not
a generic MySQL/SQLite container launcher. For schemas using `uuidv7()`, choose
PostgreSQL 18+.

## Run the package tests

Requires Go 1.27+ and a running Docker daemon (`docker info`). From the
repository root:

```bash
cd testing/dbtest
go test -race -count=1 ./...
go vet ./...
```

Tests start `postgres:17.4` on a dynamically assigned port and clean up their
containers. They do not use the root Compose database or `DATABASE_URL`. The
first run needs network access to download dependencies and the image. See the
[root development guide](../../README.md#development-and-verification) for all-module checks.

## License

[MIT](../../LICENSE).
