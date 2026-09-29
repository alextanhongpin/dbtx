-- name: Nack :one
   update dbtx.outbox
      set last_error = sqlc.arg(last_error),
          retry_count = retry_count + 1,
          -- Emulate DLQ by setting max retry to -1 (no longer retryable).
          max_retry = case when sqlc.arg(dead)::boolean then -1 else max_retry end,
          visible_at = clock_timestamp() + sqlc.arg(delay_us)::bigint * interval '1 microsecond',
          updated_at = clock_timestamp()
    where id = sqlc.arg(id)
returning *;
