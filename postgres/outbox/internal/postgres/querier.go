// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.27.0

package postgres

import (
	"context"
)

type Querier interface {
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, arg CreateParams) error
	Delete(ctx context.Context) (*Outbox, error)
}

var _ Querier = (*Queries)(nil)
