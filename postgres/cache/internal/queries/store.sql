-- name: Store :one
insert into dbtx.cache(key, value, digest, expires_at)
     values ($1, $2, $3, $4)
on conflict (key) do
     update
        set value = EXCLUDED.value, digest = EXCLUDED.digest, expires_at = EXCLUDED.expires_at
  returning *;
