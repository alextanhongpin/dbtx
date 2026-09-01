-- name: EnqueueDLQ :one
with deleted as (
  delete from dbtx.outbox o where o.id = $1 returning *
)
insert into dbtx.outbox_dlq(id, aggregate_id, aggregate_type, type, payload, created_at, failure_reason, retry_at, retry_count, run_at)
     select id,
            aggregate_id,
            aggregate_type,
            type,
            payload,
            created_at,
            failure_reason,
            retry_at,
            retry_count,
            run_at
       from deleted
  returning *;
