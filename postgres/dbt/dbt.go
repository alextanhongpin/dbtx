// Package dbt compiles SQL templates with named parameters into positional
// Postgres queries, and maps rows to and from structs.
//
//	type GetUser struct{ ID int64 `db:"id"` }
//	type User struct {
//		ID   int64  `db:"id"`
//		Name string `db:"name"`
//	}
//
//	var getUser = dbt.Must(dbt.New[GetUser, User](
//		`select {{ cols }} from users where id = @id`))
//
//	u, err := getUser.QueryRowContext(ctx, db, GetUser{ID: 1})
//
// # Struct mapping
//
// Column names come from the `db` tag, then the `json` tag, then the
// snake_cased field name. `-` skips a field. Embedded structs and fields tagged
// `,inline` are flattened with their name as a table-alias prefix
// ("author.name", selected as "author"."name" as "author_name", and available
// as @author_name). Tag a struct `,flatten` to inline it without any prefix.
// Structs implementing sql.Scanner or driver.Valuer, and time.Time, are
// single columns.
//
// # Templates
//
// {{ cols }}, {{ set }} and {{ vals }} expand using the row type R (cols) and
// the params type P (set, vals). Each accepts "-" (exclude) or "=" (exactly,
// in order) followed by column names: {{ cols "-" "password_hash" }}.
//
// Rows are matched to fields by column name, so queries may select any subset
// of columns in any order. R may also be a scalar (int64, string, ...) for
// single-column queries. A compiled *SQL is immutable and safe for concurrent
// use.
package dbt

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"text/template"

	"github.com/alextanhongpin/dbtx/postgres/dbt/internal"
)

var (
	ErrUnknownParam  = errors.New("dbt: unknown parameter")
	ErrUnknownColumn = errors.New("dbt: unknown column")
	ErrNoColumns     = errors.New("dbt: no columns selected")
	ErrNilParams     = errors.New("dbt: nil params")
)

// DB is satisfied by *sql.DB, *sql.Tx and *sql.Conn.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Must panics if err is not nil. Use it to compile queries at package init.
func Must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Option configures New.
type Option func(*config)

type config struct{ left, right string }

// WithDelims changes the template delimiters (default "{{" and "}}"). Use it
// when a query contains literals such as '{{1,2},{3,4}}'::int[].
func WithDelims(left, right string) Option {
	return func(c *config) { c.left, c.right = left, right }
}

// SQL is a compiled query. P is the params struct, R the row type.
type SQL[P, R any] struct {
	query  string
	params []internal.Field // positional: params[i] feeds $i+1
	row    rowPlan
}

type rowPlan struct {
	scalar   bool
	ptr      bool         // R is *Struct
	base     reflect.Type // Struct (R without the pointer)
	byColumn map[string]internal.Field
}

// New compiles tmpl. Template errors, unknown @params and invalid SQL are all
// reported here rather than at execution time.
func New[P, R any](tmpl string, opts ...Option) (*SQL[P, R], error) {
	cfg := config{left: "{{", right: "}}"}
	for _, opt := range opts {
		opt(&cfg)
	}

	pt, rt := reflect.TypeFor[P](), reflect.TypeFor[R]()
	paramFields, err := internal.FieldsOf(pt)
	if err != nil {
		return nil, err
	}
	rowFields, err := internal.FieldsOf(rt)
	if err != nil {
		return nil, err
	}

	t, err := template.New("sql").
		Delims(cfg.left, cfg.right).
		Funcs(templateFuncs(paramFields, rowFields)).
		Parse(tmpl)
	if err != nil {
		return nil, fmt.Errorf("dbt: parsing template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, nil); err != nil {
		return nil, fmt.Errorf("dbt: executing template: %w", err)
	}

	query, names := rewriteParams(buf.String())

	byAlias := make(map[string]internal.Field, len(paramFields))
	for _, f := range paramFields {
		byAlias[f.Alias] = f
	}
	params := make([]internal.Field, len(names))
	for i, name := range names {
		f, ok := byAlias[name]
		if !ok {
			return nil, fmt.Errorf("%w: @%s (available: %v)", ErrUnknownParam, name, aliases(paramFields))
		}
		params[i] = f
	}

	query, err = internal.ParseQuery(query)
	if err != nil {
		return nil, err
	}

	plan := rowPlan{base: rt}
	if rt.Kind() == reflect.Pointer {
		plan.ptr, plan.base = true, rt.Elem()
	}
	if plan.base.Kind() == reflect.Pointer {
		return nil, fmt.Errorf("dbt: row type %s: pointer to pointer is not supported", rt)
	}
	plan.scalar = internal.IsLeaf(plan.base)
	plan.byColumn = make(map[string]internal.Field, len(rowFields))
	for _, f := range rowFields {
		plan.byColumn[f.Alias] = f
	}

	return &SQL[P, R]{query: query, params: params, row: plan}, nil
}

// NewExec compiles a statement that returns no rows.
func NewExec[P any](tmpl string, opts ...Option) (*SQL[P, struct{}], error) {
	return New[P, struct{}](tmpl, opts...)
}

// QueryContext runs the query and scans every row. It returns an empty,
// non-nil slice when there are no rows.
func (s *SQL[P, R]) QueryContext(ctx context.Context, db DB, p P) ([]R, error) {
	args, err := s.Args(p)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, s.query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scan, err := s.scanner(rows)
	if err != nil {
		return nil, err
	}
	out := make([]R, 0)
	for rows.Next() {
		v, err := scan()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// QueryRowContext scans the first row, returning sql.ErrNoRows if there is
// none. Any further rows are ignored.
func (s *SQL[P, R]) QueryRowContext(ctx context.Context, db DB, p P) (R, error) {
	var zero R
	args, err := s.Args(p)
	if err != nil {
		return zero, err
	}
	rows, err := db.QueryContext(ctx, s.query, args...)
	if err != nil {
		return zero, err
	}
	defer rows.Close()

	scan, err := s.scanner(rows)
	if err != nil {
		return zero, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return zero, err
		}
		return zero, sql.ErrNoRows
	}
	return scan()
}

// ExecContext runs a statement that doesn't return rows.
func (s *SQL[P, R]) ExecContext(ctx context.Context, db DB, p P) (sql.Result, error) {
	args, err := s.Args(p)
	if err != nil {
		return nil, err
	}
	return db.ExecContext(ctx, s.query, args...)
}

// Args returns the positional arguments for p, in $1..$n order.
func (s *SQL[P, R]) Args(p P) ([]any, error) {
	if len(s.params) == 0 {
		return nil, nil
	}
	v := reflect.ValueOf(p)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil, ErrNilParams
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("dbt: params must be a struct, got %T", p)
	}
	args := make([]any, len(s.params))
	for i, f := range s.params {
		args[i] = f.Get(v)
	}
	return args, nil
}

// Build returns the final SQL and arguments, for use with other drivers or
// for logging.
func (s *SQL[P, R]) Build(p P) (string, []any, error) {
	args, err := s.Args(p)
	if err != nil {
		return "", nil, err
	}
	return s.query, args, nil
}

// String returns the compiled query with $n placeholders.
func (s *SQL[P, R]) String() string { return s.query }

// scanner maps the result columns to fields once, then returns a function
// that scans the current row.
func (s *SQL[P, R]) scanner(rows *sql.Rows) (func() (R, error), error) {
	if s.row.scalar {
		return func() (R, error) {
			var r R
			err := rows.Scan(&r)
			return r, err
		}, nil
	}

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	fields := make([]internal.Field, len(cols))
	for i, c := range cols {
		f, ok := s.row.byColumn[c]
		if !ok {
			return nil, fmt.Errorf("%w: %q has no matching field in %s", ErrUnknownColumn, c, s.row.base)
		}
		fields[i] = f
	}

	dest := make([]any, len(fields))
	return func() (R, error) {
		pv := reflect.New(s.row.base)
		sv := pv.Elem()
		for i, f := range fields {
			dest[i] = f.Ptr(sv)
		}
		var zero R
		if err := rows.Scan(dest...); err != nil {
			return zero, err
		}
		if s.row.ptr {
			return pv.Interface().(R), nil
		}
		return sv.Interface().(R), nil
	}, nil
}

func aliases(fs []internal.Field) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Alias
	}
	return out
}
