-- name: Find :one
select * from dbtx.outbox o where o.id = $1 for update SKIP LOCKED;
