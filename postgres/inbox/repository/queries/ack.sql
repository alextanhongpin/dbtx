-- name: Ack :execrows
with retry_args as (
    select sqlc.arg(id)::uuid as id,
           sqlc.arg(locked_by)::text as locked_by
)
select 1 from dbtx.inbox_retry((select id from retry_args), 'complete', (select locked_by from retry_args), '{}'::jsonb);
