# dbtx

[![Go Reference](https://pkg.go.dev/badge/github.com/alextanhongpin/dbtx.svg)](https://pkg.go.dev/github.com/alextanhongpin/dbtx)
[![Go Report Card](https://goreportcard.com/badge/github.com/alextanhongpin/dbtx)](https://goreportcard.com/report/github.com/alextanhongpin/dbtx)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A comprehensive Go library that provides unified database transaction management across multiple database drivers and ORM libraries. `dbtx` simplifies transaction handling, provides testing utilities, and implements common database patterns like outbox and distributed locking.

## 🚀 Quick Start

```bash
go get github.com/alextanhongpin/dbtx
```

```go
package main

import (
    "context"
    "database/sql"
    "log"
    
    "github.com/alextanhongpin/dbtx"
    _ "github.com/lib/pq"
)

func main() {
    db, err := sql.Open("postgres", "postgres://user:pass@localhost/db?sslmode=disable")
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()
    
    atomic := dbtx.New(db)
    
    // Execute in transaction
    err = atomic.RunInTx(context.Background(), func(ctx context.Context) error {
        tx := atomic.DBTx(ctx)
        _, err := tx.ExecContext(ctx, "INSERT INTO users (name) VALUES ($1)", "John Doe")
        return err
    })
    if err != nil {
        log.Fatal(err)
    }
}
```

## ✨ Features

- **🔄 Unified Transaction Interface**: Common interface for `*sql.DB` and `*sql.Tx`
- **🎯 Multiple Database Support**: Works with `database/sql`, `pgx`, `bun`, and `sqlx`
- **🔒 Transaction Safety**: Automatic rollback on errors and panic recovery
- **🧪 Comprehensive Testing**: Built-in testing utilities with Docker containers
- **📦 Outbox Pattern**: Transactional outbox implementation for reliable messaging
- **🔐 Distributed Locking**: PostgreSQL-based advisory locks
- **🎭 Middleware Support**: Customizable database operation wrappers
- **🏗️ Nested Transactions**: Safe handling of nested transaction contexts

## 📚 Table of Contents

- [Core Library](#core-library)
  - [Basic Usage](#basic-usage)
  - [Transaction Management](#transaction-management)
  - [Custom Wrappers](#custom-wrappers)
- [Database Adapters](#database-adapters)
  - [pgx Support](#pgx-support)
  - [Bun ORM Support](#bun-orm-support)
  - [SQLx Support](#sqlx-support)
- [Advanced Features](#advanced-features)
  - [Outbox Pattern](#outbox-pattern)
  - [Distributed Locking](#distributed-locking)
- [Testing Utilities](#testing-utilities)
- [API Reference](#api-reference)
- [Contributing](#contributing)

## Core Library

### Basic Usage

The core `dbtx` package provides a unified interface for database operations:

```go
import (
    "database/sql"
    "github.com/alextanhongpin/dbtx"
    _ "github.com/lib/pq"
)

// Initialize with database connection
db, err := sql.Open("postgres", "postgres://user:pass@localhost/db?sslmode=disable")
if err != nil {
    log.Fatal(err)
}

atomic := dbtx.New(db)

// Execute queries directly
result, err := atomic.DB().ExecContext(ctx, "INSERT INTO users (name) VALUES ($1)", "Alice")
if err != nil {
    log.Fatal(err)
}
```

### Transaction Management

`dbtx` provides automatic transaction management with rollback on errors:

```go
err := atomic.RunInTx(context.Background(), func(ctx context.Context) error {
    tx := atomic.DBTx(ctx)
    
    // Both operations will be rolled back if either fails
    _, err := tx.ExecContext(ctx, "INSERT INTO users (name) VALUES ($1)", "Jane Doe")
    if err != nil {
        return err
    }
    
    _, err = tx.ExecContext(ctx, "INSERT INTO orders (user_id, amount) VALUES ($1, $2)", 1, 100)
    return err
})
if err != nil {
    log.Fatal(err)
}
```

### Custom Wrappers

Add middleware to database operations for logging, metrics, or other cross-cutting concerns:

```go
// Custom logger wrapper
logger := func(dbtx dbtx.DBTX) dbtx.DBTX {
    return &LoggingDBTX{dbtx: dbtx}
}

// Metrics wrapper
metrics := func(dbtx dbtx.DBTX) dbtx.DBTX {
    return &MetricsDBTX{dbtx: dbtx}
}

atomic := dbtx.New(db, logger, metrics)
```

## Database Adapters

### pgx Support

For applications using the high-performance `pgx` driver:

```go
import "github.com/alextanhongpin/dbtx/pgxtx"

conn, err := pgx.Connect(context.Background(), "postgres://user:pass@localhost/db")
if err != nil {
    log.Fatal(err)
}

atomic := pgxtx.New(conn)

err = atomic.RunInTx(context.Background(), func(ctx context.Context) error {
    tx := atomic.DBTx(ctx)
    _, err := tx.Exec(ctx, "INSERT INTO users (name) VALUES ($1)", "John")
    return err
})
```

### Bun ORM Support

Integration with the modern Bun ORM:

```go
import "github.com/alextanhongpin/dbtx/buntx"

db := bun.NewDB(sqldb, pgdialect.New())
atomic := buntx.New(db)

err = atomic.RunInTx(context.Background(), func(ctx context.Context) error {
    tx := atomic.DBTx(ctx)
    _, err := tx.NewInsert().Model(&User{Name: "Alice"}).Exec(ctx)
    return err
})
```

### SQLx Support

For projects using the popular `sqlx` extension:

```go
import "github.com/alextanhongpin/dbtx/sqlxtx"

db, err := sqlx.Connect("postgres", "postgres://user:pass@localhost/db")
if err != nil {
    log.Fatal(err)
}

atomic := sqlxtx.New(db)

err = atomic.RunInTx(context.Background(), func(ctx context.Context) error {
    tx := atomic.DBTx(ctx)
    _, err := tx.ExecContext(ctx, "INSERT INTO users (name) VALUES ($1)", "Bob")
    return err
})
```

## Advanced Features

### Outbox Pattern

Implement reliable messaging with the transactional outbox pattern:

```go
import "github.com/alextanhongpin/dbtx/postgres/outbox"

// Setup outbox table (run this SQL once)
const schema = `
CREATE TABLE outbox (
    id bigint GENERATED ALWAYS AS IDENTITY,
    aggregate_id text NOT NULL,
    aggregate_type text NOT NULL,
    type text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);`

// Create outbox instance
atomic := dbtx.New(db)
ob := outbox.New(atomic)

// Enqueue message in transaction
err := ob.RunInTx(ctx, func(txCtx context.Context) error {
    // Your business logic
    _, err := ob.DBTx(txCtx).ExecContext(txCtx, "INSERT INTO orders (...) VALUES (...)")
    if err != nil {
        return err
    }
    
    // Enqueue outbox message
    return ob.Create(txCtx, outbox.Message{
        AggregateID:   "order-123",
        AggregateType: "order",
        Type:          "order.created",
        Payload:       json.RawMessage(`{"order_id": "123"}`),
    })
})

// Process outbox messages
err = ob.RunInTx(ctx, func(txCtx context.Context) error {
    event, err := ob.LoadAndDelete(txCtx)
    if err != nil {
        if errors.Is(err, sql.ErrNoRows) {
            return nil // No messages to process
        }
        return err
    }
    
    // Process event...
    fmt.Printf("Processing event: %s\n", event.Type)
    return nil
})
```

### Distributed Locking

PostgreSQL advisory locks for distributed coordination:

```go
import "github.com/alextanhongpin/dbtx/postgres/lock"

err = atomic.RunInTx(ctx, func(ctx context.Context) error {
    tx := atomic.DBTx(ctx)
    
    // Acquire lock
    acquired, err := lock.TryLock(ctx, tx, "my-resource")
    if err != nil {
        return err
    }
    if !acquired {
        return errors.New("could not acquire lock")
    }
    
    // Critical section - only one process can execute this
    // Lock is automatically released when transaction ends
    
    return nil
})
```

## Testing Utilities

Comprehensive testing support with Docker containers and test databases:

```go
import (
    "github.com/alextanhongpin/dbtx/testing/dbtest"
    "github.com/alextanhongpin/dbtx/testing/testcontainer"
)

func TestWithDatabase(t *testing.T) {
    // Start PostgreSQL container
    db := testcontainer.MustPostgres(t, testcontainer.PostgresConfig{
        Database: "testdb",
        Username: "test",
        Password: "test",
    })
    defer db.Close()
    
    // Use test utilities
    dbtest.MustExec(t, db, "CREATE TABLE users (id SERIAL PRIMARY KEY, name TEXT)")
    
    atomic := dbtx.New(db)
    
    // Your tests...
}
```

## API Reference

### Core Interface

The `DBTX` interface provides a unified abstraction over database operations:

```go
type DBTX interface {
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
    PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
    QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
```

### UnitOfWork Interface

The main interface for transaction management:

```go
type UnitOfWork interface {
    DB() DBTX                                                              // Direct database access
    DBTx(ctx context.Context) DBTX                                        // Context-aware database access
    Tx(ctx context.Context) DBTX                                          // Transaction-only access (panics if not in tx)
    RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error // Execute function in transaction
}
```

### Method Differences

| Method | Description | Use Case |
|--------|-------------|----------|
| `DB()` | Returns wrapped `*sql.DB` | Non-transactional operations |
| `DBTx(ctx)` | Returns transaction if in context, otherwise DB | Context-aware operations |
| `Tx(ctx)` | Returns transaction, panics if not in transaction context | Transaction-required operations |

### Error Handling

- `ErrNotTransaction`: Returned when `Tx(ctx)` is called outside a transaction context

## Package Structure

```
dbtx/
├── dbtx.go              # Core transaction management
├── context.go           # Context utilities
├── logger.go            # Logging utilities
├── buntx/              # Bun ORM adapter
├── pgxtx/              # pgx driver adapter  
├── sqlxtx/             # sqlx adapter
├── postgres/
│   ├── outbox/         # Transactional outbox pattern
│   ├── lock/           # Advisory locks
│   └── violations/     # Constraint violation handling
└── testing/
    ├── dbtest/         # Database testing utilities
    ├── buntest/        # Bun-specific test utilities
    ├── pgxtest/        # pgx-specific test utilities
    ├── redistest/      # Redis testing utilities
    └── testcontainer/  # Docker container management
```

## Best Practices

### 1. Always Use Context

```go
// ✅ Good
err := atomic.RunInTx(ctx, func(txCtx context.Context) error {
    tx := atomic.DBTx(txCtx)
    return tx.ExecContext(txCtx, query, args...)
})

// ❌ Avoid
err := atomic.RunInTx(ctx, func(txCtx context.Context) error {
    tx := atomic.DBTx(txCtx)
    return tx.Exec(query, args...) // No context
})
```

### 2. Handle Errors Properly

```go
err := atomic.RunInTx(ctx, func(txCtx context.Context) error {
    tx := atomic.DBTx(txCtx)
    
    if _, err := tx.ExecContext(txCtx, query1, args1...); err != nil {
        return fmt.Errorf("failed to execute query1: %w", err)
    }
    
    if _, err := tx.ExecContext(txCtx, query2, args2...); err != nil {
        return fmt.Errorf("failed to execute query2: %w", err)
    }
    
    return nil
})
```

### 3. Use Appropriate Method

```go
// For operations that might or might not be in a transaction
tx := atomic.DBTx(ctx)

// For operations that MUST be in a transaction
tx := atomic.Tx(ctx) // Will panic if not in transaction

// For operations that should NOT be in a transaction
db := atomic.DB()
```

## Contributing

We welcome contributions! Please see our [Contributing Guidelines](CONTRIBUTING.md) for details.

### Development Setup

```bash
# Clone the repository
git clone https://github.com/alextanhongpin/dbtx.git
cd dbtx

# Run tests
make test

# Run tests with Docker containers
make test-integration

# Lint code
make lint
```

### Running Tests

```bash
# Unit tests
go test ./...

# Integration tests (requires Docker)
go test -tags=integration ./...

# Specific package tests
go test ./buntx/
go test ./pgxtx/
go test ./testing/dbtest/
```

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

- Inspired by the need for consistent transaction management across different Go database libraries
- Built with lessons learned from production database applications
- Thanks to the Go community for excellent database libraries like `pgx`, `bun`, and `sqlx`

---

**Made with ❤️ for the Go community**
````
