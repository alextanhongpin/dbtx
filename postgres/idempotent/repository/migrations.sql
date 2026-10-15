-- Stop older workers before applying. Shared functions may serve other modules.
DROP FUNCTION IF EXISTS dbtx.idempotent_retry(text, text, bigint, jsonb);
DROP VIEW IF EXISTS dbtx.idempotent_retry_state;
