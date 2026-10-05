-- name: Release :execrows
-- Returns a leased message that was never handled, and refunds its attempt.
update dbtx.inbox
   set status = 'pending'::dbtx.inbox_status,
       attempts = attempts - 1,
       locked_by = null,
       available_at = now(),
       updated_at = now()
 where id = sqlc.arg(id)::uuid
   and locked_by = sqlc.arg(locked_by)::text
   and status = 'processing';
