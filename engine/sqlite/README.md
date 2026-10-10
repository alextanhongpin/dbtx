# SQLite engine

Opens a SQLite `*sql.DB` using the pure Go `modernc.org/sqlite` driver and
registers a `json_containment` SQL function. This module requires Go 1.26.5+.

## Install and run

In a new application directory:

```bash
go mod init example.com/sqlite-example
go get github.com/alextanhongpin/dbtx/engine/sqlite
```

Save as `main.go`:

```go
package main

import (
	"fmt"
	"github.com/alextanhongpin/dbtx/engine/sqlite"
	"log"
)

func main() {
	db, err := sqlite.New("example.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	var matches bool
	err = db.QueryRow(`SELECT json_containment(?, ?)`,
		`{"name":"Alice","active":true}`, `{"active":true}`).Scan(&matches)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(matches)
}
```

Run `go run .`; expected output is `true`. The process creates `example.db` in
its working directory; the parent directory must exist and be writable. No
Docker, PostgreSQL, or package schema migration is needed. You can pass the
pool to `dbtx.New(db)` after installing the root module, which requires Go 1.27+.

## Connection behavior

`New` pings the database and configures WAL, foreign keys, a five-second busy
timeout, `synchronous=NORMAL`, and one open/idle connection. It appends its own
DSN options, so pass a database path rather than a DSN with query parameters.
The pool limit avoids competing writers; use the transaction connection for
queries inside a transaction rather than opening another query on the pool.

`json_containment` accepts JSON **objects**, checking that every key/value in
the second object equals the corresponding value in the first. Nested values
are compared by equality, not recursive PostgreSQL-style containment. NULL
arguments return false; invalid JSON returns an error. This package does not
register a vector extension.

## Verify the module

From the repository root:

```bash
cd engine/sqlite
go test ./...
go vet ./...
```

There are currently no test files; these commands check compilation and static
analysis. The complete example above exercises the database and custom function.
