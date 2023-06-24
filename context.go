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
	depthContextKey          = contextKey("depth_ctx")
	loggerContextKey         = contextKey("log_ctx")
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

func IncDepth(ctx context.Context) context.Context {
	n := Depth(ctx)
	return context.WithValue(ctx, depthContextKey, n+1)
}

func Depth(ctx context.Context) int {
	depth, _ := ctx.Value(depthContextKey).(int)
	return depth
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

// Tx returns the DBTX from the context, only if the underlying type is a
// *sql.Tx.
// We still return the DBTX interface here, to avoid client from manually
// calling the tx.Commit.
func Tx(ctx context.Context) (DBTX, bool) {
	atmCtx, ok := Value(ctx)
	if ok && atmCtx.IsTx() {
		return atmCtx.underlying(ctx), true
	}

	return nil, false
}

// DBTx returns the DBTX from the context, which can be either *sql.DB or
// *sql.Tx.
func DBTx(ctx context.Context) (DBTX, bool) {
	atmCtx, ok := Value(ctx)
	if ok {
		return atmCtx.underlying(ctx), true
	}

	return nil, false
}

func WithLoggerValue(ctx context.Context, l logger) context.Context {
	return context.WithValue(ctx, loggerContextKey, l)
}

func LoggerValue(ctx context.Context) (logger, bool) {
	l, ok := ctx.Value(loggerContextKey).(logger)
	return l, ok
}
