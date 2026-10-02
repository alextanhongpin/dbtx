-- name: Purge :execrows
delete
  from dbtx.inbox
 where status = sqlc.arg(status)::dbtx.inbox_status
   and updated_at < sqlc.arg(updated_at)::timestamptz;
