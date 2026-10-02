-- name: Count :one
select count(*)
  from dbtx.outbox
 where status in ('pending', 'processing')
   and available_at <= now()
   and attempts < max_attempts;
