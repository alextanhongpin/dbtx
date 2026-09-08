-- name: Expire :one
update dbtx.live_cache set expires_at = $1 where key = $2 returning *;
