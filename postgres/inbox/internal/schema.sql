create schema if not exists dbtx;

-- Postgres has no "create type if not exists", and sqlc cannot parse a DO
-- block, so this statement fails if the schema is applied twice.
create type dbtx.inbox_status as enum('pending', 'processing', 'done', 'dead');

create table if not exists dbtx.inbox
 (
  id           uuid not null default uuidv7(),
  source       TEXT not null,  -- e.g. 'payments-service'
  message_id   TEXT not null,  -- e.g. the outbox id or event UUID
  message_type TEXT not null,  -- e.g. 'PaymentCaptured'
  aggregate_id TEXT,           -- optional, for per-entity ordering
  payload      JSONB not null,
  status       dbtx.inbox_status not null default 'pending',
  attempts     INT not null default 0,
  max_attempts INT not null default 10,
  available_at TIMESTAMPTZ not null default now(),
  locked_by    TEXT,
  last_error   TEXT,
  received_at  TIMESTAMPTZ not null default now(),
  processed_at TIMESTAMPTZ,
  updated_at   timestamptz not null default now(),

  constraint inbox_dedup unique (source, message_id)
);

create index if not exists inbox_ready_idx
   on dbtx.inbox(available_at, id)
where status in ('pending', 'processing');

-- Used by ClaimByAggregateID to find earlier unfinished messages.
create index if not exists inbox_aggregate_idx
   on dbtx.inbox(aggregate_id, id)
where status in ('pending', 'processing');
