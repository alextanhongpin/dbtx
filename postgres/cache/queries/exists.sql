-- name: Exists :one
select exists (select 1 from dbtx.cache where key = $1);
