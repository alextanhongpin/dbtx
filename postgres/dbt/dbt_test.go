package dbt_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/dbt"
	"github.com/stretchr/testify/assert"
)

type ABC struct {
}

func (a *ABC) Scan() map[string]any {
	return map[string]any{
		"a": nil,
		"b": nil,
		"c": nil,
	}
}

type Params struct{}

func (p *Params) Value() map[string]any {
	return map[string]any{
		"col1": "val1",
		"col2": "val2",
		"col3": "val3",
	}
}

func TestDBT_columns(t *testing.T) {
	tests := []struct {
		name string
		want string
		got  string
	}{
		{
			name: "when no select",
			want: ``,
			got:  dbt.New[dbt.NoSelect, dbt.NoArgs](`{{ columns }}`).String(),
		},
		{
			name: "when no insert",
			want: ``,
			got:  dbt.New[dbt.NoSelect, dbt.NoArgs](`{{ insert }}`).String(),
		},
		{
			name: "when no set",
			want: ``,
			got:  dbt.New[dbt.NoSelect, dbt.NoArgs](`{{ set "*" }}`).String(),
		},
		{
			name: "with columns",
			want: `a, b, c`,
			got:  dbt.New[ABC, dbt.NoArgs](`{{ columns }}`).String(),
		},
		{
			name: "with columns schema",
			want: `my.a, my.b, my.c`,
			got:  dbt.New[ABC, dbt.NoArgs](`{{ columns "my" }}`).String(),
		},
		{
			name: "with columns alias",
			want: `my.a AS my_a, my.b AS my_b, my.c AS my_c`,
			got:  dbt.New[ABC, dbt.NoArgs](`{{ columns "my" "my" }}`).String(),
		},
		{
			name: "set *",
			want: `col1 = $1, col2 = $2, col3 = $3`,
			got:  dbt.New[dbt.NoSelect, Params](`{{ set "*" }}`).String(),
		},
		{
			name: "set in",
			want: `col1 = $1, col2 = $2, col3 = $3`,
			got:  dbt.New[dbt.NoSelect, Params](`{{ set "in" "col1" }}, col2 = @col2, col3 = @col3`).String(),
		},
		{
			name: "set ex",
			want: `col1 = $1, col2 = $2, col3 = $3`,
			got:  dbt.New[dbt.NoSelect, Params](`{{ set "ex" "col3" }}, col3 = @col3`).String(),
		},
		{
			name: "named parameters",
			want: "col3 = $1, col2 = $2, col1 = $3, col3 = $1",
			got:  dbt.New[dbt.NoSelect, Params](`col3 = @col3, col2 = @col2, col1 = @col1, col3 = @col3`).String(),
		},
	}

	is := assert.New(t)
	for _, tt := range tests {
		is.Equal(tt.want, tt.got, tt.name)
	}
}

func TestDBT_params(t *testing.T) {
	tests := []struct {
		name string
		want []any
		got  []any
	}{
		{
			name: "when no args",
			want: []any{},
			got:  dbt.New[dbt.NoSelect, dbt.NoArgs](`{{ columns }}`).Args(&dbt.NoArgs{}),
		},
		{
			name: "set *",
			want: []any{"val1", "val2", "val3"},
			got:  dbt.New[dbt.NoSelect, Params](`{{ set "*" }}`).Args(&Params{}),
		},
		{
			name: "set in",
			want: []any{"val1", "val2", "val3"},
			got:  dbt.New[dbt.NoSelect, Params](`{{ set "in" "col1" }}, col2 = @col2, col3 = @col3`).Args(&Params{}),
		},
		{
			name: "set ex",
			want: []any{"val1", "val2", "val3"},
			got:  dbt.New[dbt.NoSelect, Params](`{{ set "ex" "col3" }}, col3 = @col3`).Args(&Params{}),
		},
		{
			name: "named parameters",
			want: []any{"val3", "val2", "val1"},
			got:  dbt.New[dbt.NoSelect, Params](`col3 = @col3, col2 = @col2, col1 = @col1, col3 = @col3`).Args(&Params{}),
		},
	}

	is := assert.New(t)
	for _, tt := range tests {
		is.Equal(tt.want, tt.got, tt.name)
	}
}

func ExampleNew_insert() {
	q := dbt.New[User, InsertUserParams](`INSERT INTO users {{ insert }} RETURNING {{ columns }}`)

	fmt.Println(q.String())
	fmt.Println(q.Args(&InsertUserParams{
		Name:  "john",
		Email: "john@appleseed.com",
	}))

	// Output:
	// INSERT INTO users (email, name) VALUES ($1, $2) RETURNING created_at, email, id, name, updated_at
	// [john@appleseed.com john]
}

func ExampleNew_select() {
	q := dbt.New[User, FilterUserParams](`SELECT {{ columns "u" "user" }}
FROM users u
WHERE name = @name AND email = @email AND age = @age
LIMIT 3`)

	fmt.Println(q.String())
	fmt.Println(q.Args(&FilterUserParams{
		Name:  "john",
		Email: "john.appleseed@mail.com",
		Age:   20,
	}))
}

func ExampleNew_update() {
	q := dbt.New[User, FilterUserParams](`UPDATE users
SET {{ set "ex" "email" }}
WHERE email = @email
LIMIT 3`)

	fmt.Println(q.String())
	fmt.Println(q.Args(&FilterUserParams{
		Name:  "john",
		Email: "john.appleseed@mail.com",
		Age:   32,
	}))

	// Output:
	// UPDATE users
	// SET age = $1, name = $2
	// WHERE email = $3
	// LIMIT 3
	// [32 john john.appleseed@mail.com]
}

func ExampleNew_aggregate() {
	q := dbt.New[UserBookAggregate, dbt.NoArgs](`SELECT {{ columns }}
FROM users u
JOIN books b ON (u.id = b.user_id)`)
	fmt.Println(q.String())
	fmt.Println(q.Args(&dbt.NoArgs{}))

	// Output:
	// SELECT b.author AS book_author, b.created_at AS book_created_at, b.id AS book_id, b.isbn AS book_isbn, b.published_at AS book_published_at, b.title AS book_title, b.updated_at AS book_updated_at, u.created_at AS user_created_at, u.email AS user_email, u.id AS user_id, u.name AS user_name, u.updated_at AS user_updated_at, ub.book_id AS user_book_book_id, ub.created_at AS user_book_created_at, ub.id AS user_book_id, ub.status AS user_book_status, ub.updated_at AS user_book_updated_at, ub.user_id AS user_book_user_id
	// FROM users u
	// JOIN books b ON (u.id = b.user_id)
	// []
}

type User struct {
	ID        int
	Name      string
	Email     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (u *User) Scan() map[string]any {
	return map[string]any{
		"id":         &u.ID,
		"name":       &u.Name,
		"email":      &u.Email,
		"created_at": &u.CreatedAt,
		"updated_at": &u.UpdatedAt,
	}
}

type InsertUserParams struct {
	Name  string
	Email string
}

func (p *InsertUserParams) Value() map[string]any {
	return map[string]any{
		"name":  p.Name,
		"email": p.Email,
	}
}

type FilterUserParams struct {
	Name  string
	Email string
	Age   int
}

func (p *FilterUserParams) Value() map[string]any {
	return map[string]any{
		"name":  p.Name,
		"email": p.Email,
		"age":   p.Age,
	}
}

type Book struct {
	ID          int
	Title       string
	Author      string
	PublishedAt *time.Time
	ISBN        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (b *Book) Scan() map[string]any {
	return map[string]any{
		"id":           &b.ID,
		"title":        &b.Title,
		"author":       &b.Author,
		"published_at": &b.PublishedAt,
		"isbn":         &b.ISBN,
		"created_at":   &b.CreatedAt,
		"updated_at":   &b.UpdatedAt,
	}
}

type UserBook struct {
	ID        int
	UserID    int
	BookID    int
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (ub *UserBook) Scan() map[string]any {
	return map[string]any{
		"id":         &ub.ID,
		"user_id":    &ub.UserID,
		"book_id":    &ub.BookID,
		"status":     &ub.Status,
		"created_at": &ub.CreatedAt,
		"updated_at": &ub.UpdatedAt,
	}
}

type UserBookAggregate struct {
	UserBook UserBook
	User     User
	Book     Book
}

func (ub *UserBookAggregate) Scan() map[string]any {
	ubm := dbt.M(ub.UserBook.Scan())
	bm := dbt.M(ub.Book.Scan())
	um := dbt.M(ub.User.Scan())

	return dbt.Merge(
		ubm.As("ub", "user_book"),
		bm.As("b", "book"),
		um.As("u", "user"),
	)
}
