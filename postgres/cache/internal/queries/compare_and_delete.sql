-- name: CompareAndDelete :one
delete from dbtx.live_cache where key = $1 and digest = $2 returning *;
