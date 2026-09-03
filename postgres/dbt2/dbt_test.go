package dbt_test

import (
	"context"
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
		if u2.Name != "alice-renamed" {
			t.Fatalf("update failed: %+v", u2)
		}
		t.Logf("updated user: %+v", u2)

		// list
		rows, err := repo.ListUsers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("found %d users", len(rows))
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
		t.Logf("created link: %+v", ub)
	})

	t.Run("aggregate with inline", func(t *testing.T) {
		ctx := t.Context()
		rows, err := repo.ListUserBook(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("found %d user book", len(rows))
	})
}

func TestParse(t *testing.T) {
	t.Log(dbt.Parse[CreateUserBookParams, User](`{{ cols }}`))
	t.Log(dbt.Parse[CreateUserBookParams, User](`{{ set }}`))
	t.Log(dbt.Parse[CreateUserBookParams, User](`{{ vals }}`))
	t.Log(dbt.Parse[any, UserBookAggregate](`{{ cols }}`))
	t.Log(dbt.Parse[any, *UserBookAggregate](`{{ cols }}`))
	t.Log(dbt.Parse[any, UserBookAggregatePointer](`{{ cols }}`))
	t.Log(dbt.Parse[any, *UserBookAggregatePointer](`{{ cols }}`))
}

type Book struct {
	ID    uuid.UUID
	Title string
}

type User struct {
	ID   uuid.UUID
	Name string
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

type UserBookAggregate struct {
	User `json:"u"`
	Book `json:"b"`
}

type UserBookAggregatePointer struct {
	*User `json:"u"`
	*Book `json:"b"`
}

type CreateUserParams struct {
	Name string
}

type UpdateUserParams struct {
	ID   uuid.UUID
	Name string
}

type Repository struct {
	db *sql.DB
}

var (
	createUser     = dbt.Must(dbt.New[CreateUserParams, *User]("insert into users {{ vals }} returning {{ cols }}"))
	updateUser     = dbt.Must(dbt.New[UpdateUserParams, *User]("update users set {{ set }} where id = @id returning {{ cols }}"))
	listUsers      = dbt.Must(dbt.New[any, User]("select {{ cols }} from users"))
	createBook     = dbt.Must(dbt.New[Book, *Book](`insert into books {{ vals "-" "id" }} returning {{ cols }}`))
	createUserBook = dbt.Must(dbt.New[CreateUserBookParams, *UserBook](`insert into user_books {{ vals }} returning {{ cols }}`))
	listUserBooks  = dbt.Must(dbt.New[any, UserBookAggregate](`select {{ cols }} from users u join books b on true`))
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
