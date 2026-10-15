-- name: Fail :one
   update dbtx.idempotency_keys
      set status = 'failed',
          response = sqlc.arg(response)::jsonb,
          error = sqlc.arg(error)::text,
          lease_owner = null,
          lease_expires_at = null,
          retry_after = null,
          completed_at = clock_timestamp(),
          expires_at = greatest(
            expires_at,
            clock_timestamp()
          + interval '1 second' * sqlc.arg(ttl_seconds)::float8
          ),
          updated_at = clock_timestamp()
    where idempotency_key = sqlc.arg(idempotency_key)::text
      and fencing_token = sqlc.arg(fencing_token)::bigint
      and status = 'in_progress'
returning fencing_token;
