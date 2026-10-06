package dbt

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/alextanhongpin/dbtx/postgres/dbt/internal"
)

var paramNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func fieldNames(fs []internal.Field) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

// templateFuncs builds the {{ cols }}, {{ set }} and {{ vals }} helpers.
// cols is derived from the row type R; set and vals from the params type P.
//
// Every operator takes the same arguments:
//
//	{{ cols }}                    all columns
//	{{ cols "-" "a" "b" }}        all columns except a and b
//	{{ cols "=" "b" "a" }}        exactly these columns, in this order
func templateFuncs(params, row []internal.Field) template.FuncMap {
	setNames, getNames := fieldNames(params), fieldNames(row)

	writable := func(opts []string) ([]string, error) {
		cols, err := selectColumns(setNames, opts)
		if err != nil {
			return nil, err
		}
		for _, c := range cols {
			if strings.Contains(c, ".") {
				return nil, fmt.Errorf(`dbt: column %q is qualified and cannot be written; exclude it with "-"`, c)
			}
			if !paramNameRe.MatchString(c) {
				return nil, fmt.Errorf("dbt: column %q cannot be used as a parameter name", c)
			}
		}
		return cols, nil
	}

	return template.FuncMap{
		"cols": func(opts ...string) (string, error) {
			cols, err := selectColumns(getNames, opts)
			if err != nil {
				return "", err
			}
			out := make([]string, len(cols))
			for i, c := range cols {
				out[i] = quoteIdent(c)
				if strings.Contains(c, ".") {
					out[i] += " as " + quoteIdent(strings.ReplaceAll(c, ".", "_"))
				}
			}
			return strings.Join(out, ", "), nil
		},
		"set": func(opts ...string) (string, error) {
			cols, err := writable(opts)
			if err != nil {
				return "", err
			}
			out := make([]string, len(cols))
			for i, c := range cols {
				out[i] = quoteIdent(c) + " = @" + c
			}
			return strings.Join(out, ", "), nil
		},
		"vals": func(opts ...string) (string, error) {
			cols, err := writable(opts)
			if err != nil {
				return "", err
			}
			names := make([]string, len(cols))
			ph := make([]string, len(cols))
			for i, c := range cols {
				names[i] = quoteIdent(c)
				ph[i] = "@" + c
			}
			return fmt.Sprintf("(%s) values (%s)", strings.Join(names, ", "), strings.Join(ph, ", ")), nil
		},
	}
}

func selectColumns(all, opts []string) ([]string, error) {
	op, args := "*", []string(nil)
	if len(opts) > 0 {
		op, args = opts[0], opts[1:]
	}

	var out []string
	switch op {
	case "*":
		if len(args) != 0 {
			return nil, fmt.Errorf("dbt: operator * takes no arguments, got %v", args)
		}
		out = all
	case "-":
		if missing := difference(args, all); len(missing) > 0 {
			return nil, fmt.Errorf("%w: %v (available: %v)", ErrUnknownColumn, missing, all)
		}
		out = difference(all, args)
	case "=":
		if missing := difference(args, all); len(missing) > 0 {
			return nil, fmt.Errorf("%w: %v (available: %v)", ErrUnknownColumn, missing, all)
		}
		if dups := duplicates(args); len(dups) > 0 {
			return nil, fmt.Errorf("dbt: duplicate columns %v", dups)
		}
		out = args
	default:
		return nil, fmt.Errorf("dbt: unknown operator %q (want *, - or =)", op)
	}
	if len(out) == 0 {
		return nil, ErrNoColumns
	}
	return out, nil
}

// quoteIdent always quotes identifiers (and each part of a dotted one). This is
// safe for reserved words like "order" and "user"; the normalization pass
// through the Postgres deparser drops quotes that aren't needed.
func quoteIdent(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

// difference returns the elements of a that are not in b.
func difference(a, b []string) []string {
	var out []string
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}

func duplicates(a []string) []string {
	var out []string
	for i, v := range a {
		if slices.Contains(a[:i], v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
