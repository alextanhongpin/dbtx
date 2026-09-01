-- name: CompareAndDelete :one
delete from dbtx.cache where key = $1 and digest = $2 returning *;
