-- name: AcquireLease :one
SELECT dbtx.acquire_lease(sqlc.arg(namespace)::text, sqlc.arg(key)::text,
    sqlc.arg(owner_id)::text, sqlc.arg(ttl_us)::bigint)::bigint AS token;

-- name: RenewLease :one
SELECT dbtx.renew_lease(sqlc.arg(namespace)::text, sqlc.arg(key)::text,
    sqlc.arg(owner_id)::text, sqlc.arg(token)::bigint,
    sqlc.arg(ttl_us)::bigint)::boolean AS renewed;

-- name: ReleaseLease :execrows
with ownership_args as (
    select sqlc.arg(namespace)::text as "namespace",
           sqlc.arg(key)::text as "key",
           sqlc.narg(owner_id)::text as "owner_id",
           sqlc.arg(token)::bigint as "token"
)
select 1 where dbtx.ownership_release('dbtx.resource_ownership'::regclass,
    (select "key" from ownership_args), (select "namespace" from ownership_args),
    (select "owner_id" from ownership_args), (select "token" from ownership_args),
    null::text, false, true, true, '-infinity'::timestamptz, false, null::timestamptz);

-- name: LockLeaseRow :one
SELECT fencing_token FROM dbtx.leases
WHERE namespace = sqlc.arg(namespace) AND key = sqlc.arg(key)
FOR UPDATE;

-- name: ValidateLease :one
with ownership_args as (
    select sqlc.arg(namespace)::text as "namespace",
           sqlc.arg(key)::text as "key",
           sqlc.narg(owner_id)::text as "owner_id",
           sqlc.arg(token)::bigint as "token"
)
select dbtx.ownership_lock('dbtx.resource_ownership'::regclass,
    (select "key" from ownership_args), (select "namespace" from ownership_args),
    (select "owner_id" from ownership_args), (select "token" from ownership_args),
    null::text, true, true, true)::boolean as valid;

-- name: SetTransactionTimeouts :exec
SELECT set_config('statement_timeout', sqlc.arg(timeout)::text, true),
       set_config('lock_timeout', sqlc.arg(timeout)::text, true),
       set_config('idle_in_transaction_session_timeout', sqlc.arg(timeout)::text, true),
       set_config('transaction_timeout', sqlc.arg(timeout)::text, true);

-- name: ReleaseOwner :execrows
-- Used only after an ambiguous acquisition whose token was not received.
with ownership_args as (
    select sqlc.arg(namespace)::text as "namespace",
           sqlc.arg(key)::text as "key",
           sqlc.narg(owner_id)::text as "owner_id"
)
select 1 where dbtx.ownership_release('dbtx.resource_ownership'::regclass,
    (select "key" from ownership_args), (select "namespace" from ownership_args),
    (select "owner_id" from ownership_args), null::bigint,
    null::text, false, true, false, '-infinity'::timestamptz, false, null::timestamptz);
