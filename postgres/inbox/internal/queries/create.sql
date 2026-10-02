-- name: Create :one
insert into dbtx.inbox(source, message_id, message_type, aggregate_id,
                       payload, available_at, max_attempts)
     values ($1, $2, $3, nullif(sqlc.arg(aggregate_id)::text, ''), $4,
             coalesce(sqlc.narg(available_at)::timestamptz, now()),
             coalesce(sqlc.narg(max_attempts)::int, 10))
on conflict (source, message_id) do nothing
  returning id;  -- no row returned = duplicate, just ack it
