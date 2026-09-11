-- name: Peek :one
with locked as (
    select id as locked_id
      from dbtx.outbox
     where (max_retry = 0 or retry_count < max_retry)
       and visible_at <= clock_timestamp()
  order by id
     limit 1 for update SKIP LOCKED
)
   update dbtx.outbox o
      set retry_count = retry_count + 1
    where o.id = (select locked_id from locked)
returning *;
