package dbt

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

var re = regexp.MustCompile(`@\w+`)

// DB represents the common db operations for both *sql.DB and *sql.Tx.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type StructFieldInfoList []StructFieldInfo

func (l StructFieldInfoList) Columns() []string {
	res := make([]string, len(l))
	for i, v := range l {
		res[i] = v.Name
	}

	return res
}

// StructFieldInfo holds the final mapped data
type StructFieldInfo struct {
	Name         string
	Value        any
	PointerValue any
}

type SQL[K, V any] struct {
	query     string
	namedArgs []string
}

func (s *SQL[K, V]) QueryContext(ctx context.Context, db DB, params K) ([]V, error) {
	query, args, err := s.Build(params)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	vptr := reflect.TypeFor[V]().Kind() == reflect.Pointer
	var res []V
	defer rows.Close()
	for rows.Next() {
		var v = newNonNil[V]()
		var cols StructFieldInfoList
		if !vptr {
			cols = getFields(&v, "")
		} else {
			cols = getFields(v, "")
		}
		vals := make([]any, len(cols))
		for i, c := range cols {
			vals[i] = c.PointerValue
		}

		err := rows.Scan(vals...)
		if err != nil {
			return nil, err
		}
		res = append(res, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return res, nil
}

func (s *SQL[K, V]) QueryRowContext(ctx context.Context, db DB, params K) (V, error) {
	var zero V

	v := newNonNil[V]()
	var input any = v
	if reflect.TypeFor[V]().Kind() != reflect.Pointer {
		input = &v
	}
	cols := getFields(input, "")
	vals := make([]any, len(cols))
	for i, c := range cols {
		vals[i] = c.PointerValue
	}
	query, args, err := s.Build(params)
	if err != nil {
		return zero, err
	}

	err = db.QueryRowContext(ctx, query, args...).Scan(vals...)
	if err != nil {
		return zero, err
	}
	return v, nil
}

func (s *SQL[K, V]) ExecContext(ctx context.Context, db DB, params K) (sql.Result, error) {
	query, args, err := s.Build(params)
	if err != nil {
		return nil, err
	}

	return db.ExecContext(ctx, query, args...)
}

func (s *SQL[K, V]) Build(k K) (string, []any, error) {
	var input any = k
	if reflect.TypeFor[K]().Kind() != reflect.Pointer {
		input = &k
	}
	args := getFields(input, "")
	m := make(map[string]any)
	for _, arg := range args {
		m[arg.Name] = arg.Value
	}
	res := make([]any, len(s.namedArgs))
	for i, n := range s.namedArgs {
		v, ok := m[n]
		if ok {
			res[i] = v
			continue
		}
		return "", nil, fmt.Errorf("field not found: %s", n)
	}
	return s.query, res, nil
}

func New[K, V any](query string) (*SQL[K, V], error) {
	args := getFields(newNonNilPointer[K](), "")
	cols := getFields(newNonNilPointer[V](), "")

	setter := args.Columns()
	getter := cols.Columns()

	fn := template.FuncMap{
		"cols": func(opts ...string) (string, error) {
			var op string
			if len(opts) == 0 {
				op = "*"
			} else {
				op, opts = opts[0], opts[1:]
			}
			rest, err := set(op, getter, opts)
			if err != nil {
				return "", err
			}
			getter = rest
			return strings.Join(aliasColumn(getter), ","), nil
		},
		"set": func(opts ...string) (string, error) {
			var op string
			if len(opts) == 0 {
				op = "*"
			} else {
				op, opts = opts[0], opts[1:]
			}
			rest, err := set(op, setter, opts)
			if err != nil {
				return "", err
			}
			res := make([]string, len(rest))
			for i, s := range rest {
				res[i] = fmt.Sprintf("%s = @%s", s, s)
			}
			return strings.Join(res, ", "), nil
		},
		"vals": func(opts ...string) (string, error) {
			var op string
			if len(opts) == 0 {
				op = "*"
			} else {
				op, opts = opts[0], opts[1:]
			}
			rest, err := set(op, setter, opts)
			if err != nil {
				return "", err
			}

			ins := make([]string, len(rest))
			pls := make([]string, len(rest))
			for i, s := range rest {
				ins[i] = fmt.Sprintf("%s", s)
				pls[i] = fmt.Sprintf("@%s", s)
			}
			return fmt.Sprintf("(%s) VALUES (%s)", strings.Join(ins, ","), strings.Join(pls, ",")), nil
		},
	}

	t := template.Must(template.New("").Funcs(fn).Parse(query))
	var b bytes.Buffer
	err := t.Execute(&b, nil)
	if err != nil {
		return nil, err
	}
	query = b.String()
	query, namedArgs := replaceNamedArgs(query)
	if !isSubset(namedArgs, setter) {
		return nil, fmt.Errorf("named args not found: %v", difference(namedArgs, setter))
	}
	return &SQL[K, V]{
		query:     query,
		namedArgs: namedArgs,
	}, nil
}

func isAnyType(t reflect.Type) bool {
	return t.Kind() == reflect.Interface && t.NumMethod() == 0
}

func getFields(a any, prefix string) StructFieldInfoList {
	var fields []StructFieldInfo

	v := reflect.ValueOf(a)
	// Skip invald type like "any".
	if !v.IsValid() {
		return nil
	}

	t := v.Type()
	// Ensure we are working with the underlying struct type.
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
		v = v.Elem()
	}

	// Handle "any" type.
	if isAnyType(t) {
		return nil
	}

	for i := range t.NumField() {
		tf := t.Field(i)
		vf := v.Field(i)
		tag := cmp.Or(tf.Tag.Get("db"), tf.Tag.Get("json"))
		// 1. Verify the field can be modified (must be an exported field)
		// 2. Check if the field is a pointer and is currently nil
		if vf.CanSet() && vf.Kind() == reflect.Pointer && vf.IsNil() {
			// Get the underlying type that the pointer points to
			underlyingType := vf.Type().Elem()

			// reflect.New creates a new zero value of the underlying type
			// and returns a reflect.Value pointer to it
			newValue := reflect.New(underlyingType)

			// Set the nil pointer field to our newly allocated pointer
			vf.Set(newValue)
		}

		// Skip fields without db tags
		if tag == "" || tag == "-" {
			continue
		}

		// Parse tag options (e.g., "meta,inline" -> ["meta", "inline"])
		before, inline := strings.CutSuffix(tag, ",inline")
		name := cmp.Or(before, strings.ToLower(tf.Name))
		if inline {
			v := vf
			if vf.Kind() == reflect.Pointer {
				v = v.Elem()
			}
			// Determine the new prefix for nested fields
			// Recursively parse the embedded struct fields
			newFields := getFields(v.Addr().Interface(), join([]string{prefix, name}, "."))
			fields = append(fields, newFields...)
		} else {
			// Construct the final DB column name with the accumulated prefix
			fields = append(fields, StructFieldInfo{
				Name:         join([]string{prefix, name}, "."),
				Value:        vf.Interface(),
				PointerValue: vf.Addr().Interface(),
			})
		}
	}

	return fields
}

func join(s []string, sep string) string {
	return strings.Join(filter(s), sep)
}

func filter[T comparable](vs []T) []T {
	var zero T
	var res []T
	for _, v := range vs {
		if v == zero {
			continue
		}
		res = append(res, v)
	}
	return res
}

func aliasColumn(cols []string) []string {
	res := make([]string, len(cols))
	for i, c := range cols {
		res[i] = fmt.Sprintf("%s as %s", c, strings.ReplaceAll(c, ".", "_"))
	}
	return res
}

func set(op string, from, take []string) ([]string, error) {
	if !isSubset(take, from) {
		return nil, fmt.Errorf("columns not found: %v", difference(take, from))

	}
	switch op {
	case "*":
		if len(take) != 0 {
			return nil, fmt.Errorf("* cannot accept additional columns: %v", take)
		}
		// Take all.
		return from, nil
	case "-":
		// Exclude columns.
		return difference(from, take), nil
	case "=":
		// Take exactly.
		return take, nil
	default:
		return nil, fmt.Errorf("invalid operator: %s", op)
	}
}

// isSubset checks if a is a subset of b.
func isSubset(a, b []string) bool {
	for _, v := range a {
		if !slices.Contains(b, v) {
			return false
		}
	}

	return true
}

// difference returns difference of a - b
func difference(a, b []string) []string {
	var res []string
	for _, v := range a {
		if slices.Contains(b, v) {
			continue
		}
		res = append(res, v)
	}

	return res
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

func isPtr[T any]() bool {
	return reflect.TypeFor[T]().Kind() == reflect.Pointer
}

func newNonNilPointer[T any]() any {
	v := newNonNil[T]()
	if isPtr[T]() {
		return v
	}
	return &v
}

// Instantiate non-nil instances of reference types or zero-values of basic types.
func newNonNil[T any]() T {
	var zero T
	if !reflect.ValueOf(zero).IsValid() {
		// If this is "any", it will return nil.
		return zero
	}

	// 1. Get the reflect.Type of the generic parameter T
	t := reflect.TypeFor[T]()

	// 2. Initialize based on the specific Kind of type
	val := createNonNilValue(t)

	// 3. Cast the reflect.Value safely back to the generic type T
	return val.Interface().(T)
}

func createNonNilValue(t reflect.Type) reflect.Value {
	switch t.Kind() {
	case reflect.Pointer:
		// Instantiates memory for the base element and returns a pointer to it
		return reflect.New(t.Elem())

	case reflect.Slice:
		// Allocates an empty slice with length 0 and capacity 0 (non-nil)
		return reflect.MakeSlice(t, 0, 0)

	case reflect.Map:
		// Initializes an empty map ready for writes
		return reflect.MakeMap(t)

	case reflect.Chan:
		// Initializes an unbuffered channel
		return reflect.MakeChan(t, 0)

	default:
		// For structs and primitives (int, string), reflect.New creates a pointer,
		// so we call .Elem() to extract the non-nil zero value.
		return reflect.New(t).Elem()
	}
}
