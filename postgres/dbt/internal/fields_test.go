package internal

import (
	"database/sql"
	"reflect"
	"testing"
	"time"
)

type Author struct {
	ID   int64
	Name string
}

type Book struct {
	ID          int64
	Title       string
	PublishedAt time.Time
}

type hidden struct{ X string }

type Base struct {
	CreatedAt time.Time
}

func names(t *testing.T, v any) []string {
	t.Helper()
	fs, err := FieldsOf(reflect.TypeOf(v))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

func TestFieldsOf(t *testing.T) {
	type tagged struct {
		A string `db:"a_col"`
		B string `json:"b_col,omitempty"`
		C string `json:"-"`
		D string `db:"-"`
		E string
		f string // unexported
	}
	_ = tagged{}.f

	type embedded struct {
		Author
		Book
	}
	type withHidden struct {
		hidden
		Y string
	}
	type taggedEmbed struct {
		Author `json:"u"`
		*Book  `db:"b"`
	}
	type embeddedPtr struct {
		*Author
		Extra sql.NullString
	}
	type named struct {
		Writer Author `json:"writer,inline"`
		Plain  Author // not inline: a single (struct-valued) column
	}
	type flat struct {
		Base    `json:",flatten"`
		Writer  Author `db:"writer,inline"`
		Title   string
		Created time.Time
	}

	tests := []struct {
		name string
		in   any
		want []string
	}{
		{"tags", tagged{}, []string{"a_col", "b_col", "e"}},
		{"embedded structs are prefixed", embedded{}, []string{"author.id", "author.name", "book.id", "book.title", "book.published_at"}},
		{"unexported value embed", withHidden{}, []string{"hidden.x", "y"}},
		{"embedded with tag name is the prefix", taggedEmbed{}, []string{"u.id", "u.name", "b.id", "b.title", "b.published_at"}},
		{"embedded pointer", &embeddedPtr{}, []string{"author.id", "author.name", "extra"}},
		{"inline and plain", named{}, []string{"writer.id", "writer.name", "plain"}},
		{"flatten", flat{}, []string{"created_at", "writer.id", "writer.name", "title", "created"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := names(t, tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFieldsOfErrors(t *testing.T) {
	type dup struct {
		A string `db:"x"`
		B string `db:"x"`
	}
	type clash struct { // a.b and a_b share the alias a_b
		A struct{ B string } `db:"a,inline"`
		C string             `db:"a_b"`
	}
	type Recursive struct {
		*Recursive
	}
	type badInline struct {
		N int `db:",inline"`
	}
	for name, v := range map[string]any{"dup": dup{}, "alias clash": clash{}, "recursive": Recursive{}, "bad inline": badInline{}} {
		if _, err := FieldsOf(reflect.TypeOf(v)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestFieldsOfLeaf(t *testing.T) {
	for _, v := range []any{0, "", time.Time{}, sql.NullString{}, new(int64)} {
		if fs, err := FieldsOf(reflect.TypeOf(v)); fs != nil || err != nil {
			t.Errorf("%T: got %v, %v; want nil, nil", v, fs, err)
		}
	}
}

func TestGetAndPtr(t *testing.T) {
	type row struct {
		*Author
		Age *int
	}
	fs, err := FieldsOf(reflect.TypeFor[row]())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Field{}
	for _, f := range fs {
		byName[f.Name] = f
	}

	// Get through a nil embedded pointer yields NULL instead of panicking.
	if got := byName["author.name"].Get(reflect.ValueOf(row{})); got != nil {
		t.Errorf("Get through nil pointer: got %v, want nil", got)
	}

	// Ptr allocates the embedded pointer.
	var r row
	rv := reflect.ValueOf(&r).Elem()
	*(byName["author.name"].Ptr(rv).(*string)) = "alice"
	if r.Author == nil || r.Name != "alice" {
		t.Errorf("Ptr did not allocate embedded pointer: %+v", r)
	}
	if got := byName["author.name"].Get(rv); got != "alice" {
		t.Errorf("Get: got %v", got)
	}
}
