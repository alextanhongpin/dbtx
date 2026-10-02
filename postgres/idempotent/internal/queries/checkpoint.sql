-- name: Checkpoint :one
-- Extends the lease on commit, so a Claim blocked on the row lock does not
-- take over between steps. The previous checkpoint is appended to
-- checkpoint_logs.
   update dbtx.idempotency_keys
      set checkpoint_logs = checkpoint_logs || jsonb_build_array(
            jsonb_build_object('name', checkpoint, 'data', checkpoint_data)
          ),
          checkpoint = sqlc.arg(checkpoint)::text,
          checkpoint_data = sqlc.arg(checkpoint_data)::jsonb,
          lease_expires_at = now()
        + interval '1 second' * sqlc.arg(lease_seconds)::float8,
          updated_at = now()
    where idempotency_key = sqlc.arg(idempotency_key)::text
      and fencing_token = sqlc.arg(fencing_token)::bigint
      and status = 'in_progress'
returning checkpoint;
