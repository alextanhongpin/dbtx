-- name: Create :one
insert into dbtx.outbox(aggregate_id, aggregate_type, type, payload,
                        max_retry, visible_at)
     values (sqlc.arg(aggregate_id), sqlc.arg(aggregate_type), sqlc.arg(type),
             sqlc.arg(payload), sqlc.arg(max_retry),
             coalesce(sqlc.narg(visible_at)::timestamptz, clock_timestamp()))
  returning *;
