# dbtx

[![](https://godoc.org/github.com/alextanhongpin/dbtx?status.svg)](http://godoc.org/github.com/alextanhongpin/dbtx)

Unit of Work implementation with golang. Abstracts the complexity in setting transactions across different repository. Read more about it in this blog [Simplying Transactions in Golang with Unit of Work Pattern](https://alextanhongpin.github.io/blog/post/003-unit-of-work).

## Interface

```go
// atomic represents the database atomic operations in a transactions.
type atomic interface {
	IsTx() bool
	DBTx(ctx context.Context) DB
	DB() DB
	Tx(ctx context.Context) DB
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}
```


## Enforce Tx

If an operation must be absolutely carried out in a transaction, use `dbtx.Tx(ctx)` to ensure the context contains the `*sql.Tx`:

```go
func (r *userRepository) Create(ctx context.Context, name string) error {
	tx, ok := dbtx.Tx(ctx)
	if !ok {
		panic(dbtx.ErrNonTransaction)
	}

	_, err := tx.Exec(`insert into users (name) values ($1)`, name)
	return err
}
```

## Transaction-ready Repository

```go
package main

import (
	"context"
	"database/sql"

	"github.com/alextanhongpin/dbtx"
)

type atomic interface {
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}

type dbCtx interface {
	DBTx(ctx context.Context) dbtx.DBTX
}

type atomicDBCtx interface {
	atomic
	dbCtx
}

func main() {
	var db *sql.DB
	u := dbtx.New(db)
	userRepo := &userRepository{db: db}
	accountRepo := &accountRepository{dbtx: u}
	uc := &authUseCase{
		dbtx:        u,
		userRepo:    userRepo,
		accountRepo: accountRepo,
	}
	_ = uc
}

type userRepository struct {
	db *sql.DB
}

func (r *userRepository) DB(ctx context.Context) dbtx.DBTX {
	// Obtain either a *sql.DB/*sql.Tx from the context, or use the current
	// repository's *sql.DB.
	// A convenient method *dbtx.DB(ctx) is provided (see account repository
	// below).
	v, ok := dbtx.DBTx(ctx)
	if ok {
		return v
	}

	return r.db
}

func (r *userRepository) Create(ctx context.Context, name string) error {
	// This will obtain the Tx from the context, otherwise it will fallback to Db.
	db := r.DB(ctx)
	_, err := db.Exec(`insert into users (name) values ($1)`, name)
	return err
}

type accountRepository struct {
	dbtx atomicDBCtx
}

func (r *accountRepository) Create(ctx context.Context, name string) error {
	db := r.dbtx.DBTx(ctx)
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
		// You can pass in options to override *sql.TxOptions.
		ctx = dbtx.ReadOnly(ctx, false)
		ctx = dbtx.IsolationLevel(ctx, sql.LevelDefault)
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
)

type atomic interface {
	IsTx() bool
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}

func main() {
	repo := newMockOutboxRepo()
	atm := newMockAtomic()
	outbox := newOutboxAtomic(repo, atm)
	uc := &authUsecase{dbtx: outbox}
	fmt.Println(uc.Login(context.Background(), "john@appleseed.com"))
}

type authUsecase struct {
	dbtx atomic
}

func (uc *authUsecase) Login(ctx context.Context, email string) error {
	// NOTE: if passing dependencies through context is not to your liking, you
	// can also pass the outbox as the second argument. Example:
	//
	// return uc.dbtx.RunInTx(ctx, func(txCtx context.Context, outbox Outbox) error {
	return uc.dbtx.RunInTx(ctx, func(txCtx context.Context) error {
		// Retrieve the outbox.
		outbox, ok := outboxValue(txCtx)
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

type Event struct {
	Type string
	Data any
}

type contextKey string

var outboxContextKey contextKey = "outbox"

func withOutboxValue(ctx context.Context, o Outbox) context.Context {
	return context.WithValue(ctx, outboxContextKey, o)
}

func outboxValue(ctx context.Context) (Outbox, bool) {
	o, ok := ctx.Value(outboxContextKey).(Outbox)
	return o, ok
}

// OutboxAtomic is a customized UOW that allows persisting events on transaction commit.
type OutboxAtomic struct {
	atomic
	repo outboxRepo
}

func newOutboxAtomic(repo outboxRepo, atm atomic) *OutboxAtomic {
	return &OutboxAtomic{
		atomic: atm,
		repo:   repo,
	}
}

func (u *OutboxAtomic) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return u.atomic.RunInTx(ctx, func(txCtx context.Context) error {
		// A new outbox is created per-request.
		o := new(outbox)

		// The context containing the outbox is passed down.
		if err := fn(withOutboxValue(txCtx, o)); err != nil {
			return err
		}

		// Flush events
		return u.repo.Save(txCtx, o.events...)
	})
}

type mockAtomic struct{}

func newMockAtomic() *mockAtomic {
	return new(mockAtomic)
}

func (m *mockAtomic) IsTx() bool { return true }
func (m *mockAtomic) RunInTx(ctx context.Context, fn func(txContext context.Context) error) error {
	return fn(ctx)
}

type mockOutboxRepo struct{}

func newMockOutboxRepo() *mockOutboxRepo {
	return new(mockOutboxRepo)
}

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
```
