create schema if not exists dbtx;

create TYPE dbtx.outbox_status as ENUM('pending', 'processing', 'done', 'dead');

create table if not exists dbtx.outbox
 (
  id             uuid not null default uuidv7(),
  aggregate_type text not null,
  aggregate_id   text not null,
  event_type     text not null,
  payload        jsonb not null,
  status         dbtx.outbox_status not null default 'pending',
  attempts       int not null default 0,
  max_attempts   int not null default 10,
  available_at   timestamptz not null default now(),
  locked_by      text,
  last_error     text,
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now(),
  processed_at   timestamptz,

  primary key (id),
  check (max_attempts > 0)
);

-- Only retryable messages are indexed, so finished messages do not slow down
-- Poll.
create index if not exists dbtx_outbox_ready_idx
   on dbtx.outbox(available_at, id)
where status in ('pending', 'processing');

-- Used by Purge and ListDead.
create index if not exists dbtx_outbox_finished_idx
   on dbtx.outbox(status, updated_at)
where status in ('done', 'dead');
