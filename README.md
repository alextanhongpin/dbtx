# dbtx

[![](https://godoc.org/github.com/alextanhongpin/dbtx?status.svg)](http://godoc.org/github.com/alextanhongpin/dbtx)

Unit of Work implementation with golang. Abstracts the complexity in setting transactions across different repository. Read more about it in this blog [Simplying Transactions in Golang with Unit of Work Pattern](https://alextanhongpin.github.io/blog/post/003-unit-of-work).


## Transaction-ready Repository

```go
package main

import (
	"context"
	"database/sql"

	"github.com/alextanhongpin/dbtx"
)

type atomic interface {
	IsTx() bool
	DB(ctx context.Context) dbtx.DB
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error, opts ...Option) (err error)
}

func main() {
	var db *sql.DB
	u := dbtx.New(db)
	userRepo := &userRepository{dbtx: u}
	accountRepo := &accountRepository{dbtx: u}
	uc := &authUseCase{
		dbtx:         u,
		userRepo:    userRepo,
		accountRepo: accountRepo,
	}
	_ = uc
}

type userRepository struct {
	dbtx atomic
}

func (r *userRepository) Create(ctx context.Context, name string) error {
	// This will obtain the Tx from the context, otherwise it will fallback to Db.
	db := r.dbtx.DB(ctx)
	_, err := db.Exec(`insert into users (name) values ($1)`, name)
	return err
}

type accountRepository struct {
	dbtx atomic
}

func (r *accountRepository) Create(ctx context.Context, name string) error {
	db := r.dbtx.DB(ctx)
	_, err := db.Exec(`insert into accounts (name) values ($1)`, name)
	return err
}

type authUseCase struct {
	dbtx        atomic
	userRepo    *userRepository
	accountRepo *accountRepository
}

func (uc *authUseCase) Create(ctx context.Context, name string) error {
	return uc.dbtx.RunInTx(ctx, func(ctx context.Context) error {
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

	"github.com/alextanhongpin/dbtx"
)

func main() {
	u := &OutboxAtomic{repo: &mockOutboxRepo{}, Atomic: &mockAtomic{}}
	uc := &authUsecase{dbtx: u}
	fmt.Println(uc.Login(context.Background(), "john@appleseed.com"))
}

type mockAtomic struct{}

func (m *mockAtomic) IsTx() bool                    { return true }
func (m *mockAtomic) DB(ctx context.Context) dbtx.DB { return nil }
func (m *mockAtomic) RunInTx(ctx context.Context, fn func(txContext context.Context) error, opts ...dbtx.Option) error {
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

var _ atomic = (*OutboxAtomic)(nil)

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

// OutboxAtomic is a customized UOW that allows persisting events on transaction commit.
type OutboxAtomic struct {
	atomic
	repo outboxRepo
}

func (u *OutboxAtomic) RunInTx(ctx context.Context, fn func(ctx context.Context) error, opts ...dbtx.Option) error {
	return u.Atomic.RunInTx(ctx, func(txCtx context.Context) error {
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
	dbtx *OutboxAtomic
}

func (uc *authUsecase) Login(ctx context.Context, email string) error {
	// NOTE: if passing dependencies through context is not to your liking, you
	// can also pass the outbox as the second argument. Example:
	//
	// return uc.dbtx.RunInTx(ctx, func(txCtx context.Context, outbox Outbox) error {
	return uc.dbtx.RunInTx(ctx, func(txCtx context.Context) error {
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
