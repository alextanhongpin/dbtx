-- Shared ownership of module-owned rows. No resource/work rows live here.
CREATE SCHEMA IF NOT EXISTS dbtx;

-- Accept the lease columns directly or the retry facade's existing aliases.
CREATE OR REPLACE FUNCTION dbtx.ownership_column(p_table regclass, p_name text, p_alias text)
RETURNS text LANGUAGE plpgsql STABLE AS $$
DECLARE v_name text;
BEGIN
    SELECT quote_ident(attname) INTO v_name FROM pg_attribute
     WHERE attrelid=p_table AND attname=ANY(ARRAY[p_name,p_alias]) AND attnum>0 AND NOT attisdropped
     ORDER BY (attname=p_name) DESC LIMIT 1;
    IF v_name IS NULL THEN
        RAISE EXCEPTION 'ownership: % lacks %/%',p_table,p_name,p_alias USING ERRCODE='22023';
    END IF;
    RETURN v_name;
END;
$$;

-- Native key comparisons preserve the base table's index, including composite
-- (namespace, key) resource indexes exposed as (namespace, id) by a view.
CREATE OR REPLACE FUNCTION dbtx.ownership_key(p_table regclass, p_namespace text)
RETURNS text LANGUAGE plpgsql STABLE AS $$
DECLARE v_type text; v_id text;
BEGIN
    SELECT quote_ident(attname),format_type(atttypid,atttypmod) INTO v_id,v_type FROM pg_attribute
     WHERE attrelid=p_table AND attname=ANY(ARRAY['id','retry_key']) AND attnum>0 AND NOT attisdropped
     ORDER BY (attname='id') DESC LIMIT 1;
    IF v_type IS NULL THEN
        RAISE EXCEPTION 'ownership: % has no id column', p_table USING ERRCODE = '22023';
    END IF;
    RETURN format('%s = $1::%s%s', v_id,v_type,
        CASE WHEN p_namespace IS NULL THEN '' ELSE ' AND namespace = $2' END);
END;
$$;

-- Read raw values before evaluating expiry. An expression in the SELECT list
-- can be evaluated before FOR UPDATE waits; the caller checks the clock AFTER
-- this function has acquired the row lock. The lock lasts until transaction end.
CREATE OR REPLACE FUNCTION dbtx.ownership_row(
    p_table regclass, p_id text, p_namespace text, p_status boolean, p_nowait boolean
) RETURNS TABLE(owner_id text, token bigint, lease_expires_at timestamptz, status text)
LANGUAGE plpgsql VOLATILE AS $$
BEGIN
    RETURN QUERY EXECUTE format('SELECT %s, %s, lease_expires_at, %s
        FROM %s WHERE %s FOR UPDATE %s',
        dbtx.ownership_column(p_table,'owner_id','lease_owner'), dbtx.ownership_column(p_table,'token','fencing_token'),
        CASE WHEN p_status THEN 'status::text' ELSE 'NULL::text' END,
        p_table, dbtx.ownership_key(p_table, p_namespace),
        CASE WHEN p_nowait THEN 'NOWAIT' ELSE '' END)
    USING p_id, p_namespace;
END;
$$;

CREATE OR REPLACE FUNCTION dbtx.ownership_lock(
    p_table regclass, p_id text, p_namespace text, p_owner text, p_token bigint,
    p_status text, p_require_unexpired boolean, p_check_owner boolean, p_check_token boolean
) RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
DECLARE v record;
BEGIN
    SELECT * INTO v FROM dbtx.ownership_row(p_table, p_id, p_namespace, p_status IS NOT NULL, false);
    IF NOT FOUND THEN RETURN false; END IF;
    RETURN (NOT p_check_owner OR (p_owner IS NOT NULL AND v.owner_id IS NOT DISTINCT FROM p_owner))
       AND (NOT p_check_token OR (p_token IS NOT NULL AND v.token IS NOT DISTINCT FROM p_token))
       AND (p_status IS NULL OR v.status IS NOT DISTINCT FROM p_status)
       AND (NOT p_require_unexpired OR coalesce(v.lease_expires_at > clock_timestamp(), false));
END;
$$;

-- The caller selects eligible work and/or initializes a persistent resource
-- row in this same transaction. Grant never changes domain status or attempts.
-- NULL sequence means a permanent per-key generation; a sequence preserves
-- token history for purgeable work. Unfenced messages retain their owner guard.
CREATE OR REPLACE FUNCTION dbtx.ownership_grant(
    p_table regclass, p_id text, p_namespace text, p_owner text, p_ttl interval,
    p_sequence regclass, p_fenced boolean, p_touch boolean, p_write_at timestamptz
) RETURNS bigint LANGUAGE plpgsql VOLATILE AS $$
DECLARE v record; v_token bigint; v_now timestamptz;
BEGIN
    SELECT * INTO v FROM dbtx.ownership_row(p_table, p_id, p_namespace, false, true);
    IF NOT FOUND THEN RETURN 0; END IF;
    v_now := clock_timestamp();
    IF v.owner_id IS NOT NULL AND v.lease_expires_at > v_now THEN RETURN 0; END IF;
    IF p_owner IS NULL OR p_ttl IS NULL THEN
        RAISE EXCEPTION 'ownership: grant needs owner and TTL' USING ERRCODE = '22023';
    END IF;
    IF NOT p_fenced THEN v_token := 1;
    ELSIF p_sequence IS NOT NULL THEN v_token := nextval(p_sequence);
    ELSE
        IF v.token = 9223372036854775807 THEN
            RAISE EXCEPTION 'lease fencing token exhausted' USING ERRCODE = '22003';
        END IF;
        v_token := v.token + 1;
    END IF;
    EXECUTE format('UPDATE %s SET %s = $3, lease_expires_at = $4 %s %s WHERE %s',
        p_table, dbtx.ownership_column(p_table,'owner_id','lease_owner'),
        CASE WHEN p_fenced THEN ', '||dbtx.ownership_column(p_table,'token','fencing_token')||' = $5' ELSE '' END,
        CASE WHEN p_touch THEN ', updated_at = $6' ELSE '' END,
        dbtx.ownership_key(p_table, p_namespace))
    USING p_id, p_namespace, p_owner, coalesce(p_write_at, v_now) + p_ttl, v_token,
          coalesce(p_write_at, v_now);
    RETURN v_token;
EXCEPTION WHEN lock_not_available THEN RETURN 0;
END;
$$;

CREATE OR REPLACE FUNCTION dbtx.ownership_renew(
    p_table regclass, p_id text, p_namespace text, p_owner text, p_token bigint,
    p_status text, p_require_unexpired boolean, p_check_owner boolean, p_check_token boolean,
    p_ttl interval, p_touch boolean, p_write_at timestamptz
) RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
DECLARE v_now timestamptz;
BEGIN
    IF NOT dbtx.ownership_lock(p_table, p_id, p_namespace, p_owner, p_token,
        p_status, p_require_unexpired, p_check_owner, p_check_token) THEN RETURN false; END IF;
    v_now := coalesce(p_write_at, clock_timestamp());
    EXECUTE format('UPDATE %s SET lease_expires_at = $3 %s WHERE %s',
        p_table, CASE WHEN p_touch THEN ', updated_at = $4' ELSE '' END,
        dbtx.ownership_key(p_table, p_namespace))
    USING p_id, p_namespace, v_now + p_ttl, v_now;
    RETURN true;
END;
$$;

-- Release is ownership-only. Rows whose constraints couple status and ownership
-- must instead clear ownership with their domain outcome in a single UPDATE,
-- after ownership_lock. p_expiry preserves NULL vs -infinity resource storage.
CREATE OR REPLACE FUNCTION dbtx.ownership_release(
    p_table regclass, p_id text, p_namespace text, p_owner text, p_token bigint,
    p_status text, p_require_unexpired boolean, p_check_owner boolean, p_check_token boolean,
    p_expiry timestamptz, p_touch boolean, p_write_at timestamptz
) RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
BEGIN
    IF NOT dbtx.ownership_lock(p_table, p_id, p_namespace, p_owner, p_token,
        p_status, p_require_unexpired, p_check_owner, p_check_token) THEN RETURN false; END IF;
    EXECUTE format('UPDATE %s SET %s = NULL, lease_expires_at = $3 %s WHERE %s',
        p_table, dbtx.ownership_column(p_table,'owner_id','lease_owner'), CASE WHEN p_touch THEN ', updated_at = $4' ELSE '' END,
        dbtx.ownership_key(p_table, p_namespace))
    USING p_id, p_namespace, p_expiry, coalesce(p_write_at, clock_timestamp());
    RETURN true;
END;
$$;
