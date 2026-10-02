-- name: Nack :exec
update dbtx.inbox
   set status = (case when sqlc.arg(dead)::bool then 'dead' else 'pending' end)::dbtx.inbox_status,
       available_at = now()
     + interval '1 second' * sqlc.arg(delay_seconds)::float8,
       locked_by = null,
       last_error = sqlc.arg(last_error)::text,
       updated_at = now()
 where id = $1
   and locked_by = sqlc.arg(locked_by)::text
   and status = 'processing';
