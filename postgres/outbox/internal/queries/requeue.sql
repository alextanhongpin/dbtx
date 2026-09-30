-- name: Requeue :execrows
update dbtx.outbox
   set status = 'pending',
       attempts = 0,
       available_at = now(),
       locked_by = null,
       last_error = null,
       updated_at = now()
 where id = $1
   and status = 'dead';
