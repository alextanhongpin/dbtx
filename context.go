package dbtx

import (
	"context"
	"database/sql"
)

type ctxKey[T any] string

var (
	txCtxKey  = ctxKey[*Tx]("tx")
	roCtxKey  = ctxKey[bool]("ro")
	isoCtxKey = ctxKey[sql.IsolationLevel]("iso")
)

func (key ctxKey[T]) Value(ctx context.Context) (T, bool) {
	v, ok := ctx.Value(key).(T)
	return v, ok
}

func (key ctxKey[T]) WithValue(ctx context.Context, v T) context.Context {
	return context.WithValue(ctx, key, v)
}

func ReadOnly(ctx context.Context, readOnly bool) context.Context {
	return roCtxKey.WithValue(ctx, readOnly)
}

func IsolationLevel(ctx context.Context, isoLevel sql.IsolationLevel) context.Context {
	return isoCtxKey.WithValue(ctx, isoLevel)
}

func TxOptions(ctx context.Context) *sql.TxOptions {
	readOnly, _ := roCtxKey.Value(ctx)
	isolation, _ := isoCtxKey.Value(ctx)
	return &sql.TxOptions{
		ReadOnly:  readOnly,
		Isolation: isolation,
	}
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
