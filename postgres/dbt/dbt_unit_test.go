package dbt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"testing"
)

// ---- minimal fake driver ----

type fake struct {
	cols []string
	rows [][]driver.Value
	args []driver.Value
}

func (f *fake) Connect(context.Context) (driver.Conn, error) { return (*fakeConn)(f), nil }
func (f *fake) Driver() driver.Driver                        { return nil }

type fakeConn fake

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return (*fakeStmt)(c), nil }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("unsupported") }

type fakeStmt fake

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }
func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.args = args
	return driver.RowsAffected(1), nil
}
func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.args = args
	return &fakeRows{f: (*fake)(s)}, nil
}

type fakeRows struct {
	f *fake
	i int
}

func (r *fakeRows) Columns() []string { return r.f.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.f.rows) {
		return io.EOF
	}
	copy(dest, r.f.rows[r.i])
	r.i++
	return nil
}

func open(f *fake) *sql.DB { return sql.OpenDB(f) }

// ---- types ----

type user struct {
	ID   int64  `db:"id"`
	Name string `db:"name"`
	Age  *int   `db:"age"`
}

type byID struct {
	ID int64 `db:"id"`
}

type IDParam struct {
	ID int64 `db:"id"`
}

type tenantParams struct {
	*IDParam `db:",flatten"`
	Tenant   string `db:"tenant"`
}

var ctx = context.Background()

func TestScanSubsetAndReorderedColumns(t *testing.T) {
	// Selecting fewer columns, in a different order, than the struct has.
	q := Must(New[byID, user](`select {{ cols "=" "name" "id" }} from users where id = @id`))
	db := open(&fake{cols: []string{"name", "id"}, rows: [][]driver.Value{{"alice", int64(7)}}})

	got, err := q.QueryRowContext(ctx, db, byID{ID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || got.Name != "alice" || got.Age != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestScanNullablePointerAndPointerRow(t *testing.T) {
	q := Must(New[byID, *user](`select {{ cols }} from users`))
	db := open(&fake{cols: []string{"id", "name", "age"}, rows: [][]driver.Value{
		{int64(1), "a", int64(30)},
		{int64(2), "b", nil},
	}})
	got, err := q.QueryContext(ctx, db, byID{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Age == nil || *got[0].Age != 30 || got[1].Age != nil {
		t.Fatalf("got %+v %+v", got[0], got[1])
	}
}

func TestQueryEmptyIsNonNil(t *testing.T) {
	q := Must(New[byID, user](`select {{ cols }} from users`))
	got, err := q.QueryContext(ctx, open(&fake{cols: []string{"id"}}), byID{})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %#v, %v", got, err)
	}
}

func TestQueryRowNoRows(t *testing.T) {
	q := Must(New[byID, user](`select {{ cols }} from users`))
	_, err := q.QueryRowContext(ctx, open(&fake{cols: []string{"id"}}), byID{})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("got %v, want sql.ErrNoRows", err)
	}
}

func TestScalarRow(t *testing.T) {
	q := Must(New[byID, int64](`select count(*) from users where id = @id`))
	got, err := q.QueryRowContext(ctx, open(&fake{cols: []string{"count"}, rows: [][]driver.Value{{int64(42)}}}), byID{ID: 1})
	if err != nil || got != 42 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestUnknownResultColumn(t *testing.T) {
	q := Must(New[byID, user](`select {{ cols }} from users`))
	_, err := q.QueryContext(ctx, open(&fake{cols: []string{"id", "surprise"}, rows: [][]driver.Value{{int64(1), "x"}}}), byID{})
	if !errors.Is(err, ErrUnknownColumn) {
		t.Fatalf("got %v, want ErrUnknownColumn", err)
	}
}

func TestArgsOrderRepeatsAndNilEmbedded(t *testing.T) {
	q := Must(New[tenantParams, user](`select {{ cols }} from users where tenant = @tenant and id = @id and parent = @id`))

	args, err := q.Args(tenantParams{IDParam: &IDParam{ID: 9}, Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{"acme", int64(9)}; !reflect.DeepEqual(args, want) {
		t.Fatalf("got %v, want %v", args, want)
	}

	// A nil embedded pointer becomes NULL rather than panicking.
	args, err = q.Args(tenantParams{Tenant: "acme"})
	if err != nil || args[1] != nil {
		t.Fatalf("got %v, %v", args, err)
	}

	// nil *params is an error, not a panic.
	qp := Must(New[*byID, user](`select {{ cols }} from users where id = @id`))
	if _, err := qp.Args(nil); !errors.Is(err, ErrNilParams) {
		t.Fatalf("got %v, want ErrNilParams", err)
	}
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		want error // nil: any error
	}{
		{"unknown param", `select 1 where x = @nope`, ErrUnknownParam},
		{"typo in exclusion", `select {{ cols "-" "nmae" }} from users`, ErrUnknownColumn},
		{"nothing left to select", `select {{ cols "-" "id" "name" "age" }} from users`, ErrNoColumns},
		{"bad template", `select {{ cols `, nil},
		{"unknown func", `select {{ nope }}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New[byID, user](tt.tmpl)
			if err == nil {
				t.Fatal("want error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestLiteralsAreNotParams(t *testing.T) {
	q, err := New[byID, user](`select {{ cols }} from users where email = 'a@example.com' and id = @id`)
	if err != nil {
		t.Fatal(err)
	}
	_, args, _ := q.Build(byID{ID: 3})
	if !reflect.DeepEqual(args, []any{int64(3)}) {
		t.Fatalf("got %v", args)
	}
}

func TestCustomDelims(t *testing.T) {
	q, err := New[byID, user](`select '{{1,2},{3,4}}'::int[] from users where id = @id`, WithDelims("<%", "%>"))
	if err != nil {
		t.Fatal(err)
	}
	_ = q
}

func TestSetAndVals(t *testing.T) {
	type upd struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
	}
	if _, err := New[upd, user](`update users set {{ set "-" "id" }} where id = @id`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewExec[upd](`insert into users {{ vals }}`); err != nil {
		t.Fatal(err)
	}
}
