-- =============================================================================
-- dbtx: idempotent, retryable job queue with leases and fencing tokens
-- =============================================================================
-- Assumptions / contract
--   * PostgreSQL 18+ (uuidv7() and sha256() are built in).
--   * READ COMMITTED isolation (the default). Under REPEATABLE READ or
--     SERIALIZABLE, claim queries can fail with serialization errors.
--   * Every query below is meant to be run as its own autocommit statement.
--   * Delivery is AT-LEAST-ONCE. A worker that loses its lease may still have
--     performed side effects. Make downstream effects idempotent (use the job
--     id), or pass fencing_token downstream and reject stale tokens there.
--   * Workers MUST treat a zero-row result from Heartbeat / Complete / Fail /
--     Retry as "lease lost" and stop work immediately (sqlc :one => ErrNoRows).
--   * sqlc: each query uses ONE parameter style (all sqlc.arg). Mixing $1 and
--     sqlc.arg in one query is rejected by sqlc.
--
-- Status meanings (names kept from the original schema for compatibility):
--   queued  = queued, never claimed
--   running = leased by a worker, running
--   error   = failed attempt, waiting for backoff (retry_wait)
--   success / failed = terminal
-- =============================================================================

CREATE SCHEMA IF NOT EXISTS dbtx;

-- CREATE TYPE has no IF NOT EXISTS, so guard it to keep the migration re-runnable.
DO $$
BEGIN
    CREATE TYPE dbtx.job_status AS ENUM ('queued', 'running', 'success', 'error', 'failed');
EXCEPTION WHEN duplicate_object THEN
    NULL;
END
$$;

-- -----------------------------------------------------------------------------
-- Active jobs
-- -----------------------------------------------------------------------------
-- Hot table: every claim, heartbeat and completion UPDATEs a row, so we leave
-- free space in pages and make autovacuum aggressive to limit bloat. Because
-- visible_at is indexed and changes on every lease/heartbeat, those updates
-- are not HOT; expect index churn and keep heartbeats infrequent (~lease/3).
CREATE TABLE IF NOT EXISTS dbtx.jobs (
    id              UUID        NOT NULL DEFAULT uuidv7(),
    idempotency_key TEXT        NOT NULL,
    request         JSONB       NOT NULL DEFAULT '{}',
    response        JSONB       NOT NULL DEFAULT 'null',
    status          dbtx.job_status NOT NULL DEFAULT 'queued',
    worker_id       TEXT,                              -- NULL unless currently leased
    fencing_token   BIGINT      NOT NULL DEFAULT 0,    -- +1 on every claim
    attempts        INT         NOT NULL DEFAULT 0,
    max_attempts    INT         NOT NULL DEFAULT 10,
    error           TEXT,                              -- last failure reason (kept across re-claims)
    visible_at      TIMESTAMPTZ NOT NULL,              -- next time the job may be claimed / lease expiry
    processed_at    TIMESTAMPTZ,                       -- set when terminal
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (id),
    -- Safety net only. The authoritative idempotency record is dbtx.job_keys.
    -- (If several tenants share this table, scope the key per tenant there.)
    UNIQUE (idempotency_key),

    -- Make invalid states unrepresentable.
    CONSTRAINT jobs_attempts_within_max CHECK (attempts <= max_attempts),
    CONSTRAINT jobs_pending_has_worker  CHECK (status <> 'running' OR worker_id IS NOT NULL),
    CONSTRAINT jobs_terminal_processed  CHECK (status NOT IN ('success', 'failed') OR processed_at IS NOT NULL)
) WITH (fillfactor = 70,
        autovacuum_vacuum_scale_factor = 0.02,
        autovacuum_analyze_scale_factor = 0.02);

-- Claim path: only rows that can ever be claimed are indexed, ordered by visible_at.
CREATE INDEX IF NOT EXISTS idx_jobs_queue_polling
    ON dbtx.jobs (visible_at)
    WHERE status IN ('queued', 'running', 'error');

-- Archiver path: finds finished jobs without scanning the whole hot table.
CREATE INDEX IF NOT EXISTS idx_jobs_archivable
    ON dbtx.jobs (updated_at)
    WHERE status IN ('success', 'failed');

-- Keep updated_at correct no matter which query (or human) touches a row.
-- clock_timestamp() is wall-clock time, not transaction start time.
CREATE OR REPLACE FUNCTION dbtx.touch_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := clock_timestamp();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_jobs_touch_updated_at ON dbtx.jobs;
CREATE TRIGGER trg_jobs_touch_updated_at
    BEFORE UPDATE ON dbtx.jobs
    FOR EACH ROW EXECUTE FUNCTION dbtx.touch_updated_at();

-- -----------------------------------------------------------------------------
-- Archive of finished jobs
-- -----------------------------------------------------------------------------
-- Same columns as jobs. LIKE copies NOT NULL + defaults; we add a primary key.
-- Idempotency lookups go through dbtx.job_keys -> job id, so no key index here.
CREATE TABLE IF NOT EXISTS dbtx.jobs_archive (
    LIKE dbtx.jobs INCLUDING DEFAULTS,
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_archive_created_at ON dbtx.jobs_archive (created_at);

-- -----------------------------------------------------------------------------
-- Permanent idempotency record (never moved by the archiver)
-- -----------------------------------------------------------------------------
-- Why a separate table: checking "jobs, then archive, then insert" has a race
-- with the archiver. An in-flight archive DELETE can make a concurrent INSERT
-- wait on the unique index and then succeed once the delete commits, creating
-- a duplicate job even though the archive already holds the key. A table that
-- is never deleted from by the archiver removes the race entirely: whoever
-- inserts the key row first owns the job.
CREATE TABLE IF NOT EXISTS dbtx.job_keys (
    idempotency_key TEXT        NOT NULL,
    job_id          UUID        NOT NULL,
    request_hash    BYTEA       NOT NULL,   -- sha256 of the jsonb text; cheap payload comparison
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_job_keys_created_at ON dbtx.job_keys (created_at);

-- -----------------------------------------------------------------------------
-- Create (idempotent submit)
-- -----------------------------------------------------------------------------
-- Returns the existing job when the key was seen before, and flags whether the
-- request payload matches the original so the caller can reject mismatches.
-- Visibility is a server-side delay, not a client timestamp (no clock skew).
CREATE OR REPLACE FUNCTION dbtx.create_job(
    p_idempotency_key TEXT,
    p_request         JSONB,
    p_delay_seconds   FLOAT8 DEFAULT 0,
    p_max_attempts    INT    DEFAULT 10,
    OUT o_id              UUID,
    OUT o_status          dbtx.job_status,
    OUT o_response        JSONB,
    OUT o_error           TEXT,
    OUT o_payload_matches BOOLEAN,
    OUT o_created         BOOLEAN
) AS $$
DECLARE
    v_hash        BYTEA := sha256(convert_to(p_request::text, 'UTF8'));
    v_new_id      UUID  := uuidv7();
    v_existing_id UUID;
    v_key_hash    BYTEA;
    v_tries       INT   := 0;
BEGIN
    LOOP
        v_tries := v_tries + 1;
        IF v_tries > 5 THEN
            -- Only reachable if a key row exists with no job anywhere (corruption).
            RAISE EXCEPTION 'create_job: job for key % not found in jobs or archive', p_idempotency_key;
        END IF;

        -- 1. Claim the key. Concurrent creators block here until the winner
        --    commits, then see a conflict. The key row and the job row are
        --    inserted in the same transaction, so a committed key always has a job.
        INSERT INTO dbtx.job_keys (idempotency_key, job_id, request_hash)
        VALUES (p_idempotency_key, v_new_id, v_hash)
        ON CONFLICT (idempotency_key) DO NOTHING;

        IF FOUND THEN
            INSERT INTO dbtx.jobs (id, idempotency_key, request, visible_at, max_attempts)
            VALUES (v_new_id, p_idempotency_key, p_request,
                    clock_timestamp() + (INTERVAL '1 second' * p_delay_seconds),
                    p_max_attempts)
            RETURNING jobs.id, jobs.status, jobs.response, jobs.error
            INTO o_id, o_status, o_response, o_error;
            o_payload_matches := TRUE;
            o_created := TRUE;
            RETURN;
        END IF;

        -- 2. Key already exists: find the original job.
        o_created := FALSE;
        SELECT k.job_id, k.request_hash
          INTO v_existing_id, v_key_hash
          FROM dbtx.job_keys k
         WHERE k.idempotency_key = p_idempotency_key;

        IF NOT FOUND THEN
            CONTINUE;   -- key was purged between statements; try to claim it again
        END IF;

        o_payload_matches := (v_key_hash = v_hash);

        SELECT j.id, j.status, j.response, j.error
          INTO o_id, o_status, o_response, o_error
          FROM dbtx.jobs j
         WHERE j.id = v_existing_id;
        IF FOUND THEN RETURN; END IF;

        SELECT a.id, a.status, a.response, a.error
          INTO o_id, o_status, o_response, o_error
          FROM dbtx.jobs_archive a
         WHERE a.id = v_existing_id;
        IF FOUND THEN RETURN; END IF;

        -- Missed both: the archiver moved the row between our two reads.
        -- Each statement has a fresh snapshot, so just loop and look again.
    END LOOP;
END;
$$ LANGUAGE plpgsql;
