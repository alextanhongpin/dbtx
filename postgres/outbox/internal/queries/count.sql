-- name: Count :one
select count(*)
  from dbtx.outbox
 where (max_retry = 0 or retry_count < max_retry)
   and visible_at <= now();
