package dbt_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/lib/pq"

	"github.com/alextanhongpin/dbtx/postgres/dbt"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
)

func TestMain(m *testing.M) {
	opts := dbtest.Options{
		Image: "postgres:19beta3-alpine3.24",
		Hook:  migrate,
	}
	stop := dbtest.Init(opts)
	defer stop()
	m.Run()
}

func migrate(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`
		create table users (
			id uuid default uuidv7(),
			name text not null,
			primary key (id)
		);

		create table books (
			id uuid default uuidv7(),
			title text not null,
			primary key (id)
		);

		-- Test joins.
		create table user_books (
			id uuid default uuidv7(),
			user_id uuid NOT NULL,
			book_id uuid NOT NULL,
			foreign key (user_id) references users(id),
			foreign key (book_id) references books(id),
			primary key (id)
		);

		-- Test serialization of different types.
		create table go_types (
			id int generated always as identity primary key,

			-- nullables
			uuid uuid,
			name text,
			age int,
			married bool,
			timestamp timestamptz,

			-- arrays
			tags text[]
		);

		-- Test that reserved words are quoted.
		create table reserved (
			id int generated always as identity primary key,
			"order" int not null,
			"group" text not null
		);
	`)
	return err
}

func TestDBT(t *testing.T) {
	db := dbtest.DB(t)

	repo := &Repository{db: db}
	t.Run("create and read user", func(t *testing.T) {
		ctx := t.Context()
		u, err := repo.CreateUser(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if u.ID == uuid.Nil() || u.Name != "alice" {
			t.Fatalf("unexpected user: %+v", u)
		}
		t.Logf("created user: %+v", u)

		// update
		u2, err := repo.UpdateUser(ctx, u.ID, "alice-renamed")
		if err != nil {
			t.Fatal(err)
		}
		if u2.ID != u.ID || u2.Name != "alice-renamed" {
			t.Fatalf("update failed: %+v", u2)
		}
		t.Logf("updated user: %+v", u2)

		// list
		rows, err := repo.ListUsers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			t.Fatal("want at least one user")
		}
	})

	t.Run("create book and relation", func(t *testing.T) {
		ctx := t.Context()
		b, err := repo.CreateBook(ctx, "golang")
		if err != nil {
			t.Fatal(err)
		}
		if b.ID == uuid.Nil() {
			t.Fatal("book id not set")
		}
		t.Logf("created book: %+v", b)

		// create user_book link
		// first create a user
		u, err := repo.CreateUser(ctx, "bob")
		if err != nil {
			t.Fatal(err)
		}

		ub, err := repo.CreateUserBook(ctx, u.ID, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ub.UserID != u.ID || ub.BookID != b.ID {
			t.Fatalf("unexpected link: %+v", ub)
		}
		t.Logf("created link: %+v", ub)
	})

	t.Run("aggregate with inline", func(t *testing.T) {
		ctx := t.Context()
		rows, err := repo.ListUserBook(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			t.Fatal("want rows")
		}
		// Each embedded struct is filled from its own prefix (u_*, b_*, ub_*).
		for _, row := range rows {
			if row.User.ID == uuid.Nil() || row.User.Name == "" {
				t.Fatalf("user not scanned: %+v", row)
			}
			if row.Book.ID == uuid.Nil() || row.Book.Title == "" {
				t.Fatalf("book not scanned: %+v", row)
			}
		}
	})

	t.Run("aggregate with embedded pointers", func(t *testing.T) {
		ctx := t.Context()
		rows, err := listUserBookPointers.QueryContext(ctx, db, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			t.Fatal("want rows")
		}
		for _, row := range rows {
			// Nil embedded pointers are allocated while scanning.
			if row.User == nil || row.Book == nil {
				t.Fatalf("embedded pointers not allocated: %+v", row)
			}
		}
	})

	t.Run("missing row is sql.ErrNoRows", func(t *testing.T) {
		_, err := findUser.QueryRowContext(t.Context(), db, FindUserParams{ID: uuid.Nil()})
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("want sql.ErrNoRows, got %v", err)
		}
	})

	t.Run("select fewer columns than the struct has", func(t *testing.T) {
		ctx := t.Context()

		// R only has Name, so only name is selected.
		names, err := listUserNames.QueryContext(ctx, db, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(names) == 0 || names[0].Name == "" {
			t.Fatalf("unexpected names: %+v", names)
		}

		// R has ID and Name, but only name is selected: ID stays zero.
		users, err := listUsersWithoutID.QueryContext(ctx, db, nil)
		if err != nil {
			t.Fatal(err)
		}
		if users[0].ID != uuid.Nil() || users[0].Name == "" {
			t.Fatalf("unexpected user: %+v", users[0])
		}

		// Reordered columns are mapped by name, not by position.
		users, err = listUsersReordered.QueryContext(ctx, db, nil)
		if err != nil {
			t.Fatal(err)
		}
		if users[0].ID == uuid.Nil() || users[0].Name == "" {
			t.Fatalf("unexpected user: %+v", users[0])
		}
	})

	t.Run("scalar result", func(t *testing.T) {
		n, err := countUsers.QueryRowContext(t.Context(), db, nil)
		if err != nil {
			t.Fatal(err)
		}
		if n < 2 {
			t.Fatalf("want at least 2 users, got %d", n)
		}
	})

	t.Run("column missing from the struct is an error", func(t *testing.T) {
		_, err := listUsersMismatch.QueryContext(t.Context(), db, nil)
		if !errors.Is(err, dbt.ErrUnknownColumn) {
			t.Fatalf("want dbt.ErrUnknownColumn, got %v", err)
		}
	})
}

func TestDBT_reserved_words(t *testing.T) {
	db := dbtest.DB(t)

	got, err := createReserved.QueryRowContext(t.Context(), db, Reserved{Order: 1, Group: "admins"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 || got.Order != 1 || got.Group != "admins" {
		t.Fatalf("unexpected row: %+v", got)
	}
}

func TestDBT_go_types(t *testing.T) {
	db := dbtest.DB(t)
	ctx := t.Context()

	res, err := createGoType.QueryRowContext(ctx, db, GoType{})
	if err != nil {
		t.Fatal(err)
	}
	if res.UUID != nil || res.Name != nil || res.Age != nil || res.Tags != nil {
		t.Fatalf("want NULLs to scan into nil, got %#v", res)
	}

	params := GoType{
		UUID:      new(uuid.Nil()),
		Name:      new(t.Name()),
		Age:       new(42),
		Married:   new(true),
		Timestamp: new(time.Now()),
		Tags:      pq.StringArray{"foo", "bar"},
	}
	res, err = createGoType.QueryRowContext(ctx, db, params)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name == nil || *res.Name != t.Name() || res.Age == nil || *res.Age != 42 || len(res.Tags) != 2 {
		t.Fatalf("unexpected row: %#v", res)
	}

	rows, err := listGoTypes.QueryContext(ctx, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 {
		t.Fatalf("want at least 2 rows, got %d", len(rows))
	}
}

// TestCompile checks that every combination of value/pointer for the row type
// and for embedded structs compiles to the same prefixed, aliased columns.
func TestCompile(t *testing.T) {
	const tmpl = `select {{ cols }} from users u join books b on true`

	queries := map[string]fmt.Stringer{
		"value":                      dbt.Must(dbt.New[any, UserBookAggregate](tmpl)),
		"pointer":                    dbt.Must(dbt.New[any, *UserBookAggregate](tmpl)),
		"embedded pointers":          dbt.Must(dbt.New[any, UserBookAggregatePointer](tmpl)),
		"pointer, embedded pointers": dbt.Must(dbt.New[any, *UserBookAggregatePointer](tmpl)),
	}
	for name, q := range queries {
		t.Run(name, func(t *testing.T) {
			got := strings.ToLower(q.String())
			for _, want := range []string{"u_id", "u_name", "b_id", "b_title"} {
				if !strings.Contains(got, want) {
					t.Errorf("%q not found in %s", want, q)
				}
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name string
		new  func() error
		want error
	}{
		{"unknown param", func() error {
			_, err := dbt.New[FindUserParams, User](`select {{ cols }} from users where id = @nope`)
			return err
		}, dbt.ErrUnknownParam},
		{"typo in exclusion", func() error {
			_, err := dbt.New[any, User](`select {{ cols "-" "nmae" }} from users`)
			return err
		}, dbt.ErrUnknownColumn},
		{"invalid sql", func() error {
			_, err := dbt.New[any, User](`selec {{ cols }} from users`)
			return err
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.new()
			if err == nil {
				t.Fatal("want error at compile time")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

type Book struct {
	ID    uuid.UUID
	Title string
}

type User struct {
	ID   uuid.UUID
	Name string
}

type NameOnly struct {
	Name string
}

type FindUserParams struct {
	ID uuid.UUID
}

type CreateUserBookParams struct {
	UserID uuid.UUID
	BookID uuid.UUID
}

type UserBook struct {
	ID     uuid.UUID
	UserID uuid.UUID
	BookID uuid.UUID
}

// The tag on an embedded struct is the table alias used in the query: its
// columns are selected as u.id AS u_id, u.name AS u_name, etc.
type UserBookAggregate struct {
	User `db:"u"`
	Book `db:"b"`
}

type UserBookAggregatePointer struct {
	*User `db:"u"`
	*Book `db:"b"`
}

type CreateUserParams struct {
	Name string
}

type UpdateUserParams struct {
	ID   uuid.UUID
	Name string
}

type Reserved struct {
	ID    int
	Order int
	Group string
}

type GoType struct {
	ID        int
	UUID      *uuid.UUID
	Name      *string
	Age       *int
	Married   *bool
	Timestamp *time.Time
	Tags      pq.StringArray
}

type Repository struct {
	db *sql.DB
}

var (
	createUser     = dbt.Must(dbt.New[CreateUserParams, *User]("insert into users {{ vals }} returning {{ cols }}"))
	updateUser     = dbt.Must(dbt.New[UpdateUserParams, *User](`update users set {{ set "-" "id" }} where id = @id returning {{ cols }}`))
	findUser       = dbt.Must(dbt.New[FindUserParams, *User](`select {{ cols }} from users where id = @id or name = 'nobody@example.com'`))
	listUsers      = dbt.Must(dbt.New[any, User]("select {{ cols }} from users"))
	createBook     = dbt.Must(dbt.New[Book, *Book](`insert into books {{ vals "-" "id" }} returning {{ cols }}`))
	createUserBook = dbt.Must(dbt.New[CreateUserBookParams, *UserBook](`insert into user_books {{ vals }} returning {{ cols }}`))
	listUserBooks  = dbt.Must(dbt.New[any, UserBookAggregate](`select {{ cols }} from users u join books b on true`))
	createGoType   = dbt.Must(dbt.New[GoType, GoType](`insert into go_types {{ vals "-" "id" }} returning {{ cols }}`))
	listGoTypes    = dbt.Must(dbt.New[any, GoType](`select {{ cols }} from go_types`))
	createReserved = dbt.Must(dbt.New[Reserved, Reserved](`insert into reserved {{ vals "-" "id" }} returning {{ cols }}`))

	listUserBookPointers = dbt.Must(dbt.New[any, UserBookAggregatePointer](`select {{ cols }} from users u join books b on true`))

	// Subsets and reordering of the struct's columns.
	listUserNames      = dbt.Must(dbt.New[any, NameOnly](`select {{ cols }} from users order by name`))
	listUsersWithoutID = dbt.Must(dbt.New[any, User](`select {{ cols "-" "id" }} from users order by name`))
	listUsersReordered = dbt.Must(dbt.New[any, User](`select {{ cols "=" "name" "id" }} from users order by name`))

	// Scalars.
	countUsers = dbt.Must(dbt.New[any, int64](`select count(*) from users`))

	// Selects "id", which NameOnly has no field for.
	listUsersMismatch = dbt.Must(dbt.New[any, NameOnly](`select id, name from users`))
)

func (r *Repository) CreateUser(ctx context.Context, name string) (*User, error) {
	return createUser.QueryRowContext(ctx, r.db, CreateUserParams{
		Name: name,
	})
}

func (r *Repository) UpdateUser(ctx context.Context, id uuid.UUID, name string) (*User, error) {
	return updateUser.QueryRowContext(ctx, r.db, UpdateUserParams{
		ID:   id,
		Name: name,
	})
}

func (r *Repository) ListUsers(ctx context.Context) ([]User, error) {
	return listUsers.QueryContext(ctx, r.db, nil)
}

func (r *Repository) CreateBook(ctx context.Context, title string) (*Book, error) {
	return createBook.QueryRowContext(ctx, r.db, Book{
		Title: title,
	})
}

func (r *Repository) CreateUserBook(ctx context.Context, userID, bookID uuid.UUID) (*UserBook, error) {
	return createUserBook.QueryRowContext(ctx, r.db, CreateUserBookParams{
		UserID: userID,
		BookID: bookID,
	})
}

func (r *Repository) ListUserBook(ctx context.Context) ([]UserBookAggregate, error) {
	return listUserBooks.QueryContext(ctx, r.db, nil)
}
