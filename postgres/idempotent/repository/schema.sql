create schema if not exists dbtx;

create TYPE dbtx.idempotency_key_status as ENUM('in_progress', 'retryable', 'completed', 'failed');

-- Fencing tokens come from one sequence, not a per-key counter, so that a key
-- that is purged and claimed again never reuses a token that a stale worker
-- may still hold. Tokens also keep increasing for a key across purges.
create sequence if not exists dbtx.idempotency_fencing_token_seq;

create table if not exists dbtx.idempotency_keys
 (
  idempotency_key  text primary key,
  request          jsonb not null default 'null',
  status           dbtx.idempotency_key_status not null,
  fencing_token    bigint not null default nextval('dbtx.idempotency_fencing_token_seq'),
  lease_owner      text,         -- worker/instance id
  lease_expires_at timestamptz,  -- NULL once finished
  attempts         integer not null default 1,
  checkpoint       text not null default 'started',  -- optional recovery point
  checkpoint_data  jsonb not null default 'null',
  checkpoint_logs  jsonb not null default '[]',  -- previous checkpoints, oldest first
  response         jsonb not null default 'null',
  error            text,
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now(),
  completed_at     timestamptz,
  retry_after      timestamptz,
  expires_at       timestamptz not null default now() + interval '24 hours',  -- key TTL

  constraint lease_consistency
       check (status <> 'in_progress' or (lease_owner is not null and lease_expires_at is not null))
);

-- Find stuck work (expired leases) for a sweeper / monitoring.
create index if not exists idx_idem_expired_leases
   on dbtx.idempotency_keys(lease_expires_at)
where status = 'in_progress';

-- Cleanup of old keys.
create index if not exists idx_idem_expires_at
on dbtx.idempotency_keys(expires_at);

create or replace function dbtx.claim(p_key text, p_request jsonb,
                                      p_lease_owner text,
                                      p_lease_seconds float8,
                                      p_max_attempts int,
                                      p_ttl_seconds float8 default 86400)
 returns table(outcome text, fencing_token bigint, attempts integer,
               checkpoint text, checkpoint_data jsonb, response jsonb,
               error text, lease_expires_at timestamptz,
               retry_after timestamptz)
language plpgsql
      as $$
#variable_conflict use_column
declare
  r dbtx.idempotency_keys;
begin
  if current_setting('transaction_isolation') <> 'read committed' then
    raise exception 'dbtx.claim requires READ COMMITTED' using errcode = '25000';
  end if;

  for i in 1..5 loop
    outcome := null; fencing_token := null; attempts := null; checkpoint := null;
    checkpoint_data := null; response := null; error := null;
    lease_expires_at := null; retry_after := null;

    -- 1. Inspect: plain read, no row lock.
    select * into r from dbtx.idempotency_keys where idempotency_key = p_key;

    if found then
      if r.request is distinct from p_request then
        outcome := 'payload_mismatch';
        return next; return;
      end if;

      fencing_token    := r.fencing_token;
      attempts         := r.attempts;
      response         := r.response;
      error            := r.error;
      lease_expires_at := r.lease_expires_at;
      retry_after      := r.retry_after;

      if r.status = 'completed' then
        outcome := 'completed';
        return next; return;
      elsif r.status = 'failed' then
        outcome := 'failed';
        return next; return;
      elsif r.status = 'in_progress' and r.lease_expires_at > clock_timestamp() then
        outcome := 'in_progress';
        return next; return;
      end if;

      -- Row is retryable or has an expired lease. Exhaustion beats backoff.
      if r.attempts >= p_max_attempts then
        update dbtx.idempotency_keys
           set status           = 'failed',
               error            = coalesce(error, 'max attempts exhausted'),
               lease_owner      = null,
               lease_expires_at = null,
               retry_after      = null,
               completed_at     = clock_timestamp(),
               expires_at       = greatest(expires_at,
                                           clock_timestamp() + make_interval(secs => p_ttl_seconds)),
               updated_at       = clock_timestamp()
         where idempotency_key = p_key
           and fencing_token   = r.fencing_token
           and (status = 'retryable'
                or (status = 'in_progress' and lease_expires_at <= clock_timestamp()));
        if found then
          outcome          := 'exhausted';
          error            := coalesce(r.error, 'max attempts exhausted');
          lease_expires_at := null;
          retry_after      := null;
          return next; return;
        end if;
        continue;  -- state changed under us; re-inspect
      end if;

      if r.status = 'retryable' and r.retry_after > clock_timestamp() then
        outcome := 'backoff';
        return next; return;
      end if;
    end if;

    -- 2. Claim: row is absent or claimable. The WHERE remains the authority.
    insert into dbtx.idempotency_keys as ik
           (idempotency_key, request, status, fencing_token, lease_owner,
            lease_expires_at, attempts, expires_at)
    values (p_key, p_request, 'in_progress',
            nextval('dbtx.idempotency_fencing_token_seq'), p_lease_owner,
            clock_timestamp() + make_interval(secs => p_lease_seconds), 1,
            clock_timestamp() + make_interval(secs => p_ttl_seconds))
    on conflict (idempotency_key) do update
       set status           = 'in_progress',
           fencing_token    = nextval('dbtx.idempotency_fencing_token_seq'),
           lease_owner      = excluded.lease_owner,
           lease_expires_at = clock_timestamp() + make_interval(secs => p_lease_seconds),
           attempts         = ik.attempts + 1,
           error            = null,
           retry_after      = null,
           expires_at       = greatest(ik.expires_at,
                                       clock_timestamp() + make_interval(secs => p_ttl_seconds)),
           updated_at       = clock_timestamp()
     where ik.request  = excluded.request
       and ik.attempts < p_max_attempts
       and ((ik.status = 'in_progress' and ik.lease_expires_at <= clock_timestamp())
            or (ik.status = 'retryable'
                and (ik.retry_after is null or ik.retry_after <= clock_timestamp())))
    returning * into r;

    if found then
      outcome          := case when r.attempts = 1 then 'claimed' else 'resumed' end;
      fencing_token    := r.fencing_token;
      attempts         := r.attempts;
      checkpoint       := r.checkpoint;
      checkpoint_data  := r.checkpoint_data;
      lease_expires_at := r.lease_expires_at;
      return next; return;
    end if;
    -- Lost a race or the row was purged: loop and re-inspect.
  end loop;

  outcome := 'in_progress';
  fencing_token := null; attempts := null; checkpoint := null; checkpoint_data := null;
  response := null; error := null; lease_expires_at := null; retry_after := null;
  return next;
end;
$$;
