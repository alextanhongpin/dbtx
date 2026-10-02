package dbt_test

import (
	"fmt"
	"time"

	"github.com/alextanhongpin/dbtx/postgres/dbt"
)

func ExampleNew_insert() {
	type User struct {
		ID        int
		Name      string
		Email     string
		CreatedAt time.Time
		UpdatedAt time.Time
	}
	type CreateUserParams struct {
		Name  string
		Email string
	}
	q := dbt.Must(dbt.New[CreateUserParams, User](`INSERT INTO users {{ vals }} RETURNING {{ cols }}`))

	fmt.Println(q.String())
	fmt.Println(q.Args(CreateUserParams{
		Name:  "john",
		Email: "john@appleseed.com",
	}))

	// Output:
	// INSERT INTO users (name, email) VALUES ($1, $2) RETURNING id, name, email, created_at, updated_at
	// [john john@appleseed.com] <nil>
}

func ExampleNew_select() {
	type User struct {
		ID        int
		Name      string
		Email     string
		CreatedAt time.Time
		UpdatedAt time.Time
	}
	type FilterUserParams struct {
		Name  string
		Email string
		Age   int
	}
	q := dbt.Must(dbt.New[FilterUserParams, User](`SELECT {{ cols }}
FROM users u
WHERE name = @name AND email = @email AND age = @age
LIMIT 3`))

	fmt.Println(q.String())
	fmt.Println(q.Args(FilterUserParams{
		Name:  "john",
		Email: "john.appleseed@mail.com",
		Age:   20,
	}))

	// Output:
	// SELECT id, name, email, created_at, updated_at FROM users u WHERE name = $1 AND email = $2 AND age = $3 LIMIT 3
	// [john john.appleseed@mail.com 20] <nil>
}

func ExampleNew_update() {
	type User struct {
		ID        int
		Name      string
		Email     string
		CreatedAt time.Time
		UpdatedAt time.Time
	}
	type UpdateUserParams struct {
		Name  string
		Email string
		Age   int
	}
	q := dbt.Must(dbt.New[UpdateUserParams, User](`UPDATE users
SET {{ set "-" "email" }}
WHERE email = @email`))

	fmt.Println(q.String())
	fmt.Println(q.Args(UpdateUserParams{
		Name:  "john",
		Email: "john.appleseed@mail.com",
		Age:   32,
	}))

	// Output:
	// UPDATE users SET name = $1, age = $2 WHERE email = $3
	// [john 32 john.appleseed@mail.com] <nil>
}

func ExampleNew_aggregate() {
	type User struct {
		ID        int
		Name      string
		Email     string
		CreatedAt time.Time
		UpdatedAt time.Time
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
	type UserBook struct {
		ID        int
		UserID    int
		BookID    int
		Status    string
		CreatedAt time.Time
		UpdatedAt time.Time
	}

	type UserBookAggregate struct {
		UserBook
		User `json:"u"`
		Book `json:"b"`
	}
	q := dbt.Must(dbt.New[any, UserBookAggregate](`SELECT {{ cols }}
FROM users u
JOIN books b ON (u.id = b.user_id)`))
	fmt.Println(q.String())
	fmt.Println(q.Args(nil))

	// Output:
	// SELECT user_book.id AS user_book_id, user_book.user_id AS user_book_user_id, user_book.book_id AS user_book_book_id, user_book.status AS user_book_status, user_book.created_at AS user_book_created_at, user_book.updated_at AS user_book_updated_at, u.id AS u_id, u.name AS u_name, u.email AS u_email, u.created_at AS u_created_at, u.updated_at AS u_updated_at, b.id AS b_id, b.title AS b_title, b.author AS b_author, b.published_at AS b_published_at, b.isbn AS b_isbn, b.created_at AS b_created_at, b.updated_at AS b_updated_at FROM users u JOIN books b ON u.id = b.user_id
	// [] <nil>
}
