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
with retry_args as (
    select sqlc.arg('limit')::int as "limit",
           sqlc.arg(locked_by)::text as locked_by,
           sqlc.arg(lease_seconds)::float8 as lease_seconds
), next as (
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
     limit (select "limit" from retry_args)
       for update SKIP LOCKED
)
select claimed.*
  from next
 cross join lateral dbtx.inbox_retry(next.id, 'claim', (select locked_by from retry_args),
     jsonb_build_object('lease_seconds', (select lease_seconds from retry_args))) claimed;
