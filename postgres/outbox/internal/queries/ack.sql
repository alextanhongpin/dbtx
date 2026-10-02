-- name: Ack :execrows
update dbtx.outbox
   set status = 'done'::dbtx.outbox_status,
       processed_at = now(),
       locked_by = null,
       last_error = null,
       updated_at = now()
 where id = $1
   and locked_by = $2
   and status = 'processing';
