-- name: Delete :one
delete from dbtx.live_cache where key = $1 returning *;
