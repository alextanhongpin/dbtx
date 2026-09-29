-- name: ListDead :many
  select *
    from dbtx.outbox
   where not (max_retry = 0 or retry_count < max_retry)
order by updated_at, id
   limit sqlc.arg(max_rows);

-- name: Requeue :one
   update dbtx.outbox
      set retry_count = 0,
          max_retry = sqlc.arg(max_retry),
          visible_at = clock_timestamp(),
          updated_at = clock_timestamp()
    where id = sqlc.arg(id)
      and not (max_retry = 0 or retry_count < max_retry)
returning *;

-- name: PurgeDead :execrows
delete from dbtx.outbox
 where not (max_retry = 0 or retry_count < max_retry)
   and updated_at < sqlc.arg(before);
