create schema if not exists dbtx;

create table if not exists dbtx.outbox
 (
  id             uuid not null default uuidv7(),
  aggregate_id   text not null,
  aggregate_type text not null,
  type           text not null,
  payload        jsonb not null default '{}',
  created_at     timestamptz not null default now(),

  primary key (id)
);
