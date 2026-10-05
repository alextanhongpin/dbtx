-- name: ListDead :many
  select *
    from dbtx.outbox
   where status = 'dead'
order by updated_at, id
   limit $1;
