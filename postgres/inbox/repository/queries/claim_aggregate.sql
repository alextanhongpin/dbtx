-- name: ClaimByAggregateID :many
-- Claims the oldest unfinished message of each aggregate, so messages of the
-- same aggregate are handled one at a time, in order. Messages without an
-- aggregate are not ordered. A dead message no longer blocks the messages
-- after it.
--
-- A message is also skipped while another message of its aggregate holds a
-- live lease, whatever their order: an earlier message can become pending
-- after a later one was claimed, when it is requeued or when the transaction
-- that enqueued it commits late.
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
       and not exists
           (
             select 1
               from dbtx.inbox busy
              where busy.aggregate_id = i.aggregate_id
                and busy.id <> i.id
                and busy.status = 'processing'
                and busy.available_at > now()
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
