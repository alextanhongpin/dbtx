-- Stop older workers before applying. Shared functions may serve other modules.
DROP FUNCTION IF EXISTS dbtx.jobs_retry(uuid, text, text, bigint, jsonb);
DROP VIEW IF EXISTS dbtx.jobs_retry_state;
