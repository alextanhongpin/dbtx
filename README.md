# dbtx

[![](https://godoc.org/github.com/alextanhongpin/dbtx?status.svg)](http://godoc.org/github.com/alextanhongpin/dbtx)

Unit of Work implementation with golang. Abstracts the complexity in setting transactions across different repository. Read more about it in this blog [Simplying Transactions in Golang with Unit of Work Pattern](https://alextanhongpin.github.io/blog/post/003-unit-of-work).

## Interface


> **Note**
> Implement only the methods you need, nothing more.

```go
// atomic represents the database atomic operations in a transactions.
type atomic interface {
	IsTx() bool
	DBTx(ctx context.Context) DBTX
	DB(ctx context.Context) DBTX
	Tx(ctx context.Context) DBTX

	// In your application service layer (aka usecase layer), this is probably
	// the only interface you need to implement.
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) (err error)
}
```

## Why DBTX


### Option 1: Using WithTx

There are quite a number of examples using `WithTx` which will return a new pointer to the repository with `*sql.Tx` as the underlying connection:

```go
func (r *UserRepository) WithTx(tx *sql.Tx) *UserRepository {
	return &UserRepository{
		db: tx,
	}
}
```

However, this becomes an apparent issue in the application service layer, as it is _impossible_ to define the interface for the repository:

```go
type userRepository interface {
	// WARN: WithTx now returns the concrete implementation, not interface!
	// This means all its methods can be accessed.
	WithTx(tx *sql.Tx) *repository.UserRepository
	Create(ctx context.Context, name string) (*domain.User, error)
}

type UserUsecase struct {
	// WARN: Leaking database driver implementation here.
	db *sql.DB
	repo userRepository
}

func (uc *UserUsecase) Create(ctx context.Context, name string) (*domain.User, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var u *domain.User

	// Does not comply to `userRepository`, so it can call all the methods from *UserRepository
	repo := uc.repo.WithTx(tx)
	_ = repo.SomeUnallowedMethod()

	return u, tx.Commit()
}
```

There are several issues:
1. The details of the database driver (`*sql.DB` and `*sql.Tx`) is leaked
2. The repository with transaction driver no longer fulfils the interface
3. Passing down `*sql.Tx` is dangerous, since there is no way of controlling the time the transaction is committed. By right, the commit method should only be called by the parent that initiates the transaction. :shrug:
4. Nesting of database transaction will cause issue 3 to be more apparent. For example, you may want to chain multiple application service/repository to run in a single transaction. In order to ensure that only the root parent can commit the transaction, you first need to know if an underlying transaction is passed down. `dbtx` handles this gracefully by reusing the transaction that is passed down through context.
5. Forgot to `Commit/Rollback` a transaction in deeply nested layers.


Also, the issue number 2 **cannot** be solved this way :smiley: :
```go
type userRepository interface {
	// Replacing *repository.UserRepository with userRepository does not work.
	WithTx(tx *sql.Tx) userRepository
	Create(ctx context.Context, name string) (*domain.User, error)
}
```

### Option 2: Passing `*sql.DB` and `*sql.Tx` explicitly


The only way to solve the issue in `Option 1` is to pass the underlying database connection directly through the function or method.

```go
func (r *UserRepository) Create(ctx context.Context, tx *sql.Tx, name string) (*domain.User, error) {
	// Do something ...
}
```

However, it introduces a new problem, because sometimes we don't want to run a repository method in transaction. A workaround is to use the default `*sql.DB` if the `*sql.Tx` is `nil`:

```go
type UserRepository struct {
	db *sql.DB
}


func (r *UserRepository) Create(ctx context.Context, tx *sql.Tx, name string) (*domain.User, error) {
	if tx == nil {
		// Use r.db
	} else {
		// Use tx
	}
}
```

However, this complicates the function signature, and passing `nil` is ugly:

```go
// user_usecase.go
userRepository.Create(ctx, nil, "John Appleseed")
```

Also, it still leaks the details of the database driver in the usecase layer.


### Option 3: Pass the database driver using context.Context

This is the method implemented by `dbtx`. There are a lot of articles that claims that passing dependencies through context is not idiomatic golang. There is nothing to refute those claims.

Use `dbtx` only if you are comfortable with the idea.

With `dbtx`, the implementation details of the database driver are not leaked in the _application service_ (aka _usecase_) layer.

```go
// user_usecase.go
type atomic interface {
	RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error
}

type userRepository interface {
	Create(ctx context.Context, name string) (*domain.User, error)
}

type UserUsecase struct {
	atomic
	repo userRepository
}

func (uc *UserUsecase) Create(ctx context.Context, name string) (*domain.User, error) {
	var u *domain.User
	err := uc.RunInTx(ctx, func(txCtx context.Context) error {
		err := uc.repo.Create(ctx, name)
		if err != nil {
			return err
		}

		// Chain several repository methods here that requires transactions ...

		return nil
	})

	return u, err
}
```

The _repository_ require minor modifications in order to support between `*sql.DB` or `*sql.Tx`:

```go
// user_repository.go


type atomicDBTX interface {
	DBTx(ctx context.Context) dbtx.DBTX
}

type UserRepository struct {
	atomicDBTX
}

func (r *UserRepository) Create(ctx context.Context, name string) (*domain.User, error) {
	// Returns `*sql.DB` if no transaction context is found.
	// Returns `*sql.Tx` if transaction context is found.
	dbtx := r.DBTx(ctx)

	// NOTE: If you want to ensure that this method to only use `*sql.Tx`, then:
	// tx := r.Tx(ctx)
	//
	// On the other hand, if you want this to only use `*sql.Tx`, then:
	// db := r.DB(ctx)
	//
	// Both methods above will panic if the underlying type does not match.
	// If you want to handle them yourself, use:
	//
	// dbTx, ok := dbtx.DBTx(ctx)
	// tx, ok := dbtx.Tx(ctx)

	// NOTE: Sample code
	err := dbtx.QueryRowContext(ctx, `insert into users ...`, name).Scan(&u)
	return err
}
```



## Nesting of Transaction

When explicitly passing `*sql.Tx/*sql.DB` to the repository, we can still chain different repositories together. However, what happens when you need to chain multiple usecases? Now all the usecase has to accept the database driver as the method arguments.

```go
type AccountUsecase struct {
	db *sql.DB
	userUsecase userUsecase
	adminUsecase adminUsecase
}

func (uc *AccountUsecase) CreateUserAdmin(ctx context.Context, email string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := uc.userUsecase.CreateUser(ctx, tx, email); err != nil {
		return err
	}

	if err := uc.adminUsecase.CreateAdmin(ctx, tx, email); err != nil {
		return err
	}

	return tx.Commit()
}
```

Except that it might not work, because in the `adminUsecase` and/or `userUsecase`, there is already a method that calls `tx.Commit`. This could be because originally we already have a usecase where we want to create the user within a transaction:


```go
type UserUsecase struct {
	db *sql.DB
}

func (uc *UserUsecase) CreateUser(ctx, tx *sql.Tx, email string) error {
	var err error
	if tx == nil {
		tx, err = uc.db.BeginTx()
		if err != nil {
			return err
		}
	}
	defer tx.Rollback()

	if err := tx.Exec(`...`, email); err != nil {
		return err
	}

	// WARN: This commit will end the AccountUsecase flow, and no Account will be created.
	return tx.Commit()
}
```

To make it reusable, we need to create multiple methods:

```go
type UserUsecase struct {
	db *sql.DB
}

// CreateuserTx is a method that delegates the Commit/Rollback to the parent.
func (uc *UserUsecase) CreateUserTx(ctx, tx *sql.Tx, email string) error {
	if err := tx.Exec(`...`, email); err != nil {
		return err
	}

	return nil
}

func (uc *UserUsecase) CreateUser(ctx, email string) error {
	tx, err := uc.db.BeginTx()
	if err != nil {
		return err
	}

	if err := uc.CreateUserTx(ctx, tx, email); err != nil {
		return err
	}


	return tx.Commit()
}
```


With `dbtx`, you don't need to create multiple methods, since it automatically detects if the context contains the underlying transaction and reuses it. It guards against nested transaction.


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
	// You can pass in options to override *sql.TxOptions.
	ctx = dbtx.ReadOnly(ctx, false)
	ctx = dbtx.IsolationLevel(ctx, sql.LevelDefault)

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
