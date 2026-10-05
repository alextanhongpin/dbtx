-- name: RenewLease :execrows
update dbtx.cache
   set expires_at = statement_timestamp() + sqlc.narg(ttl)::bigint * interval '1 microsecond',
       updated_at = statement_timestamp()
 where key = $1
   and lease = sqlc.arg(lease)::uuid
   and expires_at > statement_timestamp();
