-- name: Requeue :one
   update dbtx.outbox
      set retry_at = $1, failure_reason = $2
    where id = $3
returning *;
