-- name: CompareAndSwap :one
with expired as (
  delete from dbtx.cache where dbtx.cache.key = $1 and dbtx.cache.expires_at <= statement_timestamp() returning dbtx.cache.key
),
curr as (
  select dbtx.cache.key
    from dbtx.cache
   where dbtx.cache.key = $1
     and lease is null
     and not exists (select 1 from expired)
),
swapped as (
     update dbtx.cache
        set value = sqlc.arg(new_value)::jsonb,
            expires_at = statement_timestamp() + sqlc.narg(ttl)::bigint * interval '1 microsecond',
            updated_at = statement_timestamp()
      where dbtx.cache.key = $1
        and lease is null
        and value = sqlc.arg(old_value)::jsonb
        and not exists (select 1 from expired)
  returning dbtx.cache.key
)
select exists (select 1 from curr) as found, exists (select 1 from swapped) as swapped;
