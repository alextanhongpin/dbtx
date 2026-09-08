create schema if not exists dbtx;

create unlogged table if not exists dbtx.cache(key text, value jsonb not null, digest text not null, created_at timestamptz not null default now(), updated_at timestamptz not null default now(), expires_at timestamptz, primary key (key));

create view dbtx.live_cache as
select key, value, digest, created_at, updated_at, expires_at
  from dbtx.cache
 where expires_at is null
    or expires_at >= clock_timestamp() with check OPTION;
