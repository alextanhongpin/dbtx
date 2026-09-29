create schema if not exists dbtx;

create table if not exists dbtx.idempotency_keys
 (
  scope      text,
  key        text,
  request    jsonb not null,
  response   jsonb not null,
  created_at timestamptz not null default now(),

  primary key (scope, key)
);

create index if not exists idempotency_keys_created_at_idx
on dbtx.idempotency_keys(created_at);
