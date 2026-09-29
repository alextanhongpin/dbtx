-- name: Update :one
   update dbtx.outbox
      set last_error = sqlc.arg(last_error),
          visible_at = sqlc.arg(visible_at),
          max_retry = sqlc.arg(max_retry),
          retry_count = sqlc.arg(retry_count),
          updated_at = clock_timestamp()
    where id = sqlc.arg(id)
returning *;
