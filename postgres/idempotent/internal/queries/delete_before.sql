-- name: DeleteBefore :execrows
delete from dbtx.idempotency_keys where created_at < $1;
