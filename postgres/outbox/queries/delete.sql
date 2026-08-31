with deleted as (
       delete
         from dbtx.outbox
        where id = (
    select id from dbtx.outbox order by id for update SKIP LOCKED limit 1
  )
    returning *
)
select row_to_json(t) from deleted t;
