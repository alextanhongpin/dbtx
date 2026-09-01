-- name: Delete :one
delete from dbtx.outbox where id = $1 returning *;
