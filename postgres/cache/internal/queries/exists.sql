-- name: Exists :one
with expired as (
  delete from dbtx.cache where key = $1 and expires_at <= statement_timestamp() returning key
)
select exists
       (
         select 1
           from dbtx.cache
          where dbtx.cache.key = $1
            and lease is null
            and not exists (select 1 from expired)
       );
