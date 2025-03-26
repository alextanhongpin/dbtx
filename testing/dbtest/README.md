# dbtest

`dbtest` is a Go package designed to simplify database testing in Go. It provides utilities for initializing databases, managing transactions, and ensuring clean test environments.

## Features

- Initialize a global database for all tests.
- Initialize a database per test for isolation.
- Support for both pooled connections (`dbtx.DB`) and transactional connections (`dbtx.Tx`).

## Installation

To install the package, run:

```bash
go get github.com/alextanhongpin/dbtx/testing/dbtest
```

## Usage

### 1. Initialize a Global Database

You can initialize a global database that will be shared across all tests. This is useful for reducing setup overhead.

```go
package main

import (
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/testing/dbtest"
)

func TestMain(m *testing.M) {
	// Initialize the global database.
	close := dbtest.Init(dbtest.Options{
		Driver:   "postgres",
		Image:    "postgres:latest",
		Duration: 10 * time.Minute,
	})
	defer close()

	// Run tests.
	m.Run()
}

func TestGlobalDB(t *testing.T) {
	db := dbtest.DB(t) // Get a pooled connection.
	_, err := db.Exec("CREATE TABLE users (id SERIAL PRIMARY KEY, name TEXT)")
	if err != nil {
		t.Fatal(err)
	}
}
```

### 2. Initialize a Database Per Test

For better test isolation, you can initialize a new database for each test.

```go
package main

import (
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/testing/dbtest"
)

func TestPerTestDB(t *testing.T) {
	client := dbtest.New(t, dbtest.Options{
		Driver:   "postgres",
		Image:    "postgres:latest",
		Duration: 10 * time.Minute,
	})

	db := client.DB(t) // Get a pooled connection.
	_, err := db.Exec("CREATE TABLE users (id SERIAL PRIMARY KEY, name TEXT)")
	if err != nil {
		t.Fatal(err)
	}
}
```

### 3. Using `dbtx.DB` and `dbtx.Tx`

The `dbtest` package provides two types of database connections:

- **`dbtest.DB(t)`**: Returns a pooled connection (`*sql.DB`). Use this for operations that do not require transactions.
- **`dbtest.Tx(t)`**: Returns a transactional connection (`*sql.DB`) using `txdb`. Use this for operations that require transactions.

#### Example:

```go
package main

import (
	"testing"

	"github.com/alextanhongpin/dbtx/testing/dbtest"
)

func TestDBAndTx(t *testing.T) {
	// Initialize the global database.
	db := dbtest.DB(t) // Pooled connection.
	_, err := db.Exec("CREATE TABLE users (id SERIAL PRIMARY KEY, name TEXT)")
	if err != nil {
		t.Fatal(err)
	}

	// Use a transactional connection.
	tx := dbtest.Tx(t)
	_, err = tx.Exec("INSERT INTO users (name) VALUES ($1)", "Alice")
	if err != nil {
		t.Fatal(err)
	}

	// Verify the data within the transaction.
	row := tx.QueryRow("SELECT name FROM users WHERE id = $1", 1)
	var name string
	if err := row.Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Alice" {
		t.Fatalf("expected name to be Alice, got %s", name)
	}
}
```

### 4. Why Use `dbtx.Tx`?

Using `dbtx.Tx` is particularly useful in testing scenarios where you want to isolate changes made during a test. Since `dbtx.Tx` uses `txdb`, a transactional database driver, all operations performed within the connection are automatically rolled back when the connection is closed. This eliminates the need to manually rollback transactions in every test, ensuring a clean database state for subsequent tests.

#### Example:

```go
package main

import (
	"testing"

	"github.com/alextanhongpin/dbtx/testing/dbtest"
)

func TestTxIsolation(t *testing.T) {
	// Use a transactional connection.
	tx := dbtest.Tx(t)

	// Perform operations within the transaction.
	_, err := tx.Exec("INSERT INTO users (name) VALUES ($1)", "Alice")
	if err != nil {
		t.Fatal(err)
	}

	// Verify the data within the transaction.
	row := tx.QueryRow("SELECT name FROM users WHERE id = $1", 1)
	var name string
	if err := row.Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Alice" {
		t.Fatalf("expected name to be Alice, got %s", name)
	}

	// No need to manually rollback; the transaction will be rolled back automatically
	// when the connection is closed at the end of the test.
}
```

### Benefits of `dbtx.Tx`

- **Automatic Rollback**: Transactions are automatically rolled back when the connection is closed, ensuring no changes persist beyond the test.
- **Test Isolation**: Each test runs in its own isolated transaction, preventing interference between tests.
- **Simplified Cleanup**: No need to write explicit rollback logic in your tests.

## Options

The `dbtest.Options` struct allows you to configure the database initialization:

```go
type Options struct {
	Driver   string        // Database driver (default: "postgres").
	Duration time.Duration // Duration for the test container (default: 10 minutes).
	Hook     func(dsn string) error // Optional hook to run after database initialization.
	Image    string        // Docker image for the database (default: "postgres:latest").
}
```

## License

This project is licensed under the MIT License. See the LICENSE file for details.
