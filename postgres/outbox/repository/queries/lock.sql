-- name: Lock :execrows
-- Renews the lease of a message leased to locked_by, and locks its row until
-- the handler's transaction ends. Poll skips locked rows, and Dead waits for
-- them, so the message is not delivered again while it is being handled, even
-- if the lease expires meanwhile.
with retry_args as (
    select sqlc.arg(lease_seconds)::float8 as lease_seconds,
           sqlc.arg(id)::uuid as id,
           sqlc.arg(locked_by)::text as locked_by
)
select 1 from dbtx.outbox_retry((select id from retry_args), 'renew', (select locked_by from retry_args), jsonb_build_object('lease_seconds', (select lease_seconds from retry_args)));
