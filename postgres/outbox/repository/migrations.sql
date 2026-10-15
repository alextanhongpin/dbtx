-- Stop older workers before applying. Shared functions may serve other modules.
DROP FUNCTION IF EXISTS dbtx.outbox_retry(uuid, text, text, jsonb);
DROP VIEW IF EXISTS dbtx.outbox_retry_state;
