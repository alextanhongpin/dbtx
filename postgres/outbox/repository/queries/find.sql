-- name: Find :one
select * from dbtx.outbox where id = $1;
