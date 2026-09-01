create schema if not exists dbtx;

create table if not exists dbtx.outbox
 (
  id             uuid not null default uuidv7(),
  aggregate_id   text not null,
  aggregate_type text not null,
  type           text not null,
  payload        jsonb not null default '{}',
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now(),
  failure_reason text not null default '',
  retry_at       timestamptz,
  retry_count    int not null default 0,
  run_at         timestamptz,

  primary key (id)
);

create table if not exists dbtx.outbox_dlq
 (
  id             uuid not null default uuidv7(),
  aggregate_id   text not null,
  aggregate_type text not null,
  type           text not null,
  payload        jsonb not null default '{}',
  created_at     timestamptz not null default now(),
  failure_reason text not null default '',
  retry_at       timestamptz,
  retry_count    int not null default 0,
  run_at         timestamptz,

  primary key (id)
);
