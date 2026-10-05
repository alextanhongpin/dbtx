-- name: Claim :one
-- Columns that do not apply to the outcome are null; they are coalesced to
-- the Go zero values ('0001-01-01' scans to time.Time{}).
select outcome::text,
       coalesce(fencing_token, 0)::bigint as fencing_token,
       coalesce(attempts, 0)::int as attempts,
       coalesce(checkpoint, '')::text as checkpoint,
       coalesce(checkpoint_data, 'null')::jsonb as checkpoint_data,
       coalesce(response, 'null')::jsonb as response,
       coalesce(error, '')::text as error,
       coalesce(lease_expires_at, '0001-01-01 00:00:00+00')::timestamptz as lease_expires_at,
       coalesce(retry_after, '0001-01-01 00:00:00+00')::timestamptz as retry_after
  from dbtx.claim(
         sqlc.arg(idempotency_key)::text, sqlc.arg(request)::jsonb,
         sqlc.arg(lease_owner)::text, sqlc.arg(lease_seconds)::float8,
         sqlc.arg(max_attempts)::int, sqlc.arg(ttl_seconds)::float8
       );
