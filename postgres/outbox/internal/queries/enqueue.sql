-- name: Enqueue :one
insert into dbtx.outbox(aggregate_id, aggregate_type, type, payload,
                        max_retry, visible_at)
     values ($1, $2, $3, $4, $5, $6)
  returning id;
