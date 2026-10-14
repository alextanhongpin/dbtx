-- name: Nack :one
-- Exponential backoff: base * 2^(attempts-1), capped. `attempts` is the old
-- row value, i.e. the attempt that just failed. Terminal when exhausted.
with retry_args as (
    select sqlc.arg(max_attempts)::int as max_attempts,
           sqlc.arg(error)::text as error,
           sqlc.arg(max_backoff_seconds)::float8 as max_backoff_seconds,
           sqlc.arg(base_backoff_seconds)::float8 as base_backoff_seconds,
           sqlc.arg(ttl_seconds)::float8 as ttl_seconds,
           sqlc.arg(idempotency_key)::text as idempotency_key,
           sqlc.arg(fencing_token)::bigint as fencing_token
)
select fencing_token::bigint from dbtx.idempotent_retry(
    (select idempotency_key from retry_args), 'nack', (select fencing_token from retry_args),
    jsonb_build_object('error', (select error from retry_args), 'max_attempts', (select max_attempts from retry_args), 'base_backoff_seconds', (select base_backoff_seconds from retry_args), 'max_backoff_seconds', (select max_backoff_seconds from retry_args), 'ttl_seconds', (select ttl_seconds from retry_args)));
