-- name: Load :one
select * from dbtx.live_cache where key = $1 for update;
