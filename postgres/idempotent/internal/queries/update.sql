-- name: Update :exec
update dbtx.idempotency_keys set response = $1 where scope = $2 and key = $3;
