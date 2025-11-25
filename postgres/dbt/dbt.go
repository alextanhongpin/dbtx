package dbt

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

// DB represents the common db operations for both *sql.DB and *sql.Tx.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Scanner interface {
	Scan() map[string]any
}

type Valuer interface {
	Value() map[string]any
}

type Map interface {
	Map() map[string]any
}

var re = regexp.MustCompile(`@\w+`)

type NoSelect struct{}

func (n *NoSelect) Scan() map[string]any {
	return nil
}

type NoArgs struct{}

func (n *NoArgs) Value() map[string]any {
	return nil
}

type Statement[
	T any,
	V any,
	TP interface {
		*T
		Scanner
	},
	VP interface {
		*V
		Valuer
	},
] struct {
	stmt string
	args []string
}

func New[
	T any,
	V any,
	TP interface {
		*T
		Scanner
	},
	VP interface {
		*V
		Valuer
	},
](stmt string) *Statement[T, V, TP, VP] {
	var tp TP = new(T)
	var vp VP = new(V)
	var tpm = M(tp.Scan())
	var vpm = M(vp.Value())

	stmt, err := executeTemplate(stmt, nil, template.FuncMap{
		"set": func(op string, options ...string) string {
			return set(vpm, op, options...)
		},
		"insert": func() string {
			return insert(vpm)
		},
		"columns": func(opts ...string) string {
			return columns(tpm, opts...)
		},
	})
	if err != nil {
		panic(err)
	}

	stmt, args := replaceNamedArgs(stmt)
	cols := sortedKeys(vpm)
	if !isEqual(cols, args) {
		panic(fmt.Errorf("dbt.New[%T, %T](%s) returns unexpected difference in args value (-want +got):\n%s", tp, vp, stmt, symmetricDifference(args, cols)))
	}

	return &Statement[T, V, TP, VP]{
		stmt: stmt,
		args: args,
	}
}

func (s *Statement[T, V, TP, VP]) Args(in VP) []any {
	m := in.Value()
	res := make([]any, len(s.args))
	for i, k := range s.args {
		res[i] = m[k]
	}

	return res
}

func (s *Statement[T, V, TP, VP]) ExecContext(ctx context.Context, db DB, in VP) (sql.Result, error) {
	res, err := db.ExecContext(ctx, s.stmt, s.Args(in)...)
	return res, err
}

func (s *Statement[T, V, TP, VP]) QueryRowContext(ctx context.Context, db DB, in VP) (TP, error) {
	var v TP = new(T)
	err := db.QueryRowContext(ctx, s.stmt, s.Args(in)...).Scan(sortedValues(M(v.Scan()))...)
	if err != nil {
		return nil, err
	}

	return v, nil
}

func (s *Statement[T, V, TP, VP]) QueryContext(ctx context.Context, db DB, in VP) ([]TP, error) {
	rows, err := db.QueryContext(ctx, s.stmt, s.Args(in)...)
	if err != nil {
		return nil, err
	}
	defer func() {
		err := rows.Close()
		if err != nil {
			panic(err)
		}
	}()

	var result []TP
	for rows.Next() {
		var v TP = new(T)
		err := rows.Scan(sortedValues(M(v.Scan()))...)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}

	if err = rows.Err(); err != nil {
		return result, err
	}

	return result, nil
}

func (s *Statement[T, V, TP, VP]) String() string {
	return s.stmt
}

func set(v Map, op string, options ...string) string {
	cols := sortedKeys(v)
	switch op {
	case "*":
	case "in": // Include.
		if !isSubsetOf(cols, options) {
			panic(fmt.Errorf("columns %v not present in %v", difference(options, cols), cols))
		}
		cols = options
	case "ex": // Exclude.
		if !isSubsetOf(cols, options) {
			panic(fmt.Errorf("columns %v not present in %v", difference(options, cols), cols))
		}
		cols = difference(cols, options)
	default:
		panic(fmt.Errorf(`invalid set option %q: must be one of "*", "in" or "ex"`, op))
	}

	var res []string
	for _, c := range cols {
		res = append(res, fmt.Sprintf("%s = @%s", c, c))
	}
	return join(res)
}

func insert(v Map) string {
	cols := sortedKeys(v)
	if len(cols) == 0 {
		return ""
	}

	ps := make([]string, len(cols))

	for i, c := range cols {
		ps[i] = fmt.Sprintf("@%s", c)
	}

	return fmt.Sprintf("(%s) VALUES (%s)", join(cols), join(ps))
}

func columns[T Map](v T, opts ...string) string {
	switch len(opts) {
	case 0:
		return join(sortedKeys(v))
	case 1:
		return join(sortedKeys(M(v.Map()).WithPrefix(opts[0])))
	case 2:
		return join(sortedKeys(M(v.Map()).WithPrefix(opts[0]).WithAlias(opts[1])))
	default:
		panic("unknown option")
	}
}

func sortedKeys(m Map) []string {
	return slices.Sorted(maps.Keys(m.Map()))
}

func sortedValues(v Map) []any {
	if v == nil {
		return nil
	}

	m := v.Map()
	cols := sortedKeys(v)
	args := make([]any, 0, len(cols))
	for _, c := range cols {
		args = append(args, m[c])
	}

	return args
}

func replaceNamedArgs(s string) (string, []string) {
	var args []string
	s = re.ReplaceAllStringFunc(s, func(match string) string {
		match = match[1:] // remove ampersand
		i := slices.Index(args, match)
		if i != -1 {
			return fmt.Sprintf("$%d", i+1)
		}
		args = append(args, match)
		return fmt.Sprintf("$%d", len(args))
	})

	return s, args
}

func join(cols []string) string {
	return strings.Join(cols, ", ")
}

func executeTemplate(in string, data any, fn template.FuncMap) (string, error) {
	t := template.Must(template.New("").Funcs(fn).Parse(in))
	var b bytes.Buffer
	err := t.Execute(&b, data)
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

type M map[string]any

func (m M) Map() map[string]any {
	return m
}

func (m M) As(prefix, alias string) M {
	return m.WithPrefix(prefix).WithAlias(alias)
}

func (m M) WithPrefix(prefix string) M {
	c := make(M)
	for k, v := range m {
		c[fmt.Sprintf("%s.%s", prefix, k)] = v
	}

	return c
}

func (m M) WithAlias(alias string) M {
	c := make(M)
	for k, v := range m {
		p := strings.Index(k, ".")
		c[fmt.Sprintf("%s AS %s_%s", k, alias, k[p+1:])] = v
	}

	return c
}

func (m M) Merge(o M) {
	maps.Copy(m, o)
}

func Merge(os ...M) M {
	res := make(M)
	for _, o := range os {
		res.Merge(o)
	}

	return res
}

type ID[T any] struct {
	Val T
}

func (i *ID[T]) Scan() map[string]any {
	return map[string]any{
		"id": &i.Val,
	}
}

func (i *ID[T]) Value() map[string]any {
	return map[string]any{
		"id": i.Val,
	}
}

func isSubsetOf[T comparable](a, b []T) bool {
	for _, v := range b {
		if !slices.Contains(a, v) {
			return false
		}
	}

	return true
}

// difference returns a - b
func difference[T comparable, S []T](a, b S) S {
	m := make(map[T]struct{})
	for _, v := range b {
		m[v] = struct{}{}
	}

	var res S
	for _, v := range a {
		if _, ok := m[v]; !ok {
			res = append(res, v)
		}
	}

	return res
}

// Returns a new set containing elements that are unique to each set (not common to both).
func symmetricDifference[T comparable](a, b []T) []T {
	return append(difference(a, b), difference(b, a)...)
}

func isEqual[T comparable](a, b []T) bool {
	return len(a) == len(b) && len(difference(a, b)) == 0 && len(difference(b, a)) == 0
}
