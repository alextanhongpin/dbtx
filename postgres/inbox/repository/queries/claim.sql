-- name: Claim :many
with next as (
    select id
      from dbtx.inbox
     where status in ('pending', 'processing')  -- 'processing' = expired lease
       and available_at <= now()
       and attempts < max_attempts
  order by available_at, id
     limit $1
       for update SKIP LOCKED
)
   update dbtx.inbox i
      set status = 'processing'::dbtx.inbox_status,
          locked_by = sqlc.arg(locked_by)::text,
          available_at = now()
        + interval '1 second' * sqlc.arg(lease_seconds)::float8,
          attempts = i.attempts + 1,
          updated_at = now()
     from next
    where i.id = next.id
returning i.*;
