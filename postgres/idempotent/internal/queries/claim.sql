-- name: Claim :one
insert into dbtx.idempotency_keys as ik(idempotency_key, request, status,
                                        fencing_token, lease_owner,
                                        lease_expires_at, attempts)
     values (sqlc.arg(idempotency_key)::text, sqlc.arg(request)::jsonb,
             'in_progress', 1, sqlc.arg(lease_owner)::text,
             now() + interval '1 second' * sqlc.arg(lease_seconds)::float8, 1)
on conflict (idempotency_key) do
     update
        set status = 'in_progress',
            fencing_token = ik.fencing_token + 1,
            lease_owner = EXCLUDED.lease_owner,
            lease_expires_at = EXCLUDED.lease_expires_at,
            attempts = ik.attempts + 1,
            error = null,
            updated_at = now()
      where ik.request = EXCLUDED.request
        and ik.attempts < sqlc.arg(max_attempts)::int
        and ((ik.status = 'in_progress' and ik.lease_expires_at <= now()) or ik.status = 'retryable')
  returning fencing_token,
            attempts,
            checkpoint,  -- resume from here on takeover
            checkpoint_data,
            (xmax = 0) as is_new;  -- true = fresh insert, false = takeover
