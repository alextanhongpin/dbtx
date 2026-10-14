-- name: LoadCacheEntry :one
SELECT value,
       (value IS NOT NULL AND expires_at > clock_timestamp())::boolean AS fresh,
       (value IS NOT NULL AND stale_until > clock_timestamp())::boolean AS stale_ok
FROM durable.cache WHERE key = sqlc.arg(key);

-- name: SetCacheEntry :exec
INSERT INTO durable.cache AS c (key, value, version, expires_at, stale_until)
VALUES (sqlc.arg(key), sqlc.arg(value)::jsonb, nextval('durable.fencing_seq'),
        clock_timestamp() + sqlc.arg(ttl_us)::bigint * interval '1 microsecond',
        clock_timestamp() + sqlc.arg(stale_us)::bigint * interval '1 microsecond')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, version = EXCLUDED.version,
    expires_at = EXCLUDED.expires_at, stale_until = EXCLUDED.stale_until, updated_at = now();

-- name: ClaimCompute :one
-- Fails (no row) while another caller holds a live compute lease, or when a
-- fresh value appeared since the caller looked.
INSERT INTO durable.cache AS c (key, compute_owner, compute_expires_at)
VALUES (sqlc.arg(key), sqlc.arg(owner)::text, clock_timestamp() + sqlc.arg(ttl_us)::bigint * interval '1 microsecond')
ON CONFLICT (key) DO UPDATE
SET compute_owner = EXCLUDED.compute_owner, compute_expires_at = EXCLUDED.compute_expires_at
WHERE (c.compute_owner IS NULL OR c.compute_expires_at <= clock_timestamp())
  AND NOT (c.value IS NOT NULL AND c.expires_at > clock_timestamp())
RETURNING nextval('durable.fencing_seq')::bigint AS token;

-- name: StoreComputed :execrows
UPDATE durable.cache
SET value = sqlc.arg(value)::jsonb, version = sqlc.arg(token)::bigint,
    expires_at = clock_timestamp() + sqlc.arg(ttl_us)::bigint * interval '1 microsecond',
    stale_until = clock_timestamp() + sqlc.arg(stale_us)::bigint * interval '1 microsecond',
    compute_owner = CASE WHEN compute_owner = sqlc.arg(owner)::text THEN NULL ELSE compute_owner END,
    updated_at = now()
WHERE key = sqlc.arg(key) AND version < sqlc.arg(token)::bigint;

-- name: ReleaseCompute :exec
UPDATE durable.cache SET compute_owner = NULL, compute_expires_at = NULL
WHERE key = sqlc.arg(key) AND compute_owner = sqlc.arg(owner)::text;

-- name: InvalidateCacheEntry :exec
-- Tombstone with a new version, so an older computation cannot store (D17).
UPDATE durable.cache
SET value = NULL, version = nextval('durable.fencing_seq'),
    expires_at = NULL, stale_until = NULL, updated_at = now()
WHERE key = sqlc.arg(key);

-- name: PurgeCache :execrows
DELETE FROM durable.cache WHERE key IN (
    SELECT c.key FROM durable.cache c
    WHERE (c.stale_until IS NULL OR c.stale_until <= clock_timestamp())
      AND (c.compute_owner IS NULL OR c.compute_expires_at <= clock_timestamp())
    LIMIT sqlc.arg(lim)::integer);
