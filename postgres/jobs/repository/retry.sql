-- Additive lifecycle migration; existing tables and columns remain authoritative.
-- Aliases are directly updatable. Never update both aliases of one base column.
create or replace view dbtx.jobs_retry_state with (security_invoker = true) as
select id as retry_key,
       status,
       attempts,
       max_attempts,
       worker_id as lease_owner,
       fencing_token,
       visible_at as retry_at,
       visible_at as lease_expires_at,
       error as last_error,
       processed_at as completed_at,
       updated_at
  from dbtx.jobs;

-- Typed module adapter. The shared transition locks and updates this same row;
-- domain result/retention writes below occur before its transaction releases it.
create or replace function dbtx.jobs_retry(
    p_key uuid, p_action text, p_owner text, p_token bigint, p_options jsonb
) returns setof dbtx.jobs language plpgsql volatile as $$
declare
    state jsonb;
begin
    for state in select * from dbtx.retry_transition(
        'dbtx.jobs_retry_state'::regclass, p_key::text, p_action,
        p_owner, p_token, '{"running": "running", "ready": "error", "queued": "queued", "success": "success", "failure": "failed", "check_token": true, "increment_token": true, "complete_failure": true, "finish_time": "now", "expire_ready": true}'::jsonb, p_options, clock_timestamp()
    ) loop
        if p_options ? 'response' then
            update dbtx.jobs t set response = p_options->'response'
             where id = p_key;
        end if;
        return query select t.* from dbtx.jobs t where id = p_key;
    end loop;
end;
$$;
