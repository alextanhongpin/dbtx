-- name: CountDLQ :one
select count(*) from dbtx.outbox_dlq;
