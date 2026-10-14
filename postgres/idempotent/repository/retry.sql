-- Additive lifecycle migration; existing tables and columns remain authoritative.
-- Aliases are directly updatable. Never update both aliases of one base column.
create or replace view dbtx.idempotent_retry_state with (security_invoker = true) as
select idempotency_key as retry_key,
       status,
       attempts,
       null::integer as max_attempts,
       lease_owner,
       fencing_token,
       retry_after as retry_at,
       lease_expires_at,
       error as last_error,
       completed_at,
       updated_at
  from dbtx.idempotency_keys;

-- Typed module adapter. The shared transition locks and updates this same row;
-- domain result/retention writes below occur before its transaction releases it.
create or replace function dbtx.idempotent_retry(
    p_key text, p_action text, p_token bigint, p_options jsonb
) returns setof dbtx.idempotency_keys language plpgsql volatile as $$
declare
    state jsonb;
begin
    for state in select * from dbtx.retry_transition(
        'dbtx.idempotent_retry_state'::regclass, p_key::text, p_action,
        null::text, p_token, '{"running": "in_progress", "ready": "retryable", "success": "completed", "failure": "failed", "check_owner": false, "check_token": true, "separate_lease": true, "complete_failure": true, "null_backoff": true, "finish_time": "clear", "clear_success_error": false}'::jsonb, p_options, clock_timestamp()
    ) loop
        if p_options ? 'ttl_seconds' or p_options ? 'response' then
            update dbtx.idempotency_keys t
               set expires_at = case when p_options ? 'ttl_seconds'
                                     then greatest(expires_at, clock_timestamp() + interval '1 second' * (p_options->>'ttl_seconds')::float8)
                                     else expires_at end,
                   response = case when p_options ? 'response' then p_options->'response' else response end
             where idempotency_key = p_key;
        end if;
        return query select t.* from dbtx.idempotency_keys t where idempotency_key = p_key;
    end loop;
end;
$$;
