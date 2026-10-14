-- name: Poll :many
with retry_args as (
    select sqlc.narg(locked_by)::text as locked_by,
           sqlc.arg(lease_seconds)::float8 as lease_seconds,
           sqlc.arg(batch_size)::int as batch_size
), next as (
    select id
      from dbtx.outbox
     where status in ('pending', 'processing')
       and available_at <= now()
       and attempts < max_attempts
  order by available_at, id
     limit (select batch_size from retry_args)
       for update skip locked
)
select claimed.*
  from next
 cross join lateral dbtx.outbox_retry(next.id, 'claim', (select locked_by from retry_args),
     jsonb_build_object('lease_seconds', (select lease_seconds from retry_args))) claimed;
