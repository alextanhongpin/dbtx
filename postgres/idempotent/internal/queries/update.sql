-- name: Update :exec
update dbtx.idempotency_keys set request = $1, response = $2 where key = $3;
