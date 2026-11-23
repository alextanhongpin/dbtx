-- keys: attempt counting with an exhausted state, backoff between attempts,
-- and checkpoints for multi-step requests.

ALTER TABLE durable.idempotency_keys
    ADD COLUMN IF NOT EXISTS attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN IF NOT EXISTS retry_at        timestamptz,
    ADD COLUMN IF NOT EXISTS last_error      text,
    ADD COLUMN IF NOT EXISTS checkpoint_name text    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS checkpoint_data jsonb;

ALTER TABLE durable.idempotency_keys DROP CONSTRAINT IF EXISTS idempotency_keys_status_check;
ALTER TABLE durable.idempotency_keys ADD CONSTRAINT idempotency_keys_status_check
    CHECK (status IN ('in_flight', 'completed', 'failed', 'linked', 'exhausted'));

-- In-flight keys from before this migration count as one attempt.
UPDATE durable.idempotency_keys SET attempts = 1 WHERE status = 'in_flight' AND attempts = 0;

DROP FUNCTION IF EXISTS durable.begin_key(text, text, jsonb, text, bigint, bigint);

-- Returns outcome: acquired, in_flight, backoff, completed, failed, linked,
-- exhausted, mismatch. Every grant counts an attempt; a key whose holder
-- crashed after using the last attempt becomes exhausted instead of being
-- granted again. An acquired outcome carries the last saved checkpoint.
CREATE OR REPLACE FUNCTION durable.begin_key(
    p_scope text, p_key text, p_request jsonb, p_owner text,
    p_lease_us bigint, p_retention_us bigint, p_max_attempts integer
) RETURNS TABLE (outcome text, key_id uuid, token bigint, response jsonb, execution_id uuid,
                 attempts integer, retry_at timestamptz, last_error text,
                 checkpoint_name text, checkpoint_data jsonb)
LANGUAGE plpgsql AS $$
#variable_conflict use_column
DECLARE
    v_row durable.idempotency_keys%ROWTYPE;
    v_now timestamptz := clock_timestamp();
    v_token bigint;
    v_id uuid;
    v_error text;
BEGIN
    LOOP
        SELECT * INTO v_row FROM durable.idempotency_keys k
        WHERE k.scope = p_scope AND k.key = p_key FOR UPDATE;
        v_now := clock_timestamp();
        IF NOT FOUND OR v_row.expires_at <= v_now THEN
            v_token := nextval('durable.fencing_seq');
            BEGIN
                INSERT INTO durable.idempotency_keys AS k (scope, key, request, status,
                    owner_id, token, lease_expires_at, expires_at, attempts)
                VALUES (p_scope, p_key, p_request, 'in_flight', p_owner, v_token,
                    v_now + p_lease_us * interval '1 microsecond',
                    v_now + p_retention_us * interval '1 microsecond', 1)
                ON CONFLICT ON CONSTRAINT idempotency_keys_pkey DO UPDATE
                SET request = EXCLUDED.request, status = 'in_flight',
                    owner_id = EXCLUDED.owner_id, token = EXCLUDED.token,
                    lease_expires_at = EXCLUDED.lease_expires_at, response = NULL,
                    execution_id = NULL, expires_at = EXCLUDED.expires_at,
                    attempts = 1, retry_at = NULL, last_error = NULL,
                    checkpoint_name = '', checkpoint_data = NULL,
                    created_at = now(), updated_at = now()
                WHERE k.expires_at <= clock_timestamp()
                RETURNING k.id INTO v_id;
                IF FOUND THEN
                    RETURN QUERY SELECT 'acquired'::text, v_id, v_token, NULL::jsonb, NULL::uuid,
                        1, NULL::timestamptz, NULL::text, ''::text, NULL::jsonb;
                    RETURN;
                END IF;
            EXCEPTION WHEN unique_violation THEN
                NULL; -- lost an insert race; read the winner's row
            END;
            CONTINUE;
        END IF;
        IF v_row.request <> p_request THEN
            RETURN QUERY SELECT 'mismatch'::text, v_row.id, 0::bigint, NULL::jsonb, NULL::uuid,
                0, NULL::timestamptz, NULL::text, ''::text, NULL::jsonb;
            RETURN;
        END IF;
        IF v_row.status IN ('completed', 'failed', 'linked', 'exhausted') THEN
            RETURN QUERY SELECT v_row.status, v_row.id, v_row.token, v_row.response,
                v_row.execution_id, v_row.attempts, NULL::timestamptz, v_row.last_error,
                ''::text, NULL::jsonb;
            RETURN;
        END IF;
        IF v_row.lease_expires_at > v_now THEN
            RETURN QUERY SELECT 'in_flight'::text, v_row.id, 0::bigint, NULL::jsonb, NULL::uuid,
                v_row.attempts, NULL::timestamptz, NULL::text, ''::text, NULL::jsonb;
            RETURN;
        END IF;
        -- No live owner. A set owner_id means the holder's lease expired.
        IF v_row.attempts >= p_max_attempts THEN
            v_error := concat_ws(': ',
                CASE WHEN v_row.owner_id IS NOT NULL THEN 'lease expired' END, v_row.last_error);
            UPDATE durable.idempotency_keys k
            SET status = 'exhausted', owner_id = NULL, lease_expires_at = NULL,
                last_error = v_error,
                expires_at = v_now + p_retention_us * interval '1 microsecond',
                updated_at = now()
            WHERE k.id = v_row.id;
            RETURN QUERY SELECT 'exhausted'::text, v_row.id, v_row.token, NULL::jsonb, NULL::uuid,
                v_row.attempts, NULL::timestamptz, v_error, ''::text, NULL::jsonb;
            RETURN;
        END IF;
        IF v_row.retry_at > v_now THEN
            RETURN QUERY SELECT 'backoff'::text, v_row.id, 0::bigint, NULL::jsonb, NULL::uuid,
                v_row.attempts, v_row.retry_at, v_row.last_error, ''::text, NULL::jsonb;
            RETURN;
        END IF;
        v_token := nextval('durable.fencing_seq');
        UPDATE durable.idempotency_keys k
        SET owner_id = p_owner, token = v_token,
            lease_expires_at = v_now + p_lease_us * interval '1 microsecond',
            attempts = k.attempts + 1, retry_at = NULL, updated_at = now()
        WHERE k.id = v_row.id;
        RETURN QUERY SELECT 'acquired'::text, v_row.id, v_token, NULL::jsonb, NULL::uuid,
            v_row.attempts + 1, NULL::timestamptz, v_row.last_error,
            v_row.checkpoint_name, v_row.checkpoint_data;
        RETURN;
    END LOOP;
END;
$$;
