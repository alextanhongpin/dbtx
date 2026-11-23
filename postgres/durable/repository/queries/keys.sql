-- name: BeginKey :one
SELECT outcome::text AS outcome, key_id::uuid AS key_id, token::bigint AS token,
       response::jsonb AS response,
       coalesce(execution_id, '00000000-0000-0000-0000-000000000000')::uuid AS execution_id,
       attempts::integer AS attempts,
       coalesce(retry_at, 'epoch')::timestamptz AS retry_at, -- epoch means none
       coalesce(last_error, '')::text AS last_error,
       checkpoint_name::text AS checkpoint_name, checkpoint_data::jsonb AS checkpoint_data
FROM durable.begin_key(sqlc.arg(scope)::text, sqlc.arg(key)::text, sqlc.arg(request)::jsonb,
    sqlc.arg(owner_id)::text, sqlc.arg(lease_us)::bigint, sqlc.arg(retention_us)::bigint,
    sqlc.arg(max_attempts)::integer);

-- name: SetKeyResponse :exec
-- Run after durable.lease_finish in the same transaction.
UPDATE durable.idempotency_keys
SET response = sqlc.arg(response)::jsonb,
    expires_at = clock_timestamp() + sqlc.arg(retention_us)::bigint * interval '1 microsecond'
WHERE id = sqlc.arg(id);

-- name: SetKeyCheckpoint :exec
-- Run after durable.lease_lock in the same transaction.
UPDATE durable.idempotency_keys
SET checkpoint_name = sqlc.arg(checkpoint_name)::text,
    checkpoint_data = sqlc.narg(checkpoint_data)::jsonb, updated_at = now()
WHERE id = sqlc.arg(id);

-- name: ReleaseKey :exec
-- Gives the attempt back. A key with nothing to keep (no other attempt and
-- no checkpoint) is deleted, so it can be reused with any request.
WITH del AS (
    DELETE FROM durable.idempotency_keys
    WHERE id = sqlc.arg(id) AND owner_id = sqlc.arg(owner_id) AND token = sqlc.arg(token)
      AND status = 'in_flight' AND attempts <= 1 AND checkpoint_name = ''
)
UPDATE durable.idempotency_keys k
SET owner_id = NULL, lease_expires_at = NULL, attempts = greatest(k.attempts - 1, 0),
    updated_at = now()
WHERE k.id = sqlc.arg(id) AND k.owner_id = sqlc.arg(owner_id) AND k.token = sqlc.arg(token)
  AND k.status = 'in_flight' AND NOT (k.attempts <= 1 AND k.checkpoint_name = '');

-- name: AbortKey :one
-- Ends an attempt that failed. With attempts left the key waits delay_us
-- before the next attempt; otherwise it becomes exhausted.
UPDATE durable.idempotency_keys k
SET status = CASE WHEN k.attempts >= sqlc.arg(max_attempts)::integer
                  THEN 'exhausted' ELSE 'in_flight' END,
    owner_id = NULL, lease_expires_at = NULL,
    last_error = sqlc.arg(last_error)::text,
    retry_at = CASE WHEN sqlc.arg(delay_us)::bigint > 0
                    THEN clock_timestamp() + sqlc.arg(delay_us)::bigint * interval '1 microsecond' END,
    expires_at = CASE WHEN k.attempts >= sqlc.arg(max_attempts)::integer
                      THEN clock_timestamp() + sqlc.arg(retention_us)::bigint * interval '1 microsecond'
                      ELSE k.expires_at END,
    updated_at = now()
WHERE k.id = sqlc.arg(id) AND k.owner_id = sqlc.arg(owner_id) AND k.token = sqlc.arg(token)
  AND k.status = 'in_flight'
RETURNING k.status, k.retry_at;

-- name: PurgeKeys :execrows
DELETE FROM durable.idempotency_keys WHERE id IN (
    SELECT k.id FROM durable.idempotency_keys k
    WHERE k.expires_at <= clock_timestamp()
      AND NOT (k.status = 'in_flight' AND k.lease_expires_at > clock_timestamp())
    LIMIT sqlc.arg(lim) FOR UPDATE SKIP LOCKED);
