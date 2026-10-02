-- name: ExpireLeases :exec
update dbtx.inbox
   set status = 'dead'::dbtx.inbox_status,
       locked_by = null,
       last_error = 'lease expired on final attempt',
       updated_at = now()
 where status = 'processing'
   and available_at <= now()
   and attempts >= max_attempts;
