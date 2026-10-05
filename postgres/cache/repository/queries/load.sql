-- name: Load :one
-- The select reads the statement snapshot, which still holds a row that the
-- expired CTE (or a concurrent transaction) deleted, so it checks liveness
-- itself instead of relying on the delete.
with expired as (
  delete from dbtx.cache where key = $1 and expires_at <= statement_timestamp() returning key
)
select *
  from dbtx.cache
 where dbtx.cache.key = $1
   and lease is null
   and (expires_at is null or expires_at > statement_timestamp());
