# PostgreSQL constraint errors

Helpers for inspecting PostgreSQL SQLSTATE codes from `github.com/lib/pq`.
Wrapped errors are supported through `errors.As`; errors from other drivers
(such as pgx) are not recognized as `*pq.Error`.

## Install and use

Requires Go 1.27+.

```bash
go get github.com/alextanhongpin/dbtx/postgres/violations
```

Save this complete example as `main.go` in a new application module
(`go mod init example.com/violations-example`) after installing the dependency:

```go
package main

import (
	"fmt"
	"github.com/alextanhongpin/dbtx/postgres/violations"
	"github.com/lib/pq"
)

func main() {
	err := fmt.Errorf("insert user: %w", &pq.Error{Code: "23505"})
	fmt.Println(violations.IsUnique(err))
}
```

Run `go run .`; expected output is `true`. No database is needed for this
example. In an application, pass the error returned by a SQL statement.

`As(err)` returns `(*pq.Error, bool)`, giving access to `Constraint`, `Table`,
and `Detail`. `IsCode(err, code)` tests a specific code. Helpers include
`IsUnique`, `IsForeignKey`, `IsNotNull`, `IsCheck`, `IsExclusion`,
`IsRestrict`, and `IsIntegrityConstraint`. They compare exact codes;
`IsIntegrityConstraint` checks `23000`, not every code in SQLSTATE class 23.
`IsTriggerException` checks the package's `P0000` constant; use `IsCode` for
other trigger codes.

## Verify the module

From the repository root:

```bash
cd postgres/violations
go test ./...
go vet ./...
```

This module currently has no test files; these commands check compilation and
static analysis. No Docker or migrations are required.
