-- name: Expire :one
update dbtx.cache set expires_at = $1 where key = $2 returning *;
