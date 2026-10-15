-- Stop older workers before applying. Shared functions may serve other modules.
DROP FUNCTION IF EXISTS dbtx.inbox_retry(uuid, text, text, jsonb);
DROP VIEW IF EXISTS dbtx.inbox_retry_state;
