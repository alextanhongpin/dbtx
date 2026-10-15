CREATE SCHEMA IF NOT EXISTS dbtx;

-- Rows are permanent fencing history, not disposable expired work.
CREATE TABLE IF NOT EXISTS dbtx.leases (
    namespace text NOT NULL,
    key text NOT NULL,
    owner_id text,
    fencing_token bigint NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
    expires_at timestamptz NOT NULL DEFAULT '-infinity',
    PRIMARY KEY (namespace, key),
    CHECK (namespace <> '' AND key <> '')
);

-- The advisory gate makes concurrent first INSERTs nonwaiting too. Hash
-- collisions can cause brief spurious contention, never grant two owners.
-- NOWAIT also detects a fenced transaction holding the persistent row.
-- dbtx.acquire_lease is installed by ownership.sql.

-- Check expiry AFTER acquiring the row, so lock waiting cannot resurrect a
-- lease whose validity was checked using a stale statement timestamp.
-- dbtx.renew_lease is installed by ownership.sql.
