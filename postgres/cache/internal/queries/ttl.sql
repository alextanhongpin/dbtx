-- name: TTL :one
select * from dbtx.cache where key = $1;
