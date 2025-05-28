package dbtx

import (
	"context"
	"database/sql"
)

type ctxKey[T any] string

var (
	txCtxKey     = ctxKey[*Tx]("tx")
	txOptsCtxKey = ctxKey[*sql.TxOptions]("tx_opts")
)

func (key ctxKey[T]) Value(ctx context.Context) (T, bool) {
	v, ok := ctx.Value(key).(T)
	return v, ok
}

func (key ctxKey[T]) WithValue(ctx context.Context, v T) context.Context {
	return context.WithValue(ctx, key, v)
}

func WithTxOptions(ctx context.Context, opts *sql.TxOptions) context.Context {
	return txOptsCtxKey.WithValue(ctx, opts)
}

func TxOptions(ctx context.Context) *sql.TxOptions {
	v, _ := txOptsCtxKey.Value(ctx)
	return v
}

func IsTx(ctx context.Context) bool {
	_, ok := txCtxKey.Value(ctx)
	return ok
}

func Value(ctx context.Context) (DBTX, bool) {
	tx, ok := txCtxKey.Value(ctx)
	if !ok {
		return nil, false
	}

	return tx.Tx(), true
}
