# uow

[![](https://godoc.org/github.com/alextanhongpin/uow?status.svg)](http://godoc.org/github.com/alextanhongpin/uow)

Unit of Work implementation with golang. Abstracts the complexity in setting transactions across different repository. Read more about it in this blog [Simplying Transactions in Golang with Unit of Work Pattern](https://alextanhongpin.github.io/blog/post/003-unit-of-work).


## Transaction-ready Repository

```go
package main

import (
	"context"
	"database/sql"

	"github.com/alextanhongpin/uow"
)

func main() {
	var db *sql.DB
	u := uow.New(db)
	userRepo := &userRepository{uow: u}
	accountRepo := &accountRepository{uow: u}
	uc := &authUseCase{
		uow:         u,
		userRepo:    userRepo,
		accountRepo: accountRepo,
	}
	_ = uc
}

type userRepository struct {
	uow uow.UOW
}

func (r *userRepository) Create(ctx context.Context, name string) error {
	// This will obtain the Tx from the context, otherwise it will fallback to Db.
	db := r.uow.DB(ctx)
	_, err := db.Exec(`insert into users (name) values ($1)`, name)
	return err
}

type accountRepository struct {
	uow uow.UOW
}

func (r *accountRepository) Create(ctx context.Context, name string) error {
	db := r.uow.DB(ctx)
	_, err := db.Exec(`insert into accounts (name) values ($1)`, name)
	return err
}

type authUseCase struct {
	uow         uow.UOW
	userRepo    *userRepository
	accountRepo *accountRepository
}

func (uc *authUseCase) Create(ctx context.Context, name string) error {
	return uc.uow.RunInTx(ctx, func(ctx context.Context) error {
		err := uc.userRepo.Create(ctx, name)
		if err != nil {
			return err
		}

		return uc.accountRepo.Create(ctx, name)
	})
}
```


## Outbox Pattern

One common usecase when wrapping operations in a transaction is to implement Outbox pattern.

For simple usecases, we can just persist the events in-memory and flush them when the transaction commits. For a more scalable (?) solution, consider using Debezium.

```go
package main

import (
	"context"
	"fmt"

	"github.com/alextanhongpin/uow"
)

func main() {
	u := &OutboxUow{repo: &mockOutboxRepo{}, UOW: &mockUow{}}
	uc := &authUsecase{uow: u}
	fmt.Println(uc.Login(context.Background(), "john@appleseed.com"))
}

type mockUow struct{}

func (m *mockUow) IsTx() bool                    { return true }
func (m *mockUow) DB(ctx context.Context) uow.DB { return nil }
func (m *mockUow) RunInTx(ctx context.Context, fn func(txContext context.Context) error, opts ...uow.Option) error {
	return fn(ctx)
}

type mockOutboxRepo struct{}

func (r *mockOutboxRepo) Save(ctx context.Context, events ...Event) error {
	if len(events) == 0 {
		return nil
	}

	fmt.Println("[mockOutboxRepo] Save", events)
	return nil
}

var _ uow.UOW = (*OutboxUow)(nil)

type outboxRepo interface {
	Save(ctx context.Context, events ...Event) error
}

type Outbox interface {
	Fire(events ...Event)
}

type outbox struct {
	events []Event
}

func (o *outbox) Fire(events ...Event) {
	fmt.Println("fire", events)
	o.events = append(o.events, events...)
}

type Event struct {
	Type string
	Data any
}

type contextKey string

var outboxContextKey contextKey = "outbox"

func withValue(ctx context.Context, o Outbox) context.Context {
	return context.WithValue(ctx, outboxContextKey, o)
}

func value(ctx context.Context) (Outbox, bool) {
	o, ok := ctx.Value(outboxContextKey).(Outbox)
	return o, ok
}

// OutboxUow is a customized UOW that allows persisting events on transaction commit.
type OutboxUow struct {
	uow.UOW
	repo outboxRepo
}

func (u *OutboxUow) RunInTx(ctx context.Context, fn func(ctx context.Context) error, opts ...uow.Option) error {
	return u.UOW.RunInTx(ctx, func(txCtx context.Context) error {
		// A new outbox is created per-request.
		o := new(outbox)

		// The context containing the outbox is passed down.
		if err := fn(withValue(txCtx, o)); err != nil {
			return err
		}

		// Flush events
		return u.repo.Save(txCtx, o.events...)
	})
}

type authUsecase struct {
	uow *OutboxUow
}

func (uc *authUsecase) Login(ctx context.Context, email string) error {
	// NOTE: if passing dependencies through context is not to your liking, you
	// can also pass the outbox as the second argument. Example:
	//
	// return uc.uow.RunInTx(ctx, func(txCtx context.Context, outbox Outbox) error {
	return uc.uow.RunInTx(ctx, func(txCtx context.Context) error {
		// Retrieve the outbox.
		outbox, ok := value(txCtx)
		if ok {
			// Fire events. These events will be saved in the same transaction.
			outbox.Fire(
				Event{Type: "user_created", Data: map[string]any{"email": email}},
				Event{Type: "logged_in", Data: map[string]any{"email": email}},
			)
		}

		return nil
	})
}
```
