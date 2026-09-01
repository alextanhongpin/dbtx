-- name: StoreOnce :one
insert into dbtx.cache(key, value, digest, expires_at)
     values ($1, $2, $3, $4)
on conflict (key) do nothing
  returning *;
