package uow

import (
	"context"
	"errors"
)

var ErrContextNotFound = errors.New("uow: UnitOfWork not found in context")

type key[T any] string

func (k key[T]) WithValue(ctx context.Context, v T) context.Context {
	return context.WithValue(ctx, k, v)
}

func (k key[T]) Value(ctx context.Context) (t T, ok bool) {
	t, ok = ctx.Value(k).(T)

	return
}

func (k key[T]) MustValue(ctx context.Context) T {
	t, ok := ctx.Value(k).(T)
	if !ok {
		panic(ErrContextNotFound)
	}

	return t
}
