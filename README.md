# dbtx

`dbtx` is a Go package that provides a unified interface for working with database operations (`*sql.DB` and `*sql.Tx`) and simplifies transaction management. It allows you to execute queries, manage transactions, and wrap database operations with custom logic.

## Installation

To install the package, run:

```bash
go get github.com/alextanhongpin/dbtx
```

## Features

- Unified interface (`DBTX`) for `*sql.DB` and `*sql.Tx`.
- Transaction management with `RunInTx`.
- Customizable database operation wrappers.

## Usage

### 1. Initialize `Atomic`

The `Atomic` struct is the main entry point for managing database operations. You can initialize it with a `*sql.DB` instance and optional wrapper functions.

```go
import (
	"database/sql"
	"github.com/alextanhongpin/dbtx"

	_ "github.com/lib/pq"
)

db, _ := sql.Open("postgres", "user:password@/dbname")
atomic := dbtx.New(db)
```

### 2. Execute Queries

You can use the `DB()` method to execute queries directly on the database.

```go
db := atomic.DB()
result, err := db.Exec("INSERT INTO users (name) VALUES (?)", "John Doe")
if err != nil {
	log.Fatal(err)
}
```

### 3. Transaction Management

Use `RunInTx` to wrap operations in a transaction. If an error occurs, the transaction will be rolled back automatically.

```go
err := atomic.RunInTx(context.Background(), func(ctx context.Context) error {
	db := atomic.DBTx(ctx)
	_, err := db.Exec("INSERT INTO users (name) VALUES (?)", "Jane Doe")
	if err != nil {
		return err
	}

	_, err = db.Exec("INSERT INTO orders (user_id, amount) VALUES (?, ?)", 1, 100)
	return err
})
if err != nil {
	log.Fatal(err)
}
```

### 4. Custom Wrappers

You can provide custom wrapper functions to modify the behavior of database operations.

```go
logger := func(dbtx dbtx.DBTX) dbtx.DBTX {
	return &LoggingDBTX{dbtx: dbtx}
}

atomic := dbtx.New(db, logger)
```

### 5. Nested Transactions

The `Tx()` method ensures that only the parent transaction can commit, preventing nested transactions from committing prematurely.

```go
err := atomic.RunInTx(context.Background(), func(ctx context.Context) error {
	tx := atomic.Tx(ctx)
	// Perform operations with tx
	return nil
})
```

### 6. Using `atomic.Tx(ctx)`

The `Tx(ctx)` method retrieves the current transaction (`*sql.Tx`) from the context. This is useful when you want to ensure that operations are performed within an existing transaction. If the context does not contain a transaction, it will panic with `ErrNotTransaction`.

```go
err := atomic.RunInTx(context.Background(), func(ctx context.Context) error {
	tx := atomic.Tx(ctx) // Retrieve the current transaction
	_, err := tx.Exec("UPDATE users SET name = ? WHERE id = ?", "John Doe", 1)
	if err != nil {
		return err
	}

	// Perform additional operations within the same transaction
	_, err = tx.Exec("INSERT INTO logs (message) VALUES (?)", "User updated")
	return err
})
if err != nil {
	log.Fatal(err)
}
```

### Differences Between `atomic.DB`, `atomic.DBTx`, and `atomic.Tx`

- **`atomic.DB()`**: Returns the underlying `*sql.DB` wrapped with any custom functions. Use this for operations that do not require a transaction.

- **`atomic.DBTx(ctx)`**: Returns the database interface (`DBTX`) from the context. If the context contains a transaction (`*sql.Tx`), it will return the transaction. Otherwise, it falls back to the underlying `*sql.DB`.

- **`atomic.Tx(ctx)`**: Returns the current transaction (`*sql.Tx`) from the context. This ensures that the operation is performed within a transaction. If the context does not contain a transaction, it panics with `ErrNotTransaction`.

#### Example:

```go
ctx := context.Background()

// Using atomic.DB()
db := atomic.DB()
_, err := db.Exec("INSERT INTO users (name) VALUES (?)", "Alice")
if err != nil {
	log.Fatal(err)
}

// Using atomic.DBTx(ctx)
err = atomic.RunInTx(ctx, func(ctx context.Context) error {
	dbtx := atomic.DBTx(ctx)
	_, err := dbtx.Exec("INSERT INTO users (name) VALUES (?)", "Bob")
	return err
})
if err != nil {
	log.Fatal(err)
}

// Using atomic.Tx(ctx)
err = atomic.RunInTx(ctx, func(ctx context.Context) error {
	tx := atomic.Tx(ctx)
	_, err := tx.Exec("INSERT INTO users (name) VALUES (?)", "Charlie")
	return err
})
if err != nil {
	log.Fatal(err)
}
```

## Interfaces

### DBTX

The `DBTX` interface abstracts common database operations:

```go
type DBTX interface {
	Exec(query string, args ...any) (sql.Result, error)
	Prepare(query string) (*sql.Stmt, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row

	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
```

### atomic

The `atomic` interface defines methods for managing database operations and transactions:

```go
type atomic interface {
	DB() DBTX
	DBTx(ctx context.Context) DBTX
	Tx(ctx context.Context) DBTX
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}
```

## Error Handling

- `ErrNotTransaction`: Raised when attempting to access a transaction from a non-transactional context.

## License

This project is licensed under the MIT License. See the LICENSE file for details.
````
