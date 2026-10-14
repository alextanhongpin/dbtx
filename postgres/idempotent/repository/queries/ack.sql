-- name: Ack :one
with retry_args as (
    select sqlc.arg(response)::jsonb as response,
           sqlc.arg(ttl_seconds)::float8 as ttl_seconds,
           sqlc.arg(idempotency_key)::text as idempotency_key,
           sqlc.arg(fencing_token)::bigint as fencing_token
)
select fencing_token::bigint from dbtx.idempotent_retry(
    (select idempotency_key from retry_args), 'complete', (select fencing_token from retry_args),
    jsonb_build_object('response', (select response from retry_args), 'ttl_seconds', (select ttl_seconds from retry_args)));
