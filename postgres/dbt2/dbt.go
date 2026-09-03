package dbt

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/alextanhongpin/dbtx/postgres/dbt/internal"
)

var paramRe = regexp.MustCompile(`@\w+`)

// DB is the common interface for database execution.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func Must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// SQL holds a compiled query with positional parameters.
type SQL[K, V any] struct {
	query string
	args  []string
}

// New compiles a query template using struct tags for columns.
func New[K, V any](tmpl string) (*SQL[K, V], error) {
	query, args, err := Parse[K, V](tmpl)
	if err != nil {
		return nil, err
	}

	query, err = internal.ParseQuery(query)
	if err != nil {
		return nil, err
	}

	return &SQL[K, V]{query: query, args: args}, nil
}

func Parse[K, V any](tmpl string) (string, []string, error) {
	setterCols := structNames(internal.Make[K]())
	getterCols := structNames(internal.Make[V]())

	funcMap := template.FuncMap{
		"cols": func(opts ...string) (string, error) {
			cols, err := selectColumns(getterCols, opts...)
			if err != nil {
				return "", err
			}
			return strings.Join(aliasColumns(cols), ", "), nil
		},
		"set": func(opts ...string) (string, error) {
			cols, err := selectColumns(setterCols, opts...)
			if err != nil {
				return "", err
			}
			assign := make([]string, len(cols))
			for i, c := range cols {
				assign[i] = fmt.Sprintf("%s = @%s", c, c)
			}
			return strings.Join(assign, ", "), nil
		},
		"vals": func(opts ...string) (string, error) {
			cols, err := selectColumns(setterCols, opts...)
			if err != nil {
				return "", err
			}
			ph := make([]string, len(cols))
			for i, c := range cols {
				ph[i] = "@" + c
			}
			return fmt.Sprintf("(%s) values (%s)", strings.Join(cols, ", "), strings.Join(ph, ", ")), nil
		},
	}

	t := template.Must(template.New("sql").Funcs(funcMap).Parse(tmpl))
	var buf bytes.Buffer
	if err := t.Execute(&buf, nil); err != nil {
		return "", nil, err
	}
	query, args := replaceParams(buf.String())
	if !containsAll(args, setterCols) {
		return "", nil, fmt.Errorf("query references unknown parameters %v", difference(args, setterCols))
	}
	return query, args, nil
}

// QueryContext executes a SELECT and scans all rows into a slice of V.
func (s *SQL[K, V]) QueryContext(ctx context.Context, db DB, params K) ([]V, error) {
	args, err := s.Args(params)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, s.query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	isPtr := internal.IsPointerType[V]()
	var out []V
	for rows.Next() {
		v := internal.Make[V]()
		target := any(v)
		if !isPtr {
			target = &v
		}
		if err := rows.Scan(scanPointers(target)...); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// QueryRowContext executes a SELECT that returns a single row.
func (s *SQL[K, V]) QueryRowContext(ctx context.Context, db DB, params K) (V, error) {
	var zero V
	args, err := s.Args(params)
	if err != nil {
		return zero, err
	}
	v := internal.Make[V]()
	target := any(v)
	if !internal.IsPointerType[V]() {
		target = &v
	}
	if err := db.QueryRowContext(ctx, s.query, args...).Scan(scanPointers(target)...); err != nil {
		return zero, err
	}
	return v, nil
}

// ExecContext executes a mutating statement.
func (s *SQL[K, V]) ExecContext(ctx context.Context, db DB, params K) (sql.Result, error) {
	args, err := s.Args(params)
	if err != nil {
		return nil, err
	}
	return db.ExecContext(ctx, s.query, args...)
}

// Args returns the args from value.
func (s *SQL[K, V]) Args(k K) ([]any, error) {
	values := structValues(k)
	args := make([]any, len(s.args))
	for i, name := range s.args {
		v, ok := values[name]
		if !ok {
			return nil, fmt.Errorf("parameter %q not found in struct", name)
		}
		args[i] = v
	}
	return args, nil
}

func (s *SQL[K, V]) Build(v K) (string, []any, error) {
	args, err := s.Args(v)
	if err != nil {
		return "", nil, err
	}
	return s.query, args, nil
}

func (s *SQL[K, V]) String() string {
	return s.query
}

// ---------- scanning helpers ----------

func scanPointers(v any) []any {
	fields := internal.StructFields(v)
	pts := make([]any, len(fields))
	for i, f := range fields {
		pts[i] = f.Value.Addr().Interface()
	}
	return pts
}

func structNames(v any) []string {
	fs := internal.StructFields(v)
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = f.Name
	}
	return names
}

func structValues(v any) map[string]any {
	fields := internal.StructFields(v)
	values := make(map[string]any, len(fields))
	for _, f := range fields {
		values[f.Name] = f.Value.Interface()
	}
	return values
}

// ---------- column selection ----------

func selectColumns(cols []string, opts ...string) ([]string, error) {
	op := "*"
	args := opts
	if len(opts) > 0 {
		op = opts[0]
		args = opts[1:]
	}
	switch op {
	case "*":
		if len(args) != 0 {
			return nil, fmt.Errorf("operator * does not accept arguments: %v", args)
		}
		return cols, nil
	case "-":
		return difference(cols, args), nil
	case "=":
		if !containsAll(args, cols) {
			return nil, fmt.Errorf("columns %v not found in %v", difference(args, cols), cols)
		}
		return args, nil
	default:
		return nil, fmt.Errorf("unknown operator %q", op)
	}
}

func aliasColumns(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		if strings.Contains(c, ".") {
			out[i] = fmt.Sprintf("%s as %s", c, strings.ReplaceAll(c, ".", "_"))
		} else {
			out[i] = c
		}
	}
	return out
}

// ---------- utilities ----------

func replaceParams(s string) (string, []string) {
	var args []string
	s = paramRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[1:]
		if i := slices.Index(args, name); i != -1 {
			return fmt.Sprintf("$%d", i+1)
		}
		args = append(args, name)
		return fmt.Sprintf("$%d", len(args))
	})
	return s, args
}

func containsAll(a, b []string) bool {
	for _, v := range a {
		if !slices.Contains(b, v) {
			return false
		}
	}
	return true
}

func difference(a, b []string) []string {
	var out []string
	for _, v := range a {
		if slices.Contains(b, v) {
			continue
		}
		out = append(out, v)
	}
	return out
}
