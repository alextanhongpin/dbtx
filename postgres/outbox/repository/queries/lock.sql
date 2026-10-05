-- name: Lock :execrows
-- Renews the lease of a message leased to locked_by, and locks its row until
-- the handler's transaction ends. Poll skips locked rows, and Dead waits for
-- them, so the message is not delivered again while it is being handled, even
-- if the lease expires meanwhile.
update dbtx.outbox
   set available_at = now()
     + interval '1 second' * sqlc.arg(lease_seconds)::float8,
       updated_at = now()
 where id = sqlc.arg(id)
   and locked_by = sqlc.arg(locked_by)::text
   and status = 'processing';
