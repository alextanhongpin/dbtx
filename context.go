package dbtx

import (
	"context"
	"errors"
)

var ErrContextNotFound = errors.New("dbtx: Atomic not found in context")

type contextKey string

// atomicContextkey represents the key for the context containing the pointer of Atomic.
var atomicContextkey = contextKey("atm_ctx")

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
