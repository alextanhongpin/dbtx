-- Additive ownership adapter. Permanent resource rows and token histories stay local.
CREATE OR REPLACE VIEW dbtx.resource_ownership WITH (security_invoker = true) AS
SELECT namespace, key AS id, owner_id, fencing_token AS token, expires_at AS lease_expires_at FROM dbtx.leases;

CREATE OR REPLACE FUNCTION dbtx.acquire_lease(p_namespace text, p_key text, p_owner text, p_ttl_us bigint)
RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE v_row dbtx.leases%ROWTYPE;
BEGIN
    IF NOT pg_try_advisory_xact_lock(hashtextextended(json_build_array(p_namespace, p_key)::text, 726184921)) THEN RETURN 0; END IF;
    SELECT * INTO v_row FROM dbtx.leases WHERE namespace = p_namespace AND key = p_key FOR UPDATE NOWAIT;
    IF NOT FOUND THEN
        INSERT INTO dbtx.leases(namespace, key) VALUES(p_namespace, p_key);
    END IF;
    RETURN dbtx.ownership_grant('dbtx.resource_ownership'::regclass, p_key, p_namespace,
        p_owner, p_ttl_us * interval '1 microsecond', NULL::regclass, true, false, NULL);
EXCEPTION WHEN lock_not_available THEN RETURN 0;
END;
$$;

CREATE OR REPLACE FUNCTION dbtx.renew_lease(p_namespace text, p_key text, p_owner text, p_token bigint, p_ttl_us bigint)
RETURNS boolean LANGUAGE plpgsql AS $$
BEGIN
    RETURN dbtx.ownership_renew('dbtx.resource_ownership'::regclass, p_key, p_namespace,
        p_owner, p_token, NULL::text, true, true, true,
        p_ttl_us * interval '1 microsecond', false, NULL);
END;
$$;
