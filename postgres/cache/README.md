# Postgres Cache

A PostgreSQL-backed, generic, typed cache built on [dbtx](https://github.com/alextanhongpin/dbtx).  
The cache stores JSON values with an optional TTL, supports atomic operations, prefix namespacing, and negative caching.

The underlying table is `dbtx.cache`:

```sql
create schema if not exists dbtx;
create UNLOGGED table if not exists dbtx.cache(
  key text,
  value jsonb not null,
  digest text not null,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  expires_at timestamptz,
  primary key (key)
);
```

## Install

```bash
go get github.com/alextanhongpin/dbtx/postgres/cache
```

## Migration

The schema for `dbtx.cache` lives in [`repository/schema.sql`](repository/schema.sql) and is exported as `repository.Schema`. Run it once during app startup:

```go
_, err := db.ExecContext(ctx, repository.Schema)
```

This creates the `dbtx.cache` UNLOGGED table used by the cache.

## Quick start

```go
import (
    "github.com/alextanhongpin/dbtx/postgres/cache"
    "github.com/alextanhongpin/dbtx/postgres/cache/repository"
)

c := cache.New(repository.New(db), cache.WithPrefix("books:"))

// Store
err := c.Store(ctx, id.String(), book, time.Minute)

// Load
book, err := c.Load[Book](ctx, id.String())
if errors.Is(err, cache.ErrNotExist) { /* miss */ }

// Load or store atomically
val, loaded, err := c.LoadOrStore(ctx, key, value, ttl)
```

### Prefix

All keys are prefixed with the `WithPrefix` option (read back with `Prefix`). No separator is added, so include one. Useful for multi-tenant repositories:

```go
c := cache.New(repo, cache.WithPrefix("books:"))
```

## API highlights

* `Migrate(ctx)` – run embedded schema to create `dbtx.cache`
* `Store(ctx, key, value, ttl)` – write value with TTL (`cache.NoExpiration` for none)
* `StoreOnce(ctx, key, value, ttl)` – write only if absent → `cache.ErrExists`
* `Load[T](ctx, key)` – typed read
* `LoadAndDelete[T](ctx, key)` – get and remove
* `Delete(ctx, key)` – remove
* `Exists(ctx, key)` – existence check, auto-invalidates expired entries
* `TTL(ctx, key)` – remaining time, `cache.NoExpiration` if none, `cache.ErrNotExist` if missing
* `Expire(ctx, key, ttl)` – extend/lock TTL
* `CompareAndSwap(ctx, key, old, value, ttl)` – write only if value matches → `cache.ErrConflict` on mismatch
* `CompareAndDelete(ctx, key, old)` – delete only if value matches → `cache.ErrConflict` on mismatch
* `LoadOrStore(ctx, key, value, ttl)` – atomic load-or-write
* `LoadOrCreate(ctx, key, fn)` – load or compute via `fn`; the key is leased while `fn` runs, so concurrent callers get `cache.ErrRequestInFlight`
* `Purge(ctx)` – delete expired rows (all prefixes)

Errors: `ErrNotExist`, `ErrConflict`, `ErrExists`.

Values are JSON marshalled with deterministic map ordering and hashed with xxh3 for CAS.

## Example: repository with negative caching

From `examples_test.go`:

```go
type BookRepository struct {
    *cache.Cache
}

func NewBookRepository(db *sql.DB) *BookRepository {
    return &BookRepository{Cache: cache.New(db)}
}

func (r *BookRepository) Find(ctx context.Context, id uuid.UUID) (*Book, bool, error) {
    key := id.String()
    b, err := r.Load[*Book](ctx, key)
    if err == nil {
        if b.ID == uuid.Nil() {
            return nil, true, ErrNegativeCacheHit
        }
        return b, true, nil
    }

    b, err = r.DB.RunInTx2(ctx, func(ctx context.Context) (*Book, error) {
        if err := lock.Lock(ctx, lock.NewStrKey(key)); err != nil { return nil, err }
        var title string
        err = r.DBTx(ctx).QueryRowContext(ctx, `select title from books where id=$1`, id).Scan(&title)
        if errors.Is(err, sql.ErrNoRows) {
            // negative cache to avoid thundering herd
            return &Book{}, nil
        }
        b := &Book{ID: id, Title: title}
        _ = r.Store(ctx, key, b, time.Second)
        return b, nil
    })
    ...
}
```

Create caches atomically in the same transaction:

```go
func (r *BookRepository) Create(ctx context.Context, title string) (*Book, error) {
    return r.DB.RunInTx2(ctx, func(ctx context.Context) (*Book, error) {
        var id uuid.UUID
        err := r.DBTx(ctx).QueryRowContext(ctx,
            `insert into books (title) values ($1) returning id`, title).Scan(&id)
        b := &Book{ID: id, Title: title}
        err = r.Store(ctx, b.ID.String(), b, time.Second)
        return b, nil
    })
}
```

Delete invalidates cache:

```go
err = r.Cache.Delete(ctx, id.String())
```

## Function caching helpers

`func.go` provides decorators:

```go
type FuncConfig[K,V] struct {
    Cache *cache.Cache
    KeyFn func(ctx context.Context, req K) (string, error)
}

fnCached := cache.Func(fetch, &cache.FuncConfig{
    Cache: c,
    KeyFn: func(ctx context.Context, req MyReq) (string, error) { return req.ID, nil },
})

res, loaded, err := fnCached(ctx, req)
```

`Idempotent` additionally ensures the cached response matches the request hash, returning `cache.ErrConflict` on mismatch.

## Notes

* Expired entries are lazily invalidated on access and can be purged with `Cleanup`.
* Negative caching is manual – store a zero value with a short TTL as shown in the example.
* All operations use `dbtx` transaction helpers, so they can be run inside `RunInTx`/`RunInTx2`.
