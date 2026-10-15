-- name: LinkJobKey :one
WITH ins AS (
    INSERT INTO durable.idempotency_keys AS k (scope, key, request, status, execution_id, expires_at)
    VALUES (sqlc.arg(scope), sqlc.arg(key), sqlc.arg(request)::jsonb, 'linked', sqlc.arg(execution_id),
            clock_timestamp() + sqlc.arg(retention_us)::bigint * interval '1 microsecond')
    ON CONFLICT (scope, key) DO UPDATE
    SET request = EXCLUDED.request, status = 'linked', execution_id = EXCLUDED.execution_id,
        owner_id = NULL, token = 0, response = NULL, expires_at = EXCLUDED.expires_at,
        created_at = now(), updated_at = now()
    WHERE k.expires_at <= clock_timestamp()
    RETURNING 1)
SELECT EXISTS (SELECT 1 FROM ins)::boolean AS inserted;

-- name: GetJobKey :one
SELECT (request = sqlc.arg(request)::jsonb)::boolean AS same_request, status, execution_id
FROM durable.idempotency_keys WHERE scope = sqlc.arg(scope) AND key = sqlc.arg(key);

-- name: InsertExecution :exec
INSERT INTO durable.executions (id, queue, kind, scope, key, payload, payload_version,
    status, max_attempts, run_at)
VALUES (sqlc.arg(id), sqlc.arg(queue), sqlc.arg(kind), sqlc.narg(scope), sqlc.narg(key),
        sqlc.arg(payload), sqlc.arg(payload_version), 'pending', sqlc.arg(max_attempts),
        coalesce(sqlc.narg(run_at)::timestamptz, clock_timestamp()));

-- name: GetExecution :one
SELECT * FROM durable.executions WHERE id = sqlc.arg(id);

-- name: GetExecutionByKey :one
SELECT e.* FROM durable.executions e
JOIN durable.idempotency_keys k ON k.execution_id = e.id
WHERE k.scope = sqlc.arg(scope) AND k.key = sqlc.arg(key) AND k.status = 'linked';

-- name: CancelExecution :one
UPDATE durable.executions
SET status = CASE WHEN status = 'pending' THEN 'cancelled' ELSE status END,
    cancel_requested = (status = 'running'),
    finished_at = CASE WHEN status = 'pending' THEN now() ELSE finished_at END,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('pending', 'running')
RETURNING status;

-- name: ClaimExecutions :many
-- Candidates are filtered with now() so the partial indexes apply; the grant
-- re-checks them with clock_timestamp() under the row lock.
SELECT g.id::uuid AS id, g.token::bigint AS token, g.attempts::integer AS attempts
FROM durable.lease_grant('durable.executions', ARRAY(
        SELECT e.id FROM durable.executions e
        WHERE e.queue = sqlc.arg(queue)::text AND e.kind = sqlc.arg(kind)::text
          AND NOT e.cancel_requested
          AND ((e.status = 'pending' AND e.run_at <= now())
            OR (e.status = 'running' AND e.lease_expires_at <= now()
                AND e.attempts < e.max_attempts))
        ORDER BY e.run_at, e.id
        LIMIT sqlc.arg(lim)::integer
        FOR UPDATE SKIP LOCKED),
    sqlc.arg(owner_id)::text, sqlc.arg(ttl_us)::bigint, 'pending', 'running', true) g;

-- name: StartAttempt :one
-- Fixes the mode on the first claim (D8) and records the attempt.
WITH u AS (
    UPDATE durable.executions e SET mode = coalesce(e.mode, sqlc.arg(mode)::text)
    WHERE e.id = sqlc.arg(id) AND e.owner_id = sqlc.arg(owner_id) AND e.token = sqlc.arg(token)
    RETURNING e.*),
a AS (
    INSERT INTO durable.execution_attempts (execution_id, attempt, owner_id, token)
    SELECT u.id, u.attempts, u.owner_id, u.token FROM u
    ON CONFLICT (execution_id, attempt) DO UPDATE
    SET owner_id = EXCLUDED.owner_id, token = EXCLUDED.token, started_at = now(),
        finished_at = NULL, outcome = NULL, error = NULL)
SELECT u.id, u.queue, u.kind, u.scope, u.key, u.payload, u.payload_version, u.mode,
       u.attempts, u.max_attempts, u.token, u.checkpoint_name, u.checkpoint_data
FROM u;

-- name: ExpiredExecutions :many
-- Running executions whose lease expired on the final attempt, or that were
-- cancelled, among the kinds this worker registered.
SELECT e.id, e.owner_id, e.token, e.kind, e.cancel_requested, e.payload,
       coalesce(e.last_error, '')::text AS last_error, e.attempts
FROM durable.executions e
WHERE e.queue = sqlc.arg(queue)::text
  AND e.kind IN (SELECT jsonb_array_elements_text(sqlc.arg(kinds)::jsonb))
  AND e.status = 'running' AND e.lease_expires_at <= now()
  AND (e.attempts >= e.max_attempts OR e.cancel_requested)
LIMIT 100 FOR UPDATE SKIP LOCKED;

-- name: SetExecutionFinished :exec
-- Run after durable.lease_finish/lease_expire in the same transaction.
UPDATE durable.executions
SET finished_at = now(), output = coalesce(sqlc.narg(output)::jsonb, output)
WHERE id = sqlc.arg(id);

-- name: ExecutionCancelRequested :one
SELECT cancel_requested FROM durable.executions WHERE id = sqlc.arg(id);

-- name: ExecutionStatusToken :one
SELECT status, token FROM durable.executions WHERE id = sqlc.arg(id);

-- name: SaveCheckpoint :exec
-- Run after durable.lease_lock in the same transaction.
UPDATE durable.executions
SET checkpoint_name = sqlc.arg(name), checkpoint_data = sqlc.arg(data)::jsonb, updated_at = now()
WHERE id = sqlc.arg(id);

-- name: FinishAttempt :exec
UPDATE durable.execution_attempts
SET finished_at = now(), outcome = sqlc.arg(outcome), error = sqlc.narg(error)
WHERE execution_id = sqlc.arg(execution_id) AND attempt = sqlc.arg(attempt);

-- name: GetStep :one
SELECT status, request, response FROM durable.execution_steps
WHERE execution_id = sqlc.arg(execution_id) AND name = sqlc.arg(name);

-- name: InsertStep :execrows
-- Run after durable.lease_lock in the same transaction.
INSERT INTO durable.execution_steps (execution_id, name, remote_key, request, status)
VALUES (sqlc.arg(execution_id), sqlc.arg(name), sqlc.arg(remote_key), sqlc.arg(request)::jsonb, 'started')
ON CONFLICT (execution_id, name) DO NOTHING;

-- name: FinishStep :execrows
-- Run after durable.lease_lock in the same transaction.
UPDATE durable.execution_steps
SET status = 'done', response = sqlc.arg(response)::jsonb, updated_at = now()
WHERE execution_id = sqlc.arg(execution_id) AND name = sqlc.arg(name);

-- name: ListFailedExecutions :many
SELECT * FROM durable.executions
WHERE status = 'failed'
  AND (sqlc.arg(kind)::text = '' OR kind = sqlc.arg(kind)::text)
  AND (sqlc.arg(error_contains)::text = '' OR strpos(coalesce(last_error, ''), sqlc.arg(error_contains)::text) > 0)
  AND id > sqlc.arg(after)::uuid
ORDER BY id LIMIT sqlc.arg(lim)::integer;

-- name: RequeueExecution :execrows
UPDATE durable.executions
SET status = 'pending', max_attempts = attempts + max_attempts, run_at = clock_timestamp(),
    cancel_requested = false, finished_at = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('failed', 'cancelled');

-- name: RequeueFailedExecutions :execrows
UPDATE durable.executions
SET status = 'pending', max_attempts = attempts + max_attempts, run_at = clock_timestamp(),
    cancel_requested = false, finished_at = NULL, updated_at = now()
WHERE status = 'failed'
  AND (sqlc.arg(kind)::text = '' OR kind = sqlc.arg(kind)::text)
  AND (sqlc.arg(error_contains)::text = '' OR strpos(coalesce(last_error, ''), sqlc.arg(error_contains)::text) > 0);

-- name: OldestPayloadVersions :many
SELECT kind, min(payload_version)::integer AS version FROM durable.executions
WHERE status IN ('pending', 'running') GROUP BY kind;

-- name: PurgeExecutions :execrows
DELETE FROM durable.executions WHERE id IN (
    SELECT e.id FROM durable.executions e
    WHERE e.finished_at < sqlc.arg(cutoff)::timestamptz
      AND NOT EXISTS (SELECT 1 FROM durable.idempotency_keys k
                      WHERE k.execution_id = e.id AND k.expires_at > clock_timestamp())
    LIMIT sqlc.arg(lim)::integer FOR UPDATE SKIP LOCKED);
