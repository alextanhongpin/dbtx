-- name: Lock :one
-- Locks the row for the duration of the step transaction and extends the
-- lease. While the row is locked, a concurrent Claim blocks until commit, so
-- no separate heartbeat is needed.
with retry_args as (
    select sqlc.arg(lease_seconds)::float8 as lease_seconds,
           sqlc.arg(idempotency_key)::text as idempotency_key,
           sqlc.arg(fencing_token)::bigint as fencing_token
)
select fencing_token::bigint from dbtx.idempotent_retry(
    (select idempotency_key from retry_args), 'renew', (select fencing_token from retry_args),
    jsonb_build_object('lease_seconds', (select lease_seconds from retry_args)));
