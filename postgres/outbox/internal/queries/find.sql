-- name: Find :one
select * from dbtx.outbox o where o.id = $1 for update SKIP LOCKED;

-- name: Exists :one
select exists(select 1 from dbtx.outbox o where o.id = $1);
