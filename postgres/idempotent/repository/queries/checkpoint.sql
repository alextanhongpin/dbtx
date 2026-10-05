-- name: Checkpoint :one
-- Extends the lease on commit, so a Claim blocked on the row lock does not
-- take over between steps. The previous checkpoint is appended to
-- checkpoint_logs, which keeps the last 100 entries, so that a long or
-- looping run does not rewrite an ever larger value on every step.
   update dbtx.idempotency_keys
      set checkpoint_logs =
          (
            select coalesce(jsonb_agg(log.entry order by log.n), '[]')
              from jsonb_array_elements(
                     checkpoint_logs
                  || jsonb_build_array(
                       jsonb_build_object('name', checkpoint, 'data', checkpoint_data)
                     )
                   ) with ordinality as log(entry, n)
             where log.n > jsonb_array_length(checkpoint_logs) + 1 - 100
          ),
          checkpoint = sqlc.arg(checkpoint)::text,
          checkpoint_data = sqlc.arg(checkpoint_data)::jsonb,
          updated_at = now(),
          lease_expires_at = clock_timestamp()
        + interval '1 second' * sqlc.arg(lease_seconds)::float8
    where idempotency_key = sqlc.arg(idempotency_key)::text
      and fencing_token = sqlc.arg(fencing_token)::bigint
      and status = 'in_progress'
returning checkpoint;
