-- name: Load :one
select * from dbtx.cache where key = $1 for update;

-- name: Delete :one
delete from dbtx.cache where key = $1 returning *;

-- name: StoreOnce :one
insert into dbtx.cache(key, value, digest, expires_at)
     values ($1, $2, $3, $4)
on conflict (key) do nothing
  returning *;

-- name: Store :one
insert into dbtx.cache(key, value, digest, expires_at)
     values ($1, $2, $3, $4)
on conflict (key) do
     update
        set value = EXCLUDED.value, digest = EXCLUDED.digest, expires_at = EXCLUDED.expires_at
  returning *;

-- name: Expire :exec
update dbtx.cache set expires_at = $1 where key = $2;

-- name: DeleteExpired :one
delete
 from dbtx.cache
where key = $1
  and expires_at is not null
  and expires_at <= now()
returning *;

-- name: CompareAndDelete :one
delete
  from dbtx.cache
 where key = $1
   and (digest = $2 or (expires_at is not null and expires_at <= now()))
returning *;

-- name: CompareAndSwap :one
update dbtx.cache set value = $1 where key = $2 and digest = $3
returning *;

-- name: TTL :one
select * from dbtx.cache where key = $1;
