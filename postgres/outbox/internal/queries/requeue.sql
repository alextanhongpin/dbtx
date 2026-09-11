-- name: Requeue :one
   update dbtx.outbox
      set last_error = sqlc.arg(last_error),
          visible_at = sqlc.arg(visible_at),
          max_retry = coalesce(
            nullif(sqlc.arg(max_retry)::int, 0), dbtx.outbox.max_retry
          ),
          updated_at = clock_timestamp()
    where id = sqlc.arg(id)
returning *;
