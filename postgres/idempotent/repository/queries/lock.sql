-- name: Lock :one
-- Locks the row for the duration of the step transaction and extends the
-- lease. While the row is locked, a concurrent Claim blocks until commit, so
-- no separate heartbeat is needed.
   update dbtx.idempotency_keys
      set lease_expires_at = now()
        + interval '1 second' * sqlc.arg(lease_seconds)::float8,
          updated_at = now()
    where idempotency_key = sqlc.arg(idempotency_key)::text
      and fencing_token = sqlc.arg(fencing_token)::bigint
      and status = 'in_progress'
returning fencing_token;
