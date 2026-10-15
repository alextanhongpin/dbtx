-- name: BeginKey :one
SELECT outcome::text AS outcome, key_id::uuid AS key_id, token::bigint AS token,
       response::jsonb AS response,
       coalesce(execution_id, '00000000-0000-0000-0000-000000000000')::uuid AS execution_id
FROM durable.begin_key(sqlc.arg(scope)::text, sqlc.arg(key)::text, sqlc.arg(request)::jsonb,
    sqlc.arg(owner_id)::text, sqlc.arg(lease_us)::bigint, sqlc.arg(retention_us)::bigint);

-- name: SetKeyResponse :exec
-- Run after durable.lease_finish in the same transaction.
UPDATE durable.idempotency_keys
SET response = sqlc.arg(response)::jsonb,
    expires_at = clock_timestamp() + sqlc.arg(retention_us)::bigint * interval '1 microsecond'
WHERE id = sqlc.arg(id);

-- name: ReleaseKey :exec
DELETE FROM durable.idempotency_keys
WHERE id = sqlc.arg(id) AND owner_id = sqlc.arg(owner_id) AND token = sqlc.arg(token)
  AND status = 'in_flight';

-- name: PurgeKeys :execrows
DELETE FROM durable.idempotency_keys WHERE id IN (
    SELECT k.id FROM durable.idempotency_keys k
    WHERE k.expires_at <= clock_timestamp()
      AND NOT (k.status = 'in_flight' AND k.lease_expires_at > clock_timestamp())
    LIMIT sqlc.arg(lim) FOR UPDATE SKIP LOCKED);
