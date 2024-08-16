package buntx

import (
	"context"
)

type ctxKey string

// txCtxKey represents the key for the context containing the pointer of Atomic.
var txCtxKey = ctxKey("tx")

func Value(ctx context.Context) (DBTX, bool) {
	tx, ok := value(ctx)
	if !ok {
		return nil, false
	}

	return tx.underlying(), true
}

func value(ctx context.Context) (*Tx, bool) {
	tx, ok := ctx.Value(txCtxKey).(*Tx)
	return tx, ok
}

func withValue(ctx context.Context, tx *Tx) context.Context {
	return context.WithValue(ctx, txCtxKey, tx)
}
