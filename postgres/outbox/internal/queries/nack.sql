-- name: Nack :execrows
update dbtx.outbox
   set status = case when sqlc.arg(dead)::bool then 'dead' else 'pending' end,
       available_at = now()
     + interval '1 second' * sqlc.arg(delay_seconds)::float8,
       locked_by = null,
       last_error = sqlc.arg(last_error),
       updated_at = now()
 where id = sqlc.arg(id)
   and locked_by = sqlc.arg(locked_by)
   and status = 'processing';
