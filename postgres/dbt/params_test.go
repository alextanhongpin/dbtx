package dbt

import (
	"reflect"
	"testing"
)

func TestRewriteParams(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantSQL  string
		wantArgs []string
	}{
		{"basic", `select * from t where a = @a and b = @b`, `select * from t where a = $1 and b = $2`, []string{"a", "b"}},
		{"repeat reuses index", `@a @b @a`, `$1 $2 $1`, []string{"a", "b"}},
		{"cast", `@id::int`, `$1::int`, []string{"id"}},
		{"string literal", `where e = 'admin@example.com' and x = @x`, `where e = 'admin@example.com' and x = $1`, []string{"x"}},
		{"escaped quote", `'it''s @no' @yes`, `'it''s @no' $1`, []string{"yes"}},
		{"E string backslash", `E'\' @no' @yes`, `E'\' @no' $1`, []string{"yes"}},
		{"quoted ident", `select "a@b" from t where c = @c`, `select "a@b" from t where c = $1`, []string{"c"}},
		{"line comment", "-- notify @team\nselect @a", "-- notify @team\nselect $1", []string{"a"}},
		{"block comment nested", `/* a /* @b */ @c */ @d`, `/* a /* @b */ @c */ $1`, []string{"d"}},
		{"dollar quote", `$$ @a $$ @b`, `$$ @a $$ $1`, []string{"b"}},
		{"tagged dollar quote", `$fn$ $$ @a $$ $fn$ @b`, `$fn$ $$ @a $$ $fn$ $1`, []string{"b"}},
		{"existing positional", `$1 @a`, `$1 $1`, []string{"a"}},
		{"operators", `a @> b and c @? d and e @@ f`, `a @> b and c @? d and e @@ f`, nil},
		{"no ident before", `a@b`, `a@b`, nil},
		{"unterminated string", `'abc @a`, `'abc @a`, nil},
		{"unicode name", `@名前`, `$1`, []string{"名前"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSQL, gotArgs := rewriteParams(tt.in)
			if gotSQL != tt.wantSQL {
				t.Errorf("sql\n got: %s\nwant: %s", gotSQL, tt.wantSQL)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Errorf("args: got %v, want %v", gotArgs, tt.wantArgs)
			}
		})
	}
}

func FuzzRewriteParams(f *testing.F) {
	for _, s := range []string{"@a", "'@a'", "$$@a$$", "/*@a", "-- @a", `"@a`, "E'\\", "$x$"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, args := rewriteParams(s) // must not panic or loop
		if len(args) == 0 && out != s {
			t.Fatalf("no params but output changed: %q -> %q", s, out)
		}
	})
}

func TestSelectColumns(t *testing.T) {
	all := []string{"id", "name", "email"}
	tests := []struct {
		opts    []string
		want    []string
		wantErr bool
	}{
		{nil, all, false},
		{[]string{"*"}, all, false},
		{[]string{"*", "id"}, nil, true},
		{[]string{"-", "email"}, []string{"id", "name"}, false},
		{[]string{"-", "emial"}, nil, true}, // typo must not be silently ignored
		{[]string{"-", "id", "name", "email"}, nil, true},
		{[]string{"=", "email", "id"}, []string{"email", "id"}, false},
		{[]string{"=", "id", "id"}, nil, true},
		{[]string{"=", "nope"}, nil, true},
		{[]string{"+", "id"}, nil, true},
	}
	for _, tt := range tests {
		got, err := selectColumns(all, tt.opts)
		if (err != nil) != tt.wantErr {
			t.Errorf("%v: err=%v, wantErr=%v", tt.opts, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%v: got %v, want %v", tt.opts, got, tt.want)
		}
	}
}
