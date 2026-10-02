-- name: Purge :execrows
delete
  from dbtx.idempotency_keys
 where ctid in
            (
              select ctid
                from dbtx.idempotency_keys
               where expires_at < now()
                 and (status <> 'in_progress' or lease_expires_at < now())
               limit 1000
                 for update SKIP LOCKED
            );
