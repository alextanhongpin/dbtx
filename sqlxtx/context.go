package sqlxtx

import (
	"context"
	"database/sql"
)

type contextKey string

var (
	// txCtxKey represents the key for the context containing the pointer of Atomic.
	txCtxKey  = contextKey("atm")
	roCtxKey  = contextKey("ro")
	isoCtxKey = contextKey("iso")
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

func Value(ctx context.Context) (DBTX, bool) {
	tx, ok := value(ctx)
	if !ok {
		return nil, false
	}

	return tx.underlying(), true
}

func IsTx(ctx context.Context) bool {
	_, ok := value(ctx)
	return ok
}

func value(ctx context.Context) (*Tx, bool) {
	tx, ok := ctx.Value(txCtxKey).(*Tx)
	return tx, ok
}

func withValue(ctx context.Context, tx *Tx) context.Context {
	return context.WithValue(ctx, txCtxKey, tx)
}
