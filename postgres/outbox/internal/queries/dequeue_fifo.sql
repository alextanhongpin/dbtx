-- name: DequeueFIFO :one
with locked as (
      select o.id as locked_id
        from dbtx.outbox o
       where retry_at is null
          or retry_at <= now()
    order by id asc
       limit 1 for
      update SKIP LOCKED
)
   update dbtx.outbox o
      set updated_at = now(), run_at = now(), retry_count = o.retry_count + 1
     from locked l
    where o.id = l.locked_id
returning o.*;
