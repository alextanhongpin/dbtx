-- name: Delete :one
-- Deletes leased rows too, so that invalidating a key also discards any
-- value that is being computed for it.
with expired as (
  delete from dbtx.cache where key = $1 and expires_at <= statement_timestamp() returning key
)
   delete
     from dbtx.cache
    where dbtx.cache.key = $1
      and not exists (select 1 from expired)
returning *;
