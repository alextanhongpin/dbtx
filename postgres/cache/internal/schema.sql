create schema if not exists dbtx;

create UNLOGGED table if not exists dbtx.cache(key text, value jsonb not null, digest text not null, created_at timestamptz not null default now(), updated_at timestamptz not null default now(), expires_at timestamptz, primary key (key));
