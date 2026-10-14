-- name: Create :one
-- (sqlc: verify it handles OUT parameters; otherwise declare the function as
--  RETURNS TABLE(...) or call it from plain SQL.)
SELECT
  o_id::uuid as id,
  o_status::dbtx.job_status as status,
  o_response::jsonb as response,
  coalesce(o_error, '')::text as error,
  o_payload_matches::bool as payload_matches,
  o_created::bool as created
FROM dbtx.create_job(
    sqlc.arg(idempotency_key)::text,
    sqlc.arg(request)::jsonb,
    sqlc.arg(delay_seconds)::float8,
    sqlc.arg(max_attempts)::int
);

-- -----------------------------------------------------------------------------
-- Claim
-- -----------------------------------------------------------------------------
-- Time handling: filters use NOW() because it is STABLE and can drive the
-- partial index scan (a volatile clock_timestamp() in a WHERE clause cannot be
-- used as an index condition and would force filtering many future rows).
-- Lease writes use clock_timestamp() so a long transaction cannot shorten them.
--
-- ORDER BY visible_at gives oldest-due-first fairness and uses the partial
-- index. MATERIALIZED stops the planner from folding the locking subquery
-- into the outer UPDATE, which could change which rows get locked.
-- name: FindAvailableJobs :many
with retry_args as (
    select sqlc.arg(worker_id)::text as worker_id,
           sqlc.arg(lease_seconds)::float8 as lease_seconds,
           sqlc.arg('limit')::int as "limit"
), picked AS MATERIALIZED (
    SELECT sub.id
    FROM dbtx.jobs sub
    WHERE sub.status IN ('queued', 'running', 'error')   -- 'running' here = expired lease
      AND sub.visible_at <= NOW()
      AND sub.attempts < sub.max_attempts
    ORDER BY sub.visible_at
    LIMIT (select "limit" from retry_args)
    FOR UPDATE SKIP LOCKED
)
SELECT claimed.* FROM picked
CROSS JOIN LATERAL dbtx.jobs_retry(picked.id, 'claim', (select worker_id from retry_args), null::bigint,
    jsonb_build_object('lease_seconds', (select lease_seconds from retry_args))) claimed;

-- -----------------------------------------------------------------------------
-- Lease renewal. Zero rows => lease lost (expired and reclaimed, or finished).
-- A worker whose lease merely expired but was NOT yet reclaimed can still
-- heartbeat/complete: the token is unchanged, so that is correct.
-- name: Heartbeat :one
with retry_args as (
    select sqlc.arg(lease_seconds)::float8 as lease_seconds,
           sqlc.arg(id)::uuid as id,
           sqlc.arg(fencing_token)::bigint as fencing_token,
           sqlc.arg(worker_id)::text as worker_id
)
SELECT transitioned.* FROM dbtx.jobs_retry((select id from retry_args), 'renew',
    (select worker_id from retry_args), (select fencing_token from retry_args),
    jsonb_build_object('lease_seconds', (select lease_seconds from retry_args))) transitioned;

-- -----------------------------------------------------------------------------
-- Terminal success. (worker_id guard is redundant with the per-claim token but
-- kept as defense in depth.)
-- name: Complete :one
with retry_args as (
    select sqlc.arg(response)::jsonb as response,
           sqlc.arg(id)::uuid as id,
           sqlc.arg(fencing_token)::bigint as fencing_token,
           sqlc.arg(worker_id)::text as worker_id
)
SELECT transitioned.* FROM dbtx.jobs_retry((select id from retry_args), 'complete',
    (select worker_id from retry_args), (select fencing_token from retry_args),
    jsonb_build_object('response', (select response from retry_args))) transitioned;

-- -----------------------------------------------------------------------------
-- Terminal failure (non-retryable). Sets processed_at to satisfy the CHECK.
-- name: Fail :one
with retry_args as (
    select sqlc.arg(error)::text as error,
           sqlc.arg(id)::uuid as id,
           sqlc.arg(fencing_token)::bigint as fencing_token,
           sqlc.arg(worker_id)::text as worker_id
)
SELECT transitioned.* FROM dbtx.jobs_retry((select id from retry_args), 'fail',
    (select worker_id from retry_args), (select fencing_token from retry_args),
    jsonb_build_object('error', (select error from retry_args))) transitioned;

-- -----------------------------------------------------------------------------
-- Retry with backoff. When attempts are exhausted the job becomes terminal
-- ('failed') instead of sitting in 'error' forever (the claim query would
-- never pick it up again). RHS column references see the OLD row.
-- name: Retry :one
with retry_args as (
    select sqlc.arg(backoff_seconds)::float8 as backoff_seconds,
           sqlc.arg(error)::text as error,
           sqlc.arg(id)::uuid as id,
           sqlc.arg(fencing_token)::bigint as fencing_token,
           sqlc.arg(worker_id)::text as worker_id
)
SELECT transitioned.* FROM dbtx.jobs_retry((select id from retry_args), 'nack',
    (select worker_id from retry_args), (select fencing_token from retry_args),
    jsonb_build_object('error', (select error from retry_args), 'delay_seconds', (select backoff_seconds from retry_args))) transitioned;

-- -----------------------------------------------------------------------------
-- Reaper: a worker that crashes on its LAST attempt leaves a 'running' row with
-- an expired lease and attempts = max_attempts. The claim query skips it
-- (attempts < max_attempts), so without this it would be stuck forever and
-- would also never be archived. Run periodically (e.g. every minute).
--
-- A worker whose lease merely expired can still complete (see Heartbeat), so
-- the lease must have expired grace_seconds ago, giving that worker time to
-- record its outcome.
-- name: ReapExhausted :execrows
WITH exhausted AS MATERIALIZED (
    SELECT id FROM dbtx.jobs
     WHERE status IN ('running', 'error')
       AND visible_at <= NOW() - (INTERVAL '1 second' * sqlc.arg(grace_seconds)::float8)
       AND attempts >= max_attempts
       FOR UPDATE
)
SELECT 1 FROM exhausted
CROSS JOIN LATERAL dbtx.jobs_retry(exhausted.id, 'expire', null::text, null::bigint,
    jsonb_build_object('keep_error', true, 'error', 'lease expired; max attempts exceeded')) transitioned;

-- -----------------------------------------------------------------------------
-- Archive finished jobs in bounded batches. Call repeatedly until it returns 0.
-- A single unbounded DELETE would hold locks, spike WAL and bloat the table.
-- DELETE + INSERT happen in one statement/transaction, so a row is always in
-- exactly one of jobs / jobs_archive. Columns are listed explicitly so a future
-- ALTER TABLE on one table cannot silently misalign the copy.
-- name: ArchiveOldJobs :one
WITH picked AS (
    SELECT id
    FROM dbtx.jobs
    WHERE status IN ('success', 'failed')
      AND updated_at < NOW() - (INTERVAL '1 second' * sqlc.arg(retention_seconds)::float8)
    ORDER BY updated_at
    LIMIT sqlc.arg(batch_size)::int
    FOR UPDATE SKIP LOCKED
),
moved AS (
    DELETE FROM dbtx.jobs j
    USING picked
    WHERE j.id = picked.id
    RETURNING j.*
),
inserted AS (
    INSERT INTO dbtx.jobs_archive (
        id, idempotency_key, request, response, status, worker_id, fencing_token,
        attempts, max_attempts, error, visible_at, processed_at, created_at, updated_at
    )
    SELECT
        id, idempotency_key, request, response, status, worker_id, fencing_token,
        attempts, max_attempts, error, visible_at, processed_at, created_at, updated_at
    FROM moved
    RETURNING 1
)
SELECT COUNT(*) FROM inserted;

-- -----------------------------------------------------------------------------
-- Expire idempotency keys after the window in which clients may retry.
-- Keep this window LONGER than job retention. After a key is purged, the same
-- key creates a new job, which is the intended end of the idempotency window.
-- name: PurgeOldJobKeys :execrows
DELETE FROM dbtx.job_keys
WHERE idempotency_key IN (
    SELECT k.idempotency_key
    FROM dbtx.job_keys k
    WHERE k.created_at < NOW() - (INTERVAL '1 second' * sqlc.arg(key_ttl_seconds)::float8)
      AND NOT EXISTS (SELECT 1 FROM dbtx.jobs j WHERE j.id = k.job_id)  -- never orphan a live job
    ORDER BY k.created_at
    LIMIT sqlc.arg(batch_size)::int
    FOR UPDATE SKIP LOCKED
);
