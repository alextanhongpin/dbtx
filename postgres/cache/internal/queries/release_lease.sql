-- name: ReleaseLease :execrows
delete from dbtx.cache where key = $1 and lease = sqlc.arg(lease)::uuid;
