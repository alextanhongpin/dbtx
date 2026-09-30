-- name: Purge :execrows
delete from dbtx.outbox where status = $1 and updated_at < $2;
