-- name: StoreOnce :one
insert into dbtx.live_cache(key, value, digest, expires_at)
     values ($1, $2, $3, $4)
on conflict (key) do
     update
        set value = $2, digest = $3, expires_at = $4
      where dbtx.live_cache.expires_at is not null
        and dbtx.live_cache.expires_at < now()
  returning *;
