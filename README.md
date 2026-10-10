# dbtx

[![Go Reference](https://pkg.go.dev/badge/github.com/alextanhongpin/dbtx.svg)](https://pkg.go.dev/github.com/alextanhongpin/dbtx)

`dbtx` manages `database/sql` transactions through context. Repositories use
`DBTx(ctx)` to join the caller's transaction, or use the connection pool when no
transaction is present. PostgreSQL helpers and Docker-backed test utilities live
in separate Go modules.

## Requirements

- Go **1.27.0 or newer** for the root module and PostgreSQL packages in this checkout.
  See each module's `go.mod` for its declared minimum.
- Docker with a running daemon for database integration tests and the local
  PostgreSQL quick start. Check connectivity with `docker info`.
- A C compiler and `CGO_ENABLED=1` for `postgres/dbt` and race tests. On macOS,
  install the Command Line Tools; on Linux, install your distribution's C build tools.
- Network access for the first dependency download and Docker image pull.

This is a library repository; it has no application server or CLI to start.
Examples below are application code. Package READMEs link to executable tests.

## Run a complete example

From the repository root, start the database defined in [compose.yaml](compose.yaml):

```bash
docker compose up -d db
docker compose exec db pg_isready -U john -d dev
```

Wait until `pg_isready` reports that the database accepts connections. Compose
uses `postgres:19beta3-alpine3.24`, exposes `127.0.0.1:5432`, and stores data in
`./tmp`. This setup and its credentials are for local development.

Create a separate application directory outside this repository:

```bash
mkdir dbtx-example
cd dbtx-example
go mod init example.com/dbtx-example
go get github.com/alextanhongpin/dbtx github.com/lib/pq
```

Save this as `main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	"github.com/alextanhongpin/dbtx"
	_ "github.com/lib/pq"
)

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
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS dbtx_example_users (
        id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        name text NOT NULL
    )`); err != nil {
		log.Fatal(err)
	}

	atomic := dbtx.New(db)
	var id int64
	err = atomic.RunInTx(ctx, func(ctx context.Context) error {
		return atomic.DBTx(ctx).QueryRowContext(ctx,
			"INSERT INTO dbtx_example_users (name) VALUES ($1) RETURNING id",
			"Alice").Scan(&id)
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("committed user %d\n", id)
}
```

Run it from the application directory:

```bash
export DATABASE_URL='postgres://john:123456@127.0.0.1:5432/dev?sslmode=disable'
go run .
```

Each run inserts a row and prints `committed user <id>`. To use the current
checkout instead of the downloaded root module, run
`go mod edit -replace github.com/alextanhongpin/dbtx=/absolute/path/to/dbtx`
followed by `go mod tidy` in the application directory. Nested modules require
separate replacements when you want to use their local source.

Stop the local database from the repository root with `docker compose down`.
The bind-mounted `tmp` directory is retained.

## Transactions

| Method | Behavior |
| --- | --- |
| `DB()` | Returns the wrapped connection pool. |
| `DBTx(ctx)` | Returns the transaction in context, otherwise the pool. |
| `Tx(ctx)` | Requires a transaction in context; panics with `ErrNotTransaction` otherwise. |
| `RunInTx(ctx, fn)` | Starts and commits a transaction; rolls back on error or panic, then re-panics. Joins an existing transaction with the same ID. |
| `RunInSubTx(ctx, fn)` | Uses a savepoint inside an existing transaction; returns `ErrOutOfTx` without one. |
| `RunInTx2(ctx, fn)` | Like `RunInTx`, with a typed result and error. |
| `RunInSubTx2(ctx, fn)` | Like `RunInSubTx`, with a typed result and error. |

Always pass the callback's `ctx` to repositories and queries. Nested `RunInTx`
calls join the outer transaction; only its owner commits. Handle an inner
savepoint error inside the outer callback if you want the outer work to commit.
Returning that error from the outer callback rolls back the whole transaction.

```go
err := atomic.RunInTx(ctx, func(ctx context.Context) error {
    err := atomic.RunInSubTx(ctx, func(ctx context.Context) error {
        _, err := atomic.DBTx(ctx).ExecContext(ctx, query, args...)
        return err
    })
    if err != nil {
        // Decide whether this failure is recoverable before continuing.
        return err
    }
    return nil
})
```

Here `atomic`, `ctx`, `query`, and `args` come from your application. Configure
isolation with `dbtx.WithTxOptions(ctx, &sql.TxOptions{...})`. For multiple
databases, call `SetID` with a distinct ID per database before sharing clients;
repositories joining the same database must use the same ID. Context lookup is
by ID, not by connection identity.

## Query wrappers

`dbtx.New(db, wrappers...)` accepts `func(dbtx.DBTX) dbtx.DBTX` middleware.
The bundled logger implements this wrapper:

```go
atomic := dbtx.New(db, dbtx.WithLogger(logger))
```

`logger` must provide `Log(ctx context.Context, method, query string, args ...any)`
as required by [WithLogger](logger.go). See [dbtx_test.go](dbtx_test.go)
for a complete implementation. The `DBTX` interface contains `ExecContext`,
`PrepareContext`, `QueryContext`, and `QueryRowContext`; it deliberately omits
commit and rollback so repositories cannot finish the caller's transaction.

## Modules

Install each helper using its full module path, for example
`go get github.com/alextanhongpin/dbtx/postgres/outbox`.

| Module | Purpose |
| --- | --- |
| [postgres/ab](postgres/ab/README.md) | Two-variant experiments, sticky assignment, conversion recording and result interpretation. |
| [postgres/cache](postgres/cache/README.md) | JSON cache, TTL, atomic operations and compute leases. |
| [postgres/dbt](postgres/dbt/README.md) | Struct-driven PostgreSQL templates and scanning. |
| [postgres/idempotent](postgres/idempotent/README.md) | Durable requests, checkpoints and cached outcomes. |
| [postgres/inbox](postgres/inbox/README.md) | Deduplicated transactional message consumption. |
| [postgres/outbox](postgres/outbox/README.md) | Transactional event enqueueing and at-least-once publishing. |
| [postgres/jobs](postgres/jobs/README.md) | Background jobs with leases, retries and maintenance. |
| [postgres/jsonb](postgres/jsonb/README.md) | Typed scanning of JSON query results. |
| [postgres/lock](postgres/lock/README.md) | Transaction-scoped advisory locks. |
| [postgres/violations](postgres/violations/README.md) | Classify PostgreSQL errors from `lib/pq`. |
| [engine/sqlite](engine/sqlite/README.md) | SQLite pool defaults and JSON containment function. |
| [testing/dbtest](testing/dbtest/README.md) | PostgreSQL containers, pools and rollback-isolated tests. |
| [testing/redistest](testing/redistest/README.md) | Redis containers and test clients. |
| [testing/testcontainer](testing/testcontainer/README.md) | Low-level PostgreSQL container lifecycle. |

There are no `pgxtx`, `buntx`, or `sqlxtx` adapters in this checkout.

## Development and verification

```bash
git clone https://github.com/alextanhongpin/dbtx.git
cd dbtx
go test -race -count=1 ./...
go vet ./...
```

Root tests automatically start PostgreSQL 17.4 containers. Most PostgreSQL
helper suites start `postgres:19beta3-alpine3.24`; Redis tests use `redis:latest`.
They apply their own schemas and use dynamically assigned ports. You do not
need Compose, `DATABASE_URL`, or an `integration` build tag for these tests.
Even a filtered test may start Docker because the package has a `TestMain`.

The root `go test ./...` does **not** include nested modules. To test every
module from the repository root, stopping on the first failure:

```bash
set -e
for module in $(find . -name go.mod -not -path './tmp/*' | sort); do
    (cd "$(dirname "$module")" && go test -race -count=1 ./... && go vet ./...)
done
```

Each module resolves the dependency versions in its own `go.mod`; without
explicit replacements, tests use released dependencies rather than sibling
checkouts. `make test` also visits modules, writes coverage files, and opens an
HTML report, but use the loop above when you need failures to stop the run.

| Target | What it does |
| --- | --- |
| `make test` | Tests modules with race/coverage flags, then opens root coverage in a browser. |
| `make lint` | Formats SQL files in place using `go tool sqlfmt`; it is not a Go linter. |
| `make sqlc` | Regenerates repositories using a `sqlc` executable on `PATH`. |
| `make install` | Updates sqlfmt/sqlc tool dependencies in the current module. |

For a repository with `sqlc.yaml`, run `go tool sqlc generate` in that repository
directory to use the tool registered in its parent module. Generated code is
checked in; regeneration is not required to execute examples or tests.

If a test cannot connect to Docker, check `docker info` and the active Docker
context. Missing image or dependency downloads require network access. A
`uuidv7()` error means the database used for that schema must be PostgreSQL 18+.

## Codex Skills

This repository includes reusable skills for integrating dbtx into Go applications:

- [dbtx-integrate](skills/dbtx-integrate/SKILL.md): repository wiring, transaction boundaries, sqlc, savepoints, and database tests.
- [dbtx-postgres-patterns](skills/dbtx-postgres-patterns/SKILL.md): outbox, inbox, durable idempotency, jobs, locks, caching, and SQL helpers.

Copy the complete skill folders into a consuming project's `.agents/skills/` directory for project-scoped use, or into `~/.codex/skills/` for personal use. From this checkout, for example:

```bash
mkdir -p /path/to/your-go-project/.agents/skills
cp -R skills/dbtx-integrate skills/dbtx-postgres-patterns /path/to/your-go-project/.agents/skills/
```

Example prompts:

```text
Use $dbtx-integrate to integrate dbtx into this project's database/sql repositories.
Use $dbtx-postgres-patterns to enqueue order events atomically with our business writes.
```

The skills instruct the agent to inspect the dependency version used by the consuming project. Installing a skill does not install Go dependencies or apply database migrations.

## License

[MIT](LICENSE).
