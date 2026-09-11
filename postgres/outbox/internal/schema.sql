create schema if not exists dbtx;

create table if not exists dbtx.outbox
 (
  id             uuid not null default uuidv7(),
  aggregate_id   text not null check (aggregate_id <> ''),
  aggregate_type text not null check (aggregate_type <> ''),
  type           text not null check (type <> ''),
  payload        jsonb not null default '{}',
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now(),
  last_error     text not null default '',
  max_retry      int not null default 0,
  retry_count    int not null default 0 check (retry_count >= 0),
  visible_at     timestamptz not null default now(),

  primary key (id)
);

create index if not exists dbtx_outbox_visible_at on dbtx.outbox(visible_at);
