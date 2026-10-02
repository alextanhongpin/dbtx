create schema if not exists dbtx;

-- A row with a non-null lease is a placeholder held by LoadOrCreate while the
-- value is being computed. Its value is always 'null' and must not be exposed
-- to readers.
create unlogged table if not exists dbtx.cache(key text, value jsonb not null, lease uuid, created_at timestamptz not null default statement_timestamp(), updated_at timestamptz not null default statement_timestamp(), expires_at timestamptz, primary key (key));

alter table dbtx.cache add column if not exists lease uuid;

create or replace view dbtx.live_cache as
select key, value, created_at, updated_at, expires_at
  from dbtx.cache
 where lease is null
   and (expires_at is null or expires_at > statement_timestamp()) with check OPTION;
