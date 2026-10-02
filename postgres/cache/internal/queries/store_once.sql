-- name: StoreOnce :execrows
-- Inserts the value unless a live row (including a lease placeholder) exists.
-- An expired row is overwritten in place, since a sibling CTE deleting it would
-- not be visible to the insert's conflict check.
insert into dbtx.cache as c(key, value, expires_at)
     values ($1, $2, statement_timestamp() + sqlc.narg(ttl)::bigint * interval '1 microsecond')
on conflict (key) do
     update
        set value = excluded.value,
            lease = null,
            expires_at = excluded.expires_at,
            created_at = statement_timestamp(),
            updated_at = statement_timestamp()
      where c.expires_at is not null
        and c.expires_at <= statement_timestamp();
