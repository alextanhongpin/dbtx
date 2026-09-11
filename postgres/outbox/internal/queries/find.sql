-- name: Find :one
with locked as (
  select id as locked_id
    from dbtx.outbox o
   where o.id = $1 for update SKIP LOCKED
)
   update dbtx.outbox o
      set retry_count = retry_count + 1
    where o.id = (select locked_id from locked)
returning *;
