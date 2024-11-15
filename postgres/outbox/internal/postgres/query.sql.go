// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.27.0
// source: query.sql

package postgres

import (
	"context"

	"github.com/lib/pq"
)

const count = `-- name: Count :one
SELECT COUNT(*)
FROM outbox
`

func (q *Queries) Count(ctx context.Context) (int64, error) {
	row := q.db.QueryRowContext(ctx, count)
	var count int64
	err := row.Scan(&count)
	return count, err
}

const create = `-- name: Create :exec
INSERT INTO outbox (
	aggregate_id,
	aggregate_type,
	type,
	payload
) VALUES (
	UNNEST($1::text[]),
	UNNEST($2::text[]),
	UNNEST($3::text[]),
	UNNEST($4::text[])::jsonb
)
`

type CreateParams struct {
	AggregateIds   []string
	AggregateTypes []string
	Types          []string
	Payloads       []string
}

func (q *Queries) Create(ctx context.Context, arg CreateParams) error {
	_, err := q.db.ExecContext(ctx, create,
		pq.Array(arg.AggregateIds),
		pq.Array(arg.AggregateTypes),
		pq.Array(arg.Types),
		pq.Array(arg.Payloads),
	)
	return err
}

const delete = `-- name: Delete :one
DELETE FROM outbox
WHERE id = (
	SELECT id
	FROM outbox
	ORDER BY id
	FOR UPDATE
	SKIP LOCKED
	LIMIT 1
)
RETURNING id, aggregate_id, aggregate_type, type, payload, created_at
`

func (q *Queries) Delete(ctx context.Context) (*Outbox, error) {
	row := q.db.QueryRowContext(ctx, delete)
	var i Outbox
	err := row.Scan(
		&i.ID,
		&i.AggregateID,
		&i.AggregateType,
		&i.Type,
		&i.Payload,
		&i.CreatedAt,
	)
	return &i, err
}
