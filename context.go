package dbtx

import (
	"context"
	"database/sql"
	"errors"
)

var ErrContextNotFound = errors.New("dbtx: Atomic not found in context")

type contextKey string

var (
	// atomicContextkey represents the key for the context containing the pointer of Atomic.
	atomicContextkey         = contextKey("atm_ctx")
	readOnlyContextKey       = contextKey("ro_ctx")
	isolationLevelContextKey = contextKey("iso_ctx")
)

func Value(ctx context.Context) (*Atomic, bool) {
	atm, ok := ctx.Value(atomicContextkey).(*Atomic)
	return atm, ok
}

func MustValue(ctx context.Context) *Atomic {
	atm, ok := Value(ctx)
	if !ok {
		panic(ErrContextNotFound)
	}

	return atm
}

func WithValue(ctx context.Context, atm *Atomic) context.Context {
	return context.WithValue(ctx, atomicContextkey, atm)
}

func ReadOnly(ctx context.Context, readOnly bool) context.Context {
	return context.WithValue(ctx, readOnlyContextKey, readOnly)
}

func IsolationLevel(ctx context.Context, isoLevel sql.IsolationLevel) context.Context {
	return context.WithValue(ctx, isolationLevelContextKey, isoLevel)
}

func TxOptions(ctx context.Context) *sql.TxOptions {
	readOnly, _ := ctx.Value(readOnlyContextKey).(bool)
	isolation, _ := ctx.Value(isolationLevelContextKey).(sql.IsolationLevel)
	return &sql.TxOptions{
		ReadOnly:  readOnly,
		Isolation: isolation,
	}
}

func IsTx(ctx context.Context) bool {
	a, ok := Value(ctx)
	return ok && a.IsTx()
}

func DBTX(ctx context.Context) (DB, bool) {
	atmCtx, ok := Value(ctx)
	if ok {
		return atmCtx.underlying(), true
	}

	return nil, false
}
