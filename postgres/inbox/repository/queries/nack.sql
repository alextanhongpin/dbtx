-- name: Nack :execrows
with retry_args as (
    select sqlc.arg(id)::uuid as id,
           sqlc.arg(dead)::bool as dead,
           sqlc.arg(delay_seconds)::float8 as delay_seconds,
           sqlc.arg(last_error)::text as last_error,
           sqlc.arg(locked_by)::text as locked_by
)
select 1 from dbtx.inbox_retry((select id from retry_args), 'nack', (select locked_by from retry_args), jsonb_build_object('force', (select dead from retry_args), 'error', (select last_error from retry_args), 'delay_seconds', (select delay_seconds from retry_args)));
