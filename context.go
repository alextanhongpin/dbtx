package dbtx

import (
	"context"
	"database/sql"
)

const ID = "dbtx"

type ctxKey string

type txOptsCtxKey string

func WithTxOptions(ctx context.Context, opts *sql.TxOptions) context.Context {
	return WithNamedTxOptions(ctx, ID, opts)
}

func TxOptions(ctx context.Context) *sql.TxOptions {
	return NamedTxOptions(ctx, ID)
}

func IsTx(ctx context.Context) bool {
	return IsNamedTx(ctx, ID)
}

func Value(ctx context.Context) (DBTX, bool) {
	return NamedValue(ctx, ID)
}

func WithValue(ctx context.Context, tx *Tx) context.Context {
	return WithNamedValue(ctx, ID, tx)
}

func WithNamedTxOptions(ctx context.Context, id string, opts *sql.TxOptions) context.Context {
	return context.WithValue(ctx, txOptsCtxKey(id), opts)
}

func NamedTxOptions(ctx context.Context, id string) *sql.TxOptions {
	v, _ := ctx.Value(txOptsCtxKey(id)).(*sql.TxOptions)
	return v
}

func IsNamedTx(ctx context.Context, id string) bool {
	_, ok := ctx.Value(ctxKey(id)).(*Tx)
	return ok
}

func WithNamedValue(ctx context.Context, id string, tx *Tx) context.Context {
	return context.WithValue(ctx, ctxKey(id), tx)
}

func NamedValue(ctx context.Context, id string) (DBTX, bool) {
	tx, ok := ctx.Value(ctxKey(id)).(*Tx)
	if !ok {
		return nil, false
	}

	return tx.Tx(), true
}
