-- name: ExpireLeases :exec
-- Marks messages whose lease expired on their final attempt as dead.
--
-- Rows locked by a running handler are skipped instead of waited for: the
-- handler is still delivering the message. Skipping also means concurrent
-- calls never wait on each other, so they cannot deadlock.
with expired as (
    select id
      from dbtx.inbox
     where status = 'processing'
       and available_at <= now()
       and attempts >= max_attempts
       for update skip locked
)
update dbtx.inbox i
   set status = 'dead'::dbtx.inbox_status,
       locked_by = null,
       last_error = 'lease expired on final attempt',
       updated_at = now()
  from expired
 where i.id = expired.id;
