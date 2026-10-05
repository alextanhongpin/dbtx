-- name: Nack :one
-- Exponential backoff: base * 2^(attempts-1), capped. `attempts` is the old
-- row value, i.e. the attempt that just failed. Terminal when exhausted.
   update dbtx.idempotency_keys
      set status = (
  case
       when attempts >= sqlc.arg(max_attempts)::int
       then 'failed'
       else 'retryable'
  end)::dbtx.idempotency_key_status,
          error = sqlc.arg(error)::text,
          lease_owner = null,
          lease_expires_at = null,
          retry_after = case
                             when attempts >= sqlc.arg(max_attempts)::int
                             then null
                             else clock_timestamp() + interval '1 second' * least(sqlc.arg(max_backoff_seconds)::float8, sqlc.arg(base_backoff_seconds)::float8 * power(2::float8, (attempts - 1)::float8))
                        end,
          completed_at = case
                              when attempts >= sqlc.arg(max_attempts)::int
                              then clock_timestamp()
                         end,
          expires_at = greatest(
            expires_at,
            clock_timestamp()
          + interval '1 second' * sqlc.arg(ttl_seconds)::float8
          ),
          updated_at = clock_timestamp()
    where idempotency_key = sqlc.arg(idempotency_key)::text
      and fencing_token = sqlc.arg(fencing_token)::bigint
      and status = 'in_progress'
returning *;
