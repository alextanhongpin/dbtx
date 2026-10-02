-- name: ClaimByAggregateID :many
-- Claims the oldest unfinished message of each aggregate, so messages of the
-- same aggregate are handled one at a time, in order. Messages without an
-- aggregate are not ordered. A dead message no longer blocks the messages
-- after it.
with next as (
    select i.id
      from dbtx.inbox i
     where i.status in ('pending', 'processing')
       and i.available_at <= now()
       and i.attempts < i.max_attempts
       and not exists
           (
             select 1
               from dbtx.inbox earlier
              where earlier.aggregate_id = i.aggregate_id
                and earlier.id < i.id
                and earlier.status in ('pending', 'processing')
           )
  order by i.available_at, i.id
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
