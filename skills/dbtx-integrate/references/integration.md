# Repository wiring

Use the application's existing *sql.DB, driver, configuration, migrations, and connection lifecycle.

```go
package storage

import (
    "context"
    "github.com/alextanhongpin/dbtx"
)

type Accounts struct { uow dbtx.UnitOfWork }

func NewAccounts(uow dbtx.UnitOfWork) *Accounts {
    return &Accounts{uow: uow}
}

func (r *Accounts) Debit(ctx context.Context, id, amount int64) error {
    _, err := r.uow.DBTx(ctx).ExecContext(ctx,
        "UPDATE accounts SET balance = balance - $1 WHERE id = $2", amount, id)
    return err
}

func (r *Accounts) Credit(ctx context.Context, id, amount int64) error {
    _, err := r.uow.DBTx(ctx).ExecContext(ctx,
        "UPDATE accounts SET balance = balance + $1 WHERE id = $2", amount, id)
    return err
}

func Transfer(ctx context.Context, uow dbtx.UnitOfWork, r *Accounts,
    from, to, amount int64) error {
    return uow.RunInTx(ctx, func(txCtx context.Context) error {
        if err := r.Debit(txCtx, from, amount); err != nil {
            return err
        }
        return r.Credit(txCtx, to, amount)
    })
}
```

At composition time, construct `uow := dbtx.New(db)` and pass it to NewAccounts and the transaction-owning operation. This example demonstrates context wiring; retain application checks for missing accounts, positive amounts, balances, and authorization.

Do not capture `uow.DBTx(context.Background())` in a repository field: that resolves the pool before the request transaction exists. Do not use the incoming request context instead of txCtx inside a callback. Continue closing query rows and checking rows.Err().

## sqlc

For database/sql-generated sqlc code, inspect its DBTX interface. When compatible, construct queries per repository operation:

```go
q := generated.New(r.uow.DBTx(ctx))
return q.CreateAccount(ctx, params)
```

Adapt names to the existing generated code. Do not edit generated files or type-assert wrapped DBTX to *sql.Tx to call WithTx. Native pgx-generated interfaces are different and are not automatically compatible with core dbtx.

## Options

With imports for context, database/sql, and dbtx:

```go
ctx = dbtx.WithTxOptions(ctx, &sql.TxOptions{
    Isolation: sql.LevelSerializable,
})
err := uow.RunInTx(ctx, func(txCtx context.Context) error {
    return perform(txCtx)
})
```

For a custom ID configured before use, call WithNamedTxOptions with that ID.

## Database tests

Prefer the consuming project's harness. The optional `github.com/alextanhongpin/dbtx/testing/dbtest` module exposes Init(Options...) func() error, DB(t), Tx(t), and New(t, Options...). Options.Hook is func(dsn string) error for migrations. These helpers require Docker; verify the selected version's API.

`dbtest.Tx(t)` returns *sql.DB backed by go-txdb, with rollback on cleanup. Use it for fixture isolation. Use normal pooled connections for actual commit visibility, independent transaction contention, advisory locks, and worker leases.

In TestMain, run migrations through the existing hook, preserve the test exit code, and clean up before os.Exit:

```go
func TestMain(m *testing.M) {
    stop := dbtest.Init(dbtest.Options{Hook: migrate})
    code := m.Run()
    if err := stop(); err != nil {
        fmt.Fprintln(os.Stderr, err)
        code = 1
    }
    os.Exit(code)
}
```

This snippet assumes testing, fmt, os, dbtest imports and the application's migrate hook. Defers do not run after os.Exit.

Assert that successful work persists all writes, a forced second-write error persists neither, and callback contexts join the same transaction. When introducing savepoints, assert the intended outer-write preservation. Run tests in each changed module; root go test ./... excludes nested modules.
