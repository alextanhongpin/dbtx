# dbt

A tiny Go helper for type-safe PostgreSQL queries using struct tags and `text/template`.

`dbt` compiles a query template into a PostgreSQL statement with positional parameters (`$1`, `$2`, …), derives column lists from Go structs, and scans rows back into structs **by column name**. It is meant for the boring CRUD parts where the shape of a row is known at compile time: `INSERT … RETURNING`, `UPDATE … SET … RETURNING`, and `SELECT`, including joins into an aggregate struct via embedding.

## Installation

```bash
go get github.com/alextanhongpin/dbtx/postgres/dbt
```

Requires Go 1.22+. The package uses `pg_query_go` (cgo) to validate and normalize SQL when a query is compiled, so it is PostgreSQL-specific.

## Quick start

```go
type User struct {
    ID        int
    Name      string
    Email     string
    CreatedAt time.Time
}

type CreateUserParams struct {
    Name  string
    Email string
}

var createUser = dbt.Must(dbt.New[CreateUserParams, User](`
    insert into users {{ vals }} returning {{ cols }}
`))

fmt.Println(createUser)
// INSERT INTO users (name, email) VALUES ($1, $2) RETURNING id, name, email, created_at
// (keyword case and whitespace are normalized by the Postgres deparser)

user, err := createUser.QueryRowContext(ctx, db, CreateUserParams{Name: "john", Email: "john@a.co"})
```

Compile queries once, at package level. `New` reports template errors, unknown `@params` and invalid SQL immediately, so a broken query fails at program start rather than on first use.

Any `*sql.DB`, `*sql.Tx` or `*sql.Conn` works:

```go
rows, err := q.QueryContext(ctx, db, params)    // []R, empty (never nil) when no rows
one, err  := q.QueryRowContext(ctx, db, params) // R, or sql.ErrNoRows
res, err  := q.ExecContext(ctx, db, params)     // sql.Result
```

The generic parameters are `P` for the parameter struct and `R` for the row type. Statements that return no rows can use `NewExec[P]`.

## Struct mapping

Column names are resolved per field, in this order:

1. the `db` tag, then the `json` tag (`-` skips the field; options such as `omitempty` are ignored)
2. otherwise `stringcase.ToSnake(FieldName)`

Unexported fields are skipped.

**Leaf types.** `time.Time`, anything implementing `sql.Scanner` or `driver.Valuer` (`sql.NullString`, `pq.StringArray`, UUID types, …), and every non-struct type is a single column.

**Nested structs.** Embedded structs, and fields tagged `,inline`, are expanded. Their columns are prefixed with the field's name (the tag name if given), treated as a table alias:

| Go | selected as | scanned from column |
|---|---|---|
| `User` embedded with `db:"u"`, field `Name` | `u.name AS u_name` | `u_name` |
| `Author` field tagged `db:"author,inline"` | `author.id AS author_id` | `author_id` |

Tag a struct `,flatten` to expand it **without** a prefix, which suits shared base models:

```go
type Base struct{ CreatedAt time.Time }

type Post struct {
    Base  `db:",flatten"` // created_at
    Title string          // title
}
```

Pointer embeds (`*User`) are allocated while scanning and read as `NULL` when nil. A struct field that is neither embedded nor tagged `,inline` is a single column, for example a `jsonb` value type with a `Scanner`.

Two fields that resolve to the same column name (`a.b` and `a_b` collide too) are an error when the query is compiled.

## Template helpers

Helpers expand when the query is compiled. `cols` uses the row type `R`; `set` and `vals` use the parameter type `P`. All three take the same optional arguments:

```
{{ cols }}                      all columns
{{ cols "-" "id" "secret" }}    all columns except these
{{ cols "=" "name" "id" }}      exactly these, in this order
```

* `{{ cols }}`: the select list.
* `{{ set }}`: `col = @col, …`
* `{{ vals }}`: `(col, …) values (@col, …)`

Unknown column names in `"-"` or `"="`, duplicate names in `"="`, and empty selections are compile errors (`ErrUnknownColumn`, `ErrNoColumns`), so a typo can never silently select everything. Identifiers are always quoted, so columns named `order`, `user` or `group` work; the deparser removes quotes that aren't needed.

`set` and `vals` write real columns, so qualified columns (`u.name`) are rejected. Exclude them with `"-"`, or use separate parameter and row types, which is the usual shape anyway.

Prefer `{{ set "-" "id" }}` over plain `{{ set }}` for updates, otherwise the primary key is assigned too.

## Parameters

Write parameters as `@name`. Each is rewritten to a positional `$n`; repeated names share one `$n`, and arguments are ordered by first appearance. A parameter name is the field's column name, with dots replaced by underscores (`@author_id`).

Parameter detection is lexical, not a regexp. Placeholders are **not** recognized inside:

* string literals (`'admin@example.com'`, `E'…'`) and quoted identifiers
* `--` and `/* … */` comments (which nest, as in Postgres)
* `$$ … $$` and `$tag$ … $tag$` bodies

and the operators `@>`, `@?` and `@@` are left alone. `$1`-style placeholders already in the query are kept.

If a query contains `{{` as data (for example `'{{1,2},{3,4}}'::int[]`), switch the template delimiters:

```go
dbt.New[P, R](tmpl, dbt.WithDelims("<%", "%>"))
```

## Scanning

Rows are matched to struct fields by **column name**, so a query can select any subset of the struct's columns, in any order, and extra struct fields simply keep their zero value:

```go
type User struct{ ID int; Name string; Email string }

dbt.New[any, User](`select {{ cols "-" "email" }} from users`) // email stays ""
dbt.New[any, User](`select {{ cols "=" "name" "id" }} from users`)
dbt.New[any, User](`select id, name from users`)               // hand-written also works
```

A result column with no matching field fails with `ErrUnknownColumn`, which is better than silently dropping data.

`R` may also be a scalar for single-column queries:

```go
countUsers := dbt.Must(dbt.New[any, int64](`select count(*) from users`))
n, err := countUsers.QueryRowContext(ctx, db, nil)
```

`NULL` scans into pointer fields as `nil` (`*string`, `*time.Time`, …) or into `sql.Null*` types.

## Examples

### Update

```go
type UpdateUserParams struct{ ID int; Name string }

var updateUser = dbt.Must(dbt.New[UpdateUserParams, User](
    `update users set {{ set "-" "id" }} where id = @id returning {{ cols }}`))
// UPDATE users SET name = $1 WHERE id = $2 RETURNING id, name, email, created_at
```

### Select with filters

```go
type Filter struct{ Name, Email string }

var findUsers = dbt.Must(dbt.New[Filter, User](
    `select {{ cols }} from users where name = @name and email = @email`))
```

### Aggregates via embedding

The tag on each embedded struct is the table alias used in the query:

```go
type UserBookAggregate struct {
    UserBook `db:"ub"`
    User     `db:"u"`
    Book     `db:"b"`
}

var listUserBooks = dbt.Must(dbt.New[any, UserBookAggregate](`
    select {{ cols }}
    from user_books ub
    join users u on ub.user_id = u.id
    join books b on ub.book_id = b.id`))
// SELECT ub.id AS ub_id, ub.user_id AS ub_user_id, ub.book_id AS ub_book_id,
//        u.id AS u_id, u.name AS u_name, b.id AS b_id, b.title AS b_title
// FROM user_books ub JOIN users u ON …
```

Pointer embeds (`*User`) are allocated whenever the row is scanned. They do not make the `NULL`s of an outer join scannable: for `left join`, give the optional struct's fields pointer or `sql.Null*` types.

## API

```go
func New[P, R any](tmpl string, opts ...Option) (*SQL[P, R], error)
func NewExec[P any](tmpl string, opts ...Option) (*SQL[P, struct{}], error)
func Must[T any](v T, err error) T
func WithDelims(left, right string) Option

func (s *SQL[P, R]) QueryContext(ctx, db DB, p P) ([]R, error)
func (s *SQL[P, R]) QueryRowContext(ctx, db DB, p P) (R, error)
func (s *SQL[P, R]) ExecContext(ctx, db DB, p P) (sql.Result, error)
func (s *SQL[P, R]) Args(p P) ([]any, error)
func (s *SQL[P, R]) Build(p P) (query string, args []any, err error)
func (s *SQL[P, R]) String() string

var ErrUnknownParam, ErrUnknownColumn, ErrNoColumns, ErrNilParams error
```

`DB` is the subset of `database/sql` the package needs:

```go
type DB interface {
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
```

A compiled `*SQL` is immutable and safe for concurrent use. `Build` returns the final SQL and arguments, useful for logging or for driving another client such as `pgx` directly. Driver errors are returned unwrapped, so `errors.Is(err, sql.ErrNoRows)` and `errors.As(err, &pgErr)` work as usual.

## Limitations

* **PostgreSQL only.** Queries are parsed and deparsed with `pg_query_go`, which needs cgo.
* **Static templates.** The helpers are `cols`, `set` and `vals`. There is no dynamic `WHERE` building, conditional joins or `IN` expansion; use `= any(@ids)` with an array parameter for the latter.
* **Reflection-based mapping.** Parameters are checked against the struct when the query is compiled, but result columns can only be checked when the query runs, where a mismatch gives `ErrUnknownColumn`. Struct metadata is computed once per type and cached.
* **Qualified columns are read-only.** `set` and `vals` reject `table.column` names.
* **Unexported embedded pointers** (`*user`) are skipped, because reflection cannot allocate them. Export the type.
* **Custom conversions.** Values go through `database/sql` unchanged, so custom types need `sql.Scanner` / `driver.Valuer`.
* **Formatting.** `pg_query.Deparse` normalizes the SQL and drops comments, so `String()` won't match your template text.
* **Init-time cost.** `New` parses the template and the SQL once. Call it at startup, not per request.

For fully dynamic queries, use a query builder, or the `sqlc` codegen flow.

## Upgrading from the previous version

* `Parse` is no longer exported; use `New`.
* Type parameters are renamed `K, V` to `P, R`. No code change needed unless you spell them out.
* Scanning is by column name rather than struct field order. Reordering fields no longer changes behavior, and `cols "-"` / `cols "="` now work with scanning.
* `QueryContext` returns an empty slice instead of `nil` when there are no rows.
* The `DB` interface no longer requires `QueryRowContext`.
* Tags are read from `db` first, then `json`. Embedded structs keep their prefix behavior; a tag on an embed (for example `json:"u"`) is still its prefix.
* `{{ set }}` includes every field of `P`. Use `{{ set "-" "id" }}` to leave the key alone.
* Typos in `cols "-"` used to be ignored and are now errors.
* `@` inside string literals and comments is no longer treated as a parameter.

## Testing

`dbt_unit_test.go` covers the compiler and scanner with a fake driver, so it needs no database. `dbt_test.go` runs against a real PostgreSQL container via `dbtest`, including joins, `NULL`/array round-trips, reserved-word columns, column subsets and `sql.ErrNoRows`.

## License

MIT
