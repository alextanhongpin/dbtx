package dbtx

import (
	"context"
	"database/sql"
)

type ctxKey string

var (
	txCtxKey  = ctxKey("tx")
	roCtxKey  = ctxKey("ro")
	isoCtxKey = ctxKey("iso")
)

func ReadOnly(ctx context.Context, readOnly bool) context.Context {
	return context.WithValue(ctx, roCtxKey, readOnly)
}

func IsolationLevel(ctx context.Context, isoLevel sql.IsolationLevel) context.Context {
	return context.WithValue(ctx, isoCtxKey, isoLevel)
}

func TxOptions(ctx context.Context) *sql.TxOptions {
	readOnly, _ := ctx.Value(roCtxKey).(bool)
	isolation, _ := ctx.Value(isoCtxKey).(sql.IsolationLevel)
	return &sql.TxOptions{
		ReadOnly:  readOnly,
		Isolation: isolation,
	}
}

func IsTx(ctx context.Context) bool {
	_, ok := value(ctx)
	return ok
}

func Value(ctx context.Context) (DBTX, bool) {
	tx, ok := value(ctx)
	if !ok {
		return nil, false
	}

	return tx.Tx(), true
}

func value(ctx context.Context) (*Tx, bool) {
	tx, ok := ctx.Value(txCtxKey).(*Tx)
	return tx, ok
}

func withValue(ctx context.Context, t *Tx) context.Context {
	return context.WithValue(ctx, txCtxKey, t)
}
