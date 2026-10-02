package jsonb_test

import (
	"time"

	"github.com/lib/pq"

	"database/sql"
	"testing"
	"uuid"

	"github.com/alextanhongpin/dbtx/postgres/jsonb"
	"github.com/alextanhongpin/dbtx/testing/dbtest"
	"github.com/stretchr/testify/assert"
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
	create table books (
		id uuid default uuidv7(),
		title text not null,
		created_at timestamptz not null default now(),
		updated_at timestamptz not null default now(),
		primary key (id)
	);
	create table users (
		id uuid default uuidv7(),
		name text not null,
		created_at timestamptz not null default now(),
		updated_at timestamptz not null default now(),
		primary key (id)
	);
	create table authors (
		id uuid default uuidv7(),
		user_id uuid not null,
		book_id uuid not null,
		created_at timestamptz not null default now(),
		updated_at timestamptz not null default now(),
		primary key (id),
		foreign key (user_id) references users(id),
		foreign key (book_id) references books(id)
	);`)
	return err
}

type User struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Book struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Author struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"user_id"`
	BookID    uuid.UUID `json:"book_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PartialBook struct {
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ExtraBook struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
}

type Aggregate struct {
	Author *Author `json:"author"`
	Book   *Book   `json:"book"`
	User   *User   `json:"user"`
}

func TestJSONB(t *testing.T) {
	ctx := t.Context()
	js := jsonb.New(dbtest.DB(t))

	res, err := js.QueryRowContext[[]Book](ctx, `
with inserted as (
	insert into books (title)
	select *
	from unnest($1::text[])
	returning *
)
-- when returning multiple, aggregate it first.
select jsonb_agg(inserted) from inserted;
	`, pq.Array([]string{"a", "b", "c"}))
	is := assert.New(t)
	is.NoError(err)
	is.Len(res, 3)
	for i, b := range res {
		t.Logf("%d) %v\n", i+1, b)
	}

	res, err = js.QueryRowContext[[]Book](ctx, `
select jsonb_agg(b)
from books b
where b.title > $1
	`, "a")
	is.NoError(err)
	is.Len(res, 2)
	for i, b := range res {
		t.Logf("%d) %v\n", i+1, b)
	}

	b, err := js.QueryRowContext[Book](ctx, `
update books
set title = $1
where books.id = $2
-- when returning 1 row, use to_jsonb.
returning to_jsonb(books)
	`, "edited", res[0].ID)
	is.NoError(err)
	t.Log(b)

	t.Run("missing", func(t *testing.T) {
		b, err := js.QueryContext[PartialBook](ctx, `select to_jsonb(b) from books b`)
		is.NoError(err)
		t.Log(b)
	})

	t.Run("additional", func(t *testing.T) {
		b, err := js.QueryContext[ExtraBook](ctx, `select to_jsonb(b) from books b`)
		is.NoError(err)
		t.Log(b)
	})

	t.Run("nested", func(t *testing.T) {
		book, err := js.QueryRowContext[Book](ctx, "insert into books (title) values ($1) returning to_jsonb(books)", t.Name())
		if err != nil {
			t.Fatal(err)
		}
		user, err := js.QueryRowContext[User](ctx, "insert into users (name) values ($1) returning to_jsonb(users)", t.Name())
		if err != nil {
			t.Fatal(err)
		}

		_, err = js.ExecContext(ctx, "insert into authors (user_id, book_id) values ($1, $2)", user.ID, book.ID)
		if err != nil {
			t.Fatal(err)
		}

		a, err := js.QueryContext[Aggregate](ctx, `
			select json_build_object(
				'author', to_jsonb(a),
				'user', to_jsonb(u),
				'book', to_jsonb(b)
			)
			from authors a
			join books b on (b.id = a.book_id)
			join users u on (u.id = a.user_id)
		`)
		is.NoError(err)
		for _, i := range a {
			t.Logf("book=%v author=%v user=%v", *i.Book, *i.Author, *i.User)
		}
	})
}
