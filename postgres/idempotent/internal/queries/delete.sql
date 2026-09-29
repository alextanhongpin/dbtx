-- name: Delete :one
delete from dbtx.idempotency_keys where key = $1 returning *;
