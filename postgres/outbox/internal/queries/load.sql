-- name: Load :one
  select *
    from dbtx.outbox
   where (max_retry = 0 or retry_count < max_retry)
     and visible_at <= clock_timestamp()
order by id
   limit 1
     for update SKIP LOCKED;
