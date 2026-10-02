-- name: Inspect :one
-- Called only when Claim returned no rows.
select status,
       response,
       error,
       lease_expires_at,
       attempts,
       coalesce(status = 'in_progress' and lease_expires_at <= now(), false)::bool as lease_expired,
       (request <> sqlc.arg(request)::jsonb) as payload_mismatch
  from dbtx.idempotency_keys
 where idempotency_key = sqlc.arg(idempotency_key)::text;
