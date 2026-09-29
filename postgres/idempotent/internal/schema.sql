create schema if not exists dbtx;

create table if not exists dbtx.idempotency_keys
 (
  key        text,
  request    jsonb not null,
  response   jsonb not null,
  created_at timestamptz not null default now(),

  primary key (key)
);
