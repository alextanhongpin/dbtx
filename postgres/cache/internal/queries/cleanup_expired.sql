-- name: CleanupExpired :execrows
delete from dbtx.cache where expires_at is not null and expires_at <= now();
