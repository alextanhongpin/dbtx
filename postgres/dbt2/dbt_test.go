package dbt_test

import (
	"database/sql"
	"testing"
	"uuid"

	_ "github.com/lib/pq"

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
	create table user_books (
		id uuid default uuidv7(),
		user_id uuid NOT NULL,
		book_id uuid NOT NULL,
		foreign key (user_id) references users(id),
		foreign key (book_id) references books(id),
		primary key (id)
	);
	`)
	return err
}

func TestDBT(t *testing.T) {
	ctx := t.Context()
	db := dbtest.DB(t)

	var u User
	var b Book

	t.Run("create user", func(t *testing.T) {
		create, err := dbt.New[CreateUserParams, User]("insert into users {{ vals }} returning {{ cols }}")
		if err != nil {
			t.Fatal(err)
		}
		params := CreateUserParams{Name: "john"}
		t.Log(create.Build(params))
		u, err = create.QueryRowContext(ctx, db, params)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(u)
	})

	t.Run("update user", func(t *testing.T) {
		update, err := dbt.New[UpdateUserParams, User]("update users set name = @name where id = @id returning {{ cols }}")
		if err != nil {
			t.Fatal(err)
		}
		params := UpdateUserParams{Name: "jessie", ID: u.ID}
		t.Log(update.Build(params))
		t.Log(update.QueryRowContext(ctx, db, params))
	})

	t.Run("list user", func(t *testing.T) {
		list, err := dbt.New[any, User]("select {{ cols }} from users")
		if err != nil {
			t.Fatal(err)
		}
		t.Log(list.Build(nil))
		res, err := list.QueryContext(ctx, db, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(res)
		for _, u := range res {
			t.Logf("%#v\n", u)
		}
	})

	t.Run("create book", func(t *testing.T) {
		create, err := dbt.New[Book, Book](`insert into books {{ vals "-" "id" }} returning {{ cols }}`)
		if err != nil {
			t.Fatal(err)
		}
		params := Book{Title: "hallo"}
		t.Log(create.Build(params))
		b, err = create.QueryRowContext(ctx, db, params)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(b)
	})

	t.Run("create user book", func(t *testing.T) {
		create, err := dbt.New[CreateUserBookParams, UserBook](`insert into user_books {{ vals }} returning {{ cols }}`)
		if err != nil {
			t.Fatal(err)
		}
		params := CreateUserBookParams{
			UserID: u.ID,
			BookID: b.ID,
		}
		t.Log(create.Build(params))
		ub, err := create.QueryRowContext(ctx, db, params)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(ub)
	})

	t.Run("join user book", func(t *testing.T) {
		list, err := dbt.New[any, UserBookAggregate](`select {{ cols }} from users u join books b on true`)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(list.Build(nil))
		ub, err := list.QueryRowContext(ctx, db, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(ub)
	})
}

func TestDBT_Pointer(t *testing.T) {
	ctx := t.Context()
	db := dbtest.DB(t)

	var u *User
	var b *Book

	t.Run("create user", func(t *testing.T) {
		create, err := dbt.New[*CreateUserParams, *User]("insert into users {{ vals }} returning {{ cols }}")
		if err != nil {
			t.Fatal(err)
		}
		params := &CreateUserParams{Name: "john"}
		t.Log(create.Build(params))
		u, err = create.QueryRowContext(ctx, db, params)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(u)
	})

	t.Run("update user", func(t *testing.T) {
		update, err := dbt.New[*UpdateUserParams, *User]("update users set name = @name where id = @id returning {{ cols }}")
		if err != nil {
			t.Fatal(err)
		}
		params := &UpdateUserParams{Name: "jessie", ID: u.ID}
		t.Log(update.Build(params))
		t.Log(update.QueryRowContext(ctx, db, params))
	})

	t.Run("list user", func(t *testing.T) {
		list, err := dbt.New[any, *User]("select {{ cols }} from users")
		if err != nil {
			t.Fatal(err)
		}
		t.Log(list.Build(nil))
		res, err := list.QueryContext(ctx, db, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(res)
		for _, u := range res {
			t.Logf("%#v\n", u)
		}
	})

	t.Run("create book", func(t *testing.T) {
		create, err := dbt.New[*Book, *Book](`insert into books {{ vals "-" "id" }} returning {{ cols }}`)
		if err != nil {
			t.Fatal(err)
		}
		params := &Book{Title: "hallo"}
		t.Log(create.Build(params))
		b, err = create.QueryRowContext(ctx, db, params)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(b)
	})

	t.Run("create user book", func(t *testing.T) {
		create, err := dbt.New[*CreateUserBookParams, *UserBook](`insert into user_books {{ vals }} returning {{ cols }}`)
		if err != nil {
			t.Fatal(err)
		}
		params := &CreateUserBookParams{
			UserID: u.ID,
			BookID: b.ID,
		}
		t.Log(create.Build(params))
		ub, err := create.QueryRowContext(ctx, db, params)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(ub)
	})

	t.Run("join user book", func(t *testing.T) {
		list, err := dbt.New[any, *UserBookAggregatePointer](`select {{ cols }} from users u join books b on true`)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(list.Build(nil))
		ub, err := list.QueryRowContext(ctx, db, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(ub)
	})
}

type Book struct {
	ID    uuid.UUID `db:"id"`
	Title string    `db:"title"`
}

type User struct {
	ID   uuid.UUID `db:"id"`
	Name string    `db:"name"`
}

type CreateUserBookParams struct {
	UserID uuid.UUID `db:"user_id"`
	BookID uuid.UUID `db:"book_id"`
}

type UserBook struct {
	ID     uuid.UUID `db:"id"`
	UserID uuid.UUID `db:"user_id"`
	BookID uuid.UUID `db:"book_id"`
}

type UserBookAggregate struct {
	User `db:"u,inline"`
	Book `db:"b,inline"`
}

type UserBookAggregatePointer struct {
	*User `db:"u,inline"`
	*Book `db:"b,inline"`
}

type CreateUserParams struct {
	Name string `db:"name"`
}

type UpdateUserParams struct {
	ID   uuid.UUID `db:"id"`
	Name string    `db:"name"`
}
