-- name: Enqueue :one
insert into dbtx.outbox(aggregate_id, aggregate_type, type, payload)
     values ($1, $2, $3, $4)
  returning id;
