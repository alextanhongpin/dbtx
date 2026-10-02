-- name: Create :one
insert into dbtx.outbox(aggregate_id, aggregate_type, event_type, payload,
                        max_attempts, available_at)
     values (sqlc.arg(aggregate_id), sqlc.arg(aggregate_type),
             sqlc.arg(event_type), sqlc.arg(payload),
             coalesce(sqlc.narg(max_attempts)::int, 10),
             coalesce(sqlc.narg(available_at)::timestamptz, now()))
  returning id;
