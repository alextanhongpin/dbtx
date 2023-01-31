package uow

import (
	"context"
	"errors"
)

var ErrContextNotFound = errors.New("uow: UnitOfWork not found in context")

type contextKey string

// uowContextKey represents the key for the context containing the pointer of UnitOfWork.
var uowContextKey = contextKey("uow")

func Value(ctx context.Context) (*UnitOfWork, bool) {
	uow, ok := ctx.Value(uowContextKey).(*UnitOfWork)
	return uow, ok
}

func MustValue(ctx context.Context) *UnitOfWork {
	uow, ok := Value(ctx)
	if !ok {
		panic(ErrContextNotFound)
	}

	return uow
}

func WithValue(ctx context.Context, uow *UnitOfWork) context.Context {
	return context.WithValue(ctx, uowContextKey, uow)
}
