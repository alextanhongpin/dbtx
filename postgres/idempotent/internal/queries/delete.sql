-- name: Delete :one
delete from dbtx.idempotency_keys where scope = $1 and key = $2 returning *;
