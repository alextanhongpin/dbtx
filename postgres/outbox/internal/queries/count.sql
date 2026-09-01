-- name: Count :one
select count(*) from dbtx.outbox where retry_at is null or retry_at <= now();
