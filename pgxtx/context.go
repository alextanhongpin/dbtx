package pgxtx

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type ctxKey string

var (
	txCtxKey  = ctxKey("tx")
	optCtxKey = ctxKey("opt")
)

func WithTxOptions(ctx context.Context, opts pgx.TxOptions) context.Context {
	return context.WithValue(ctx, optCtxKey, opts)
}

func TxOptions(ctx context.Context) pgx.TxOptions {
	opts, _ := ctx.Value(optCtxKey).(pgx.TxOptions)
	return opts
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
