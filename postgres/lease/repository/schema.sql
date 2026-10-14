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
CREATE OR REPLACE FUNCTION dbtx.acquire_lease(
    p_namespace text, p_key text, p_owner text, p_ttl_us bigint
) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
    v_row dbtx.leases%ROWTYPE;
    v_now timestamptz;
BEGIN
    IF NOT pg_try_advisory_xact_lock(hashtextextended(
        json_build_array(p_namespace, p_key)::text, 726184921)) THEN
        RETURN 0;
    END IF;
    SELECT * INTO v_row FROM dbtx.leases
    WHERE namespace = p_namespace AND key = p_key FOR UPDATE NOWAIT;
    IF NOT FOUND THEN
        -- All cooperating initializers hold the advisory gate. Avoid an
        -- INSERT conflict check on an existing row held by a transaction.
        INSERT INTO dbtx.leases (namespace, key)
        VALUES (p_namespace, p_key) RETURNING * INTO v_row;
    END IF;
    v_now := clock_timestamp();
    IF v_row.owner_id IS NOT NULL AND v_row.expires_at > v_now THEN
        RETURN 0;
    END IF;
    IF v_row.fencing_token = 9223372036854775807 THEN
        RAISE EXCEPTION 'lease fencing token exhausted' USING ERRCODE = '22003';
    END IF;
    UPDATE dbtx.leases SET owner_id = p_owner,
        fencing_token = v_row.fencing_token + 1,
        expires_at = v_now + p_ttl_us * interval '1 microsecond'
    WHERE namespace = p_namespace AND key = p_key;
    RETURN v_row.fencing_token + 1;
EXCEPTION WHEN lock_not_available THEN
    RETURN 0;
END;
$$;

-- Check expiry AFTER acquiring the row, so lock waiting cannot resurrect a
-- lease whose validity was checked using a stale statement timestamp.
CREATE OR REPLACE FUNCTION dbtx.renew_lease(
    p_namespace text, p_key text, p_owner text, p_token bigint, p_ttl_us bigint
) RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE
    v_row dbtx.leases%ROWTYPE;
    v_now timestamptz;
BEGIN
    SELECT * INTO v_row FROM dbtx.leases
    WHERE namespace = p_namespace AND key = p_key FOR UPDATE;
    v_now := clock_timestamp();
    IF NOT FOUND OR v_row.owner_id IS DISTINCT FROM p_owner
        OR v_row.fencing_token <> p_token OR v_row.expires_at <= v_now THEN
        RETURN false;
    END IF;
    UPDATE dbtx.leases
    SET expires_at = v_now + p_ttl_us * interval '1 microsecond'
    WHERE namespace = p_namespace AND key = p_key;
    RETURN true;
END;
$$;
