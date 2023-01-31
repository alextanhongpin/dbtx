# uow

[![](https://godoc.org/github.com/alextanhongpin/uow?status.svg)](http://godoc.org/github.com/alextanhongpin/uow)

Unit of Work implementation with golang. Abstracts the complexity in setting transactions across different repository.


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
