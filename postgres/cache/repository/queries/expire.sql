-- name: Expire :one
with expired as (
  delete from dbtx.cache where key = $1 and expires_at <= statement_timestamp() returning key
)
   update dbtx.cache
      set expires_at = statement_timestamp() + sqlc.narg(ttl)::bigint * interval '1 microsecond',
          updated_at = statement_timestamp()
    where dbtx.cache.key = $1
      and lease is null
      and not exists (select 1 from expired)
returning *;
