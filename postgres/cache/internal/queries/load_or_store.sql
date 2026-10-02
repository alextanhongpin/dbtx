-- name: LoadOrStore :one
-- Stores the value (or a lease placeholder) unless a live row exists, in which
-- case the live row is returned with loaded = true. The caller must check the
-- returned lease to tell a computed value from an in-flight placeholder.
--
-- Returns no rows when a concurrent transaction inserted the key after this
-- statement's snapshot was taken; the caller should retry.
with ins as (
  insert into dbtx.cache as c(key, value, lease, expires_at)
       values ($1, sqlc.arg(value)::jsonb, sqlc.narg(lease)::uuid, statement_timestamp() + sqlc.narg(ttl)::bigint * interval '1 microsecond')
  on conflict (key) do
       update
          set value = excluded.value,
              lease = excluded.lease,
              expires_at = excluded.expires_at,
              created_at = statement_timestamp(),
              updated_at = statement_timestamp()
        where c.expires_at is not null
          and c.expires_at <= statement_timestamp()  -- only overwrite if the existing row is expired
    returning *
)
select *, false as loaded
  from ins
union all
select *, true as loaded
  from dbtx.cache
 where key = $1
   and not exists (select 1 from ins);
