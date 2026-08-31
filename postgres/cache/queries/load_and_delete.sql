-- name: LoadAndDelete :one
delete from dbtx.cache where key = $1 returning *;
