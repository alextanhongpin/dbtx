-- name: FulfillLease :execrows
-- Replaces the lease placeholder with the computed value. Affects no rows if
-- the lease expired, was taken over, or the key was deleted or overwritten.
update dbtx.cache
   set value = sqlc.arg(value)::jsonb,
       lease = null,
       expires_at = statement_timestamp() + sqlc.narg(ttl)::bigint * interval '1 microsecond',
       updated_at = statement_timestamp()
 where key = $1
   and lease = sqlc.arg(lease)::uuid
   and expires_at > statement_timestamp();
