-- name: Dead :execrows
-- Dead marks messages whose lease expired on their final attempt, for
-- example because the worker crashed, as dead.
update dbtx.outbox
   set status = 'dead',
       locked_by = null,
       last_error = 'lease expired on final attempt',
       updated_at = now()
 where status = 'processing'
   and available_at <= now()
   and attempts >= max_attempts;
