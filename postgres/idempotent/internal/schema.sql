create schema if not exists dbtx;

create TYPE dbtx.idempotency_key_status as ENUM('in_progress', 'retryable', 'completed', 'failed');

create table if not exists dbtx.idempotency_keys
 (
  idempotency_key  text primary key,
  request          jsonb not null default 'null',
  status           dbtx.idempotency_key_status not null,
  fencing_token    bigint not null default 1,
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
