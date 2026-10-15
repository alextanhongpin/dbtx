-- name: Ack :execrows
update dbtx.inbox
   set status = 'done'::dbtx.inbox_status,
       processed_at = now(),
       locked_by = null,
       last_error = null,
       updated_at = now()
 where id = sqlc.arg(id)::uuid
   and locked_by = sqlc.arg(locked_by)::text
   and status = 'processing';
