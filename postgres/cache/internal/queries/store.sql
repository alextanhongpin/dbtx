-- name: Store :one
-- Overwrites any existing row, including a lease placeholder. The lease
-- holder then fails to fulfill its lease and does not clobber this value.
insert into dbtx.cache(key, value, expires_at)
     values ($1, $2, statement_timestamp() + sqlc.narg(ttl)::bigint * interval '1 microsecond')
on conflict (key) do
     update
        set value = excluded.value,
            lease = null,
            expires_at = excluded.expires_at,
            updated_at = statement_timestamp()
  returning *;
