-- Shared lease transitions (see the generic functions in schema.sql). tbl is
-- the table name, e.g. 'durable.executions'; running_status is the status a
-- held row has. Each feature's claim query calls durable.lease_grant itself,
-- because candidate selection is feature specific.

-- name: LeaseRenew :one
SELECT durable.lease_renew(sqlc.arg(tbl)::text::regclass, sqlc.arg(id)::uuid,
    sqlc.arg(owner_id)::text, sqlc.arg(token)::bigint,
    sqlc.arg(running_status)::text, sqlc.arg(ttl_us)::bigint)::boolean AS renewed;

-- name: LeaseLock :one
SELECT durable.lease_lock(sqlc.arg(tbl)::text::regclass, sqlc.arg(id)::uuid,
    sqlc.arg(owner_id)::text, sqlc.arg(token)::bigint,
    sqlc.arg(running_status)::text, sqlc.arg(require_unexpired)::boolean)::boolean AS held;

-- name: LeaseFinish :one
SELECT durable.lease_finish(sqlc.arg(tbl)::text::regclass, sqlc.arg(id)::uuid,
    sqlc.arg(owner_id)::text, sqlc.arg(token)::bigint,
    sqlc.arg(running_status)::text, sqlc.arg(require_unexpired)::boolean,
    sqlc.arg(status)::text, sqlc.narg(delay_us)::bigint, sqlc.narg(last_error)::text,
    sqlc.arg(attempt_delta)::integer)::boolean AS finished;

-- name: LeaseExpire :one
SELECT durable.lease_expire(sqlc.arg(tbl)::text::regclass, sqlc.arg(id)::uuid,
    sqlc.arg(owner_id)::text, sqlc.arg(token)::bigint,
    sqlc.arg(running_status)::text, sqlc.arg(status)::text,
    sqlc.arg(reason)::text)::boolean AS expired;

-- name: LeaseContract :many
SELECT missing::text FROM durable.lease_contract(sqlc.arg(tbl)::text::regclass,
    sqlc.arg(full_contract)::boolean) AS missing;

-- name: SetLocalTimeouts :exec
SELECT durable.set_local_timeouts(sqlc.arg(timeout)::text);

-- name: ClockTimestamp :one
SELECT clock_timestamp()::timestamptz AS now;

-- name: NextToken :one
SELECT nextval('durable.fencing_seq')::bigint AS token;

-- Resource leases (package lease) use per-key generations instead.

-- name: AcquireResourceLease :one
SELECT durable.acquire_lease(sqlc.arg(namespace)::text, sqlc.arg(key)::text,
    sqlc.arg(owner_id)::text, sqlc.arg(ttl_us)::bigint)::bigint AS token;

-- name: RenewResourceLease :one
SELECT durable.renew_lease(sqlc.arg(namespace)::text, sqlc.arg(key)::text,
    sqlc.arg(owner_id)::text, sqlc.arg(token)::bigint,
    sqlc.arg(ttl_us)::bigint)::boolean AS renewed;

-- name: ReleaseResourceLease :execrows
UPDATE durable.leases SET owner_id = NULL, expires_at = NULL
WHERE namespace = sqlc.arg(namespace) AND key = sqlc.arg(key)
  AND owner_id = sqlc.arg(owner_id) AND token = sqlc.arg(token);

-- name: ReleaseResourceOwner :execrows
-- Used only after an acquisition whose response was lost.
UPDATE durable.leases SET owner_id = NULL, expires_at = NULL
WHERE namespace = sqlc.arg(namespace) AND key = sqlc.arg(key)
  AND owner_id = sqlc.arg(owner_id);

-- name: LockResourceLease :one
SELECT (owner_id IS NOT DISTINCT FROM sqlc.arg(owner_id)::text AND token = sqlc.arg(token)::bigint
        AND expires_at > clock_timestamp())::boolean AS held
FROM durable.leases WHERE namespace = sqlc.arg(namespace) AND key = sqlc.arg(key)
FOR UPDATE;

-- Transaction-scoped advisory locks (package txlock).

-- name: TryAdvisoryXactLock :one
SELECT pg_try_advisory_xact_lock(hashtextextended(sqlc.arg(key)::text, 0))::boolean AS locked;

-- name: AdvisoryXactLock :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(key)::text, 0));

-- name: CurrentLockTimeout :one
SELECT current_setting('lock_timeout')::text AS lock_timeout;

-- name: SetLocalLockTimeout :exec
SELECT set_config('lock_timeout', sqlc.arg(timeout)::text, true);
