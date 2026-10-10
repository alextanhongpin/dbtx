# PostgreSQL test containers

Low-level Docker lifecycle helper for PostgreSQL. Most tests should use
[dbtest](../dbtest/README.md), which adds SQL clients and `t.Cleanup` handling.

## Requirements and installation

Requires Go 1.24.2+, a running Docker daemon (`docker info`), and network access
for the initial image pull. Ports are dynamically assigned; no local database
or Compose service is needed.

```bash
go get github.com/alextanhongpin/dbtx/testing/testcontainer github.com/lib/pq
```

## Complete example

Save this as `container_test.go` in a new application module
(`go mod init example.com/container-example`) after installing dependencies:

```go
package example_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/testing/testcontainer"
	_ "github.com/lib/pq"
)

func TestPostgres(t *testing.T) {
	result, err := testcontainer.Run("postgres:17.4", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := result.Stop(); err != nil {
			t.Error(err)
		}
	})
	db, err := sql.Open("postgres", result.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d", n)
	}
}
```

Run `go test -v -count=1 ./...`; expect `TestPostgres` to pass. Apply your
application schema through the returned DSN before using application queries.

`Run(image, expiry)` returns `*RunResult` with `DSN` and `Stop func() error`.
It waits for readiness and sets container expiry as a safety limit. The caller
owns cleanup after a successful start. This helper starts PostgreSQL with a
test user/database and local testing credentials; use it for disposable tests.

## Verify the module

From the repository root:

```bash
cd testing/testcontainer
go test ./...
go vet ./...
```

There are currently no module test files, so these checks only compile/analyze
code. The complete example above exercises Docker startup and connectivity.
