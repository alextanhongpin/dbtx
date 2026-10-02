-- name: TTL :one
-- Returns the remaining time to live in microseconds, or 0 if the key does not
-- expire. A live key always reports at least 1 microsecond.
with expired as (
  delete from dbtx.cache where key = $1 and expires_at <= statement_timestamp() returning key
)
select (case
          when expires_at is null then 0
          else greatest(1, (extract(epoch from expires_at - statement_timestamp()) * 1000000)::bigint)
        end)::bigint as ttl
  from dbtx.cache
 where dbtx.cache.key = $1
   and lease is null
   and not exists (select 1 from expired);
