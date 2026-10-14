-- name: Claim :many
with retry_args as (
    select sqlc.arg('limit')::int as "limit",
           sqlc.arg(locked_by)::text as locked_by,
           sqlc.arg(lease_seconds)::float8 as lease_seconds
), next as (
    select id
      from dbtx.inbox
     where status in ('pending', 'processing')  -- 'processing' = expired lease
       and available_at <= now()
       and attempts < max_attempts
  order by available_at, id
     limit (select "limit" from retry_args)
       for update SKIP LOCKED
)
select claimed.*
  from next
 cross join lateral dbtx.inbox_retry(next.id, 'claim', (select locked_by from retry_args),
     jsonb_build_object('lease_seconds', (select lease_seconds from retry_args))) claimed;
