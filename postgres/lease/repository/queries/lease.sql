-- name: AcquireLease :one
SELECT dbtx.acquire_lease(sqlc.arg(namespace)::text, sqlc.arg(key)::text,
    sqlc.arg(owner_id)::text, sqlc.arg(ttl_us)::bigint)::bigint AS token;

-- name: RenewLease :one
SELECT dbtx.renew_lease(sqlc.arg(namespace)::text, sqlc.arg(key)::text,
    sqlc.arg(owner_id)::text, sqlc.arg(token)::bigint,
    sqlc.arg(ttl_us)::bigint)::boolean AS renewed;

-- name: ReleaseLease :execrows
UPDATE dbtx.leases SET owner_id = NULL, expires_at = '-infinity'
WHERE namespace = sqlc.arg(namespace) AND key = sqlc.arg(key)
  AND owner_id = sqlc.arg(owner_id) AND fencing_token = sqlc.arg(token);

-- name: LockLeaseRow :one
SELECT fencing_token FROM dbtx.leases
WHERE namespace = sqlc.arg(namespace) AND key = sqlc.arg(key)
FOR UPDATE;

-- name: ValidateLease :one
SELECT EXISTS (
    SELECT 1 FROM dbtx.leases
    WHERE namespace = sqlc.arg(namespace) AND key = sqlc.arg(key)
      AND owner_id = sqlc.arg(owner_id) AND fencing_token = sqlc.arg(token)
      AND expires_at > clock_timestamp()
)::boolean AS valid;

-- name: SetTransactionTimeouts :exec
SELECT set_config('statement_timeout', sqlc.arg(timeout)::text, true),
       set_config('lock_timeout', sqlc.arg(timeout)::text, true),
       set_config('idle_in_transaction_session_timeout', sqlc.arg(timeout)::text, true),
       set_config('transaction_timeout', sqlc.arg(timeout)::text, true);

-- name: ReleaseOwner :execrows
-- Used only after an ambiguous acquisition whose token was not received.
UPDATE dbtx.leases SET owner_id = NULL, expires_at = '-infinity'
WHERE namespace = sqlc.arg(namespace) AND key = sqlc.arg(key)
  AND owner_id = sqlc.arg(owner_id);
