# dbtx/postgres/lock

PostgreSQL advisory locks for Go, built on top of `dbtx`. Provides transaction-scoped advisory locking via `pg_advisory_xact_lock` and `pg_try_advisory_xact_lock`.

Advisory locks are application-defined locks stored in PostgreSQL. They are not tied to rows or tables and are automatically released at the end of the transaction (`*_xact_lock`). This package makes it easy to serialize work within a transaction by key.

## Installation

This package is part of `github.com/alextanhongpin/dbtx`. Import the lock package:

```go
import "github.com/alextanhongpin/dbtx/postgres/lock"
```

Requires PostgreSQL 9.0+ and a `dbtx` transaction context.

## Usage

Advisory locks must be acquired inside a `dbtx` transaction. The lock is released automatically when the transaction commits or rolls back.

### Lock – blocking

`Lock` waits until the lock is acquired. Use it when you want serialization.

```go
import (
    "context"
    "github.com/alextanhongpin/dbtx"
    "github.com/alextanhongpin/dbtx/postgres/lock"
)

db := dbtx.New(myDB)
err := db.RunInTx(ctx, func(ctx context.Context) error {
    if err := lock.Lock(ctx, "user:123"); err != nil {
        return err
    }
    // critical section, only one transaction at a time holds this lock
    return nil
})
```

### TryLock – non-blocking

`TryLock` returns immediately. The second return value is `true` if the lock was acquired, `false` if it was already held by another transaction.

```go
err := db.RunInTx(ctx, func(ctx context.Context) error {
    ok, err := lock.TryLock(ctx, "user:123")
    if err != nil {
        return err
    }
    if !ok {
        // already locked, skip or retry
        return nil
    }
    // work
    return nil
})
```

### Named variants

If you use multiple `dbtx` instances with different IDs, use `NamedLock` / `NamedTryLock`:

```go
err := lock.NamedLock(ctx, myID, "key")
ok, err := lock.NamedTryLock(ctx, myID, "key")
```

`Lock` and `TryLock` are wrappers around `NamedLock` / `NamedTryLock` using `dbtx.ID`.

## Keys

The package supports four key types via the `key` interface:

* `int64` – direct bigint advisory lock `pg_advisory_xact_lock(bigint)`
* `string` – hashed to `int64` with FNV-1a 64-bit, `Hash64`
* `Pair[int32]` – two integers for `pg_advisory_xact_lock(int,int)`
* `Pair[string]` – two strings hashed to `int32` with FNV-1a 32-bit, `Hash32`

```go
lock.Lock(ctx, int64(42))
lock.Lock(ctx, "resource-name")
lock.Lock(ctx, lock.Pair[int32]{1, 2})
lock.Lock(ctx, lock.Pair[string]{"service", "resource"})
```

Hashing ensures string keys fit into the integer space required by PostgreSQL while remaining deterministic.

Example:

```go
fmt.Println(lock.Hash32("hello world"))  // -712294489
fmt.Println(lock.Hash64("hello world"))  // 8618312879776256743
```

### Pair

```go
p := lock.Pair[string]{"Foo", "Bar"}
// String() → "Foo, Bar"
```

## Error handling

* `ErrLockOutsideTx` is returned when the context does not contain a `dbtx` transaction. Locks must be acquired inside `db.RunInTx` / `RunInTx` and are transaction-scoped.
* Advisory locks are released automatically at transaction end. Do not hold them across transactions.

## Use cases

* **Serialize updates to the same logical entity** – e.g., lock `user:123` while updating a user profile to prevent race conditions.
* **Distributed mutual exclusion** – multiple services or workers share PostgreSQL, use an advisory lock keyed by job ID or resource name.
* **Idempotency guard** – lock on a request ID to prevent duplicate processing.
* **Rate limiting / throttling** – hold a lock for a hot key while a background job completes.
* **Composite locking** – use `Pair[string]` / `Pair[int32]` to lock on two dimensions, e.g., `(tenant, entity)`.

## Concurrency behavior

* `Lock` blocks until the lock is granted or the transaction ends.
* `TryLock` is non-blocking and returns `false` when the key is already held by another transaction in the same database.
* Locks are per-database connection, and `*_xact_lock` variants are scoped to the transaction. They are not session locks.

## Notes

* Advisory locks do not prevent conflicting SQL statements; they are purely application-level coordination.
* The lock is automatically released on `COMMIT` or `ROLLBACK`. Use `pg_advisory_lock` for session-scoped locks if you need different semantics – this package uses `*_xact_lock` intentionally.
* Hash collisions are theoretically possible but extremely unlikely with FNV-1a for practical workloads.
