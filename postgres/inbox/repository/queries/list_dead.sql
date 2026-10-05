-- name: ListDead :many
  select *
    from dbtx.inbox
   where status = 'dead'
order by updated_at, id
   limit $1;
