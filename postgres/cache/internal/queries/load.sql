-- name: Load :one
select * from dbtx.cache where key = $1 for update;
