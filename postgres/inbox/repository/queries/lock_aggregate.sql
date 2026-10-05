-- name: LockAggregate :exec
-- Serialises the handlers of an aggregate until the transaction ends. Claim
-- already skips aggregates with a live lease; this also covers a handler that
-- is still running after its lease expired. The two-key form keeps the lock
-- apart from other advisory locks of the application.
select pg_advisory_xact_lock(hashtext('dbtx.inbox'), hashtext(sqlc.arg(aggregate_id)::text));
