-- name: Fail :one
   update dbtx.idempotency_keys
      set status = 'failed',
          response = $3,
          error = sqlc.arg(error)::text,
          lease_owner = null,
          lease_expires_at = null,
          completed_at = now(),
          updated_at = now()
    where idempotency_key = $1
      and fencing_token = $2
      and status = 'in_progress'
returning *;
