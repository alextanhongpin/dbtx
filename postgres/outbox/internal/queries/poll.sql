-- name: Poll :many
with next as (
    select id
      from dbtx.outbox
     where status in ('pending', 'processing')
       and available_at <= now()
       and attempts < max_attempts
  order by available_at, id
     limit sqlc.arg(batch_size)
       for update skip locked
)
   update dbtx.outbox o
      set status = 'processing',
          locked_by = sqlc.arg(locked_by),
          updated_at = now(),
          available_at = now()
        + interval '1 second' * sqlc.arg(lease_seconds)::float8,
          attempts = o.attempts + 1
     from next
    where o.id = next.id
returning o.*;
