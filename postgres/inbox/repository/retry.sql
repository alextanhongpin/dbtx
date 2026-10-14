-- Additive lifecycle migration; existing tables and columns remain authoritative.
-- Aliases are directly updatable. Never update both aliases of one base column.
create or replace view dbtx.inbox_retry_state with (security_invoker = true) as
select id as retry_key,
       status,
       attempts,
       max_attempts,
       locked_by as lease_owner,
       null::bigint as fencing_token,
       available_at as retry_at,
       available_at as lease_expires_at,
       last_error,
       processed_at as completed_at,
       updated_at
  from dbtx.inbox;

-- Typed module adapter. The shared transition locks and updates this same row;
-- domain result/retention writes below occur before its transaction releases it.
create or replace function dbtx.inbox_retry(
    p_key uuid, p_action text, p_owner text, p_options jsonb
) returns setof dbtx.inbox language plpgsql volatile as $$
declare
    state jsonb;
begin
    for state in select * from dbtx.retry_transition(
        'dbtx.inbox_retry_state'::regclass, p_key::text, p_action,
        p_owner, null::bigint, '{"running": "processing", "ready": "pending", "success": "done", "failure": "dead", "nack_exhausts": false}'::jsonb, p_options, now()
    ) loop
        return query select t.* from dbtx.inbox t where id = p_key;
    end loop;
end;
$$;
