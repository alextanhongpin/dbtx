# dbt

A tiny Go helper for type-safe PostgreSQL query building using struct tags and `text/template`.

`dbt` compiles a query template into a PostgreSQL statement with positional parameters `$1,$2,…` and derives column lists from Go structs. It is meant for the boring CRUD parts where the shape of a row is known at compile time: `INSERT … RETURNING`, `UPDATE … SET … RETURNING`, and `SELECT` with optional aggregation via embedded structs.

## Installation

```bash
go get github.com/alextanhongpin/dbtx/postgres/dbt
```

Requires Go 1.21+. The package uses `pg_query_go` to parse and normalize SQL on creation, so it is PostgreSQL-specific.

## Quick start

```go
type User struct {
    ID        int
    Name      string
    Email     string
    CreatedAt time.Time
    UpdatedAt time.Time
}

type CreateUserParams struct {
    Name  string
    Email string
}

q := dbt.Must(dbt.New[CreateUserParams, User](`
    INSERT INTO users {{ vals }} RETURNING {{ cols }}
`))

fmt.Println(q.String())
// INSERT INTO users (name, email) VALUES ($1, $2) RETURNING id, name, email, created_at, updated_at

args, _ := q.Args(CreateUserParams{Name: "john", Email: "john@a.co"})
// [john john@a.co]
```

Execute with any `database/sql` compatible DB:

```go
rows, err := q.QueryContext(ctx, db, params)
one, err  := q.QueryRowContext(ctx, db, params)
res, err  := q.ExecContext(ctx, db, params)
```

The generic parameters are `K` for the parameter struct and `V` for the result struct.

## Template helpers

All helpers are evaluated at `New` time using reflection on the two type arguments.

* `{{ cols [operator ...args] }}` – column list for the result type `V`.
  * `{{ cols }}` → all fields
  * `{{ cols "-" "id" }}` → all except `id`
  * `{{ cols "=" "name" "email" }}` → only the listed fields
* `{{ set [operator ...args] }}` – `col = @col, …` assignments for the parameter type `K`.
* `{{ vals [operator ...args] }}` – `(col, …) VALUES (@col, …)` for inserts.

Column names are derived from struct fields:
* `json` tag is used, with optional `,inline` suffix to flatten.
* Anonymous embedded structs are also flattened.
* If no tag is present, `stringcase.ToSnake(FieldName)` is used.
* Nested fields are emitted as `parent.child` and aliased as `parent_child` in SELECTs.

Parameters in the template use `@name` syntax. They are rewritten to positional `$n` and collected in the order of first appearance. The final query and argument order are therefore deterministic.

## Examples

### Insert + returning

```go
type CreateUserParams struct{ Name, Email string }
q := dbt.Must(dbt.New[CreateUserParams, User]("INSERT INTO users {{ vals }} RETURNING {{ cols }}"))
```

### Update

```go
type UpdateUserParams struct{ ID int; Name string }
q := dbt.Must(dbt.New[UpdateUserParams, User]("UPDATE users SET {{ set "-" "id" }} WHERE id = @id RETURNING {{ cols }}"))
// UPDATE users SET name = $1 WHERE id = $2 RETURNING …
```

### Select with filters

```go
type Filter struct{ Name, Email string }
q := dbt.Must(dbt.New[Filter, User]("SELECT {{ cols }} FROM users WHERE name = @name AND email = @email"))
```

### Aggregates via embedded structs

```go
type UserBookAggregate struct {
    UserBook
    User `json:"u"`
    Book `json:"b"`
}
q := dbt.Must(dbt.New[any, UserBookAggregate]("SELECT {{ cols }} FROM user_books ub JOIN users u ON ub.user_id = u.id JOIN books b ON ub.book_id = b.id"))
// selects user_book_id AS user_book_id, u_id AS u_id, b_id AS b_id, …
```

See `dbt_examples_test.go` for runnable examples.

## API

```go
func New[K,V any](tmpl string) (*SQL[K,V], error)
func Parse[K,V any](tmpl string) (string, []string, error)

type SQL[K,V] struct { … }
func (s *SQL[K,V]) QueryContext(ctx, db DB, params K) ([]V, error)
func (s *SQL[K,V]) QueryRowContext(ctx, db DB, params K) (V, error)
func (s *SQL[K,V]) ExecContext(ctx, db DB, params K) (sql.Result, error)
func (s *SQL[K,V]) Args(k K) ([]any, error)
func (s *SQL[K,V]) Build(v K) (string, []any, error)
func (s *SQL[K,V]) String() string
```

`Must` panics on error and is convenient for init-time compilation.

`DB` is the minimal `database/sql` interface used by the package:
```go
type DB interface {
    ExecContext(context.Context, string, ...any) (sql.Result, error)
    QueryContext(context.Context, string, ...any) (*sql.Rows, error)
    QueryRowContext(context.Context, string, ...any) *sql.Row
}
```

## Use cases

* Repository methods that map 1:1 to a table row.
* `INSERT … RETURNING` and `UPDATE … RETURNING` patterns.
* Selecting a flat entity with a small, static filter set.
* Joining multiple tables into a single aggregate struct via embedding.
* Reducing boilerplate for column enumeration and parameter ordering.

## Limitations

* **PostgreSQL only.** The query is parsed/deparsed with `pg_query_go`. Other dialects are not supported.
* **Static templates.** Helpers are limited to `cols`, `set`, `vals`. No dynamic WHERE building, conditional joins, or `IN` clause expansion.
* **Reflection-based mapping.** Column names are derived from struct field names / `json` tags. A mismatch with the actual DB schema causes runtime scan errors. No compile-time verification of column existence beyond `Parse` checks.
* **Scanning order dependency.** `QueryContext` scans rows in struct field declaration order. Changing field order changes the SQL.
* **Parameter names must match struct fields.** `@name` must exist in `K`. Missing parameters cause an error at `Args` time.
* **No support for custom conversions.** `database/sql` scanners are used directly; custom `sql.Scanner` types must be supported by the driver.
* **Template parsing cost.** `New` parses the template and runs `pg_query.Parse` once. It is intended for init-time use, not per-request.
* **`@\w+` regex.** Simple parameter detection; it will not match quoted identifiers or more exotic names.
* **Inline flattening.** Nested structs require either an anonymous field or `json:"...,inline"`. Pointer fields are dereferenced for field discovery but can be confusing.
* **Formatting.** `pg_query.Deparse` normalizes whitespace and may change comments/formatting.

These constraints keep the package small and predictable, but for fully dynamic queries use a query builder or the `sqlc` codegen flow.

## License

MIT
