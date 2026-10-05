-- name: Dead :execrows
-- Dead marks messages whose lease expired on their final attempt, for
-- example because the worker crashed, as dead.
--
-- Rows locked by a running handler are skipped instead of waited for: the
-- handler is still delivering the message. Skipping also means concurrent
-- calls never wait on each other, so they cannot deadlock.
with expired as (
    select id
      from dbtx.outbox
     where status = 'processing'
       and available_at <= now()
       and attempts >= max_attempts
       for update skip locked
)
update dbtx.outbox o
   set status = 'dead'::dbtx.outbox_status,
       locked_by = null,
       last_error = 'lease expired on final attempt',
       updated_at = now()
  from expired
 where o.id = expired.id;
