package jsonb_test

import (
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

	_, err = db.Exec(`create table books (
		id uuid default uuidv7(),
		title text not null
	);
	`)
	return err
}

type Book struct {
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
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

	type Params struct {
		ID    uuid.UUID `json:"id"`
		Title string    `json:"title"`
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
}
