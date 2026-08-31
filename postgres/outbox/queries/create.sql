with inserted as (
    insert into dbtx.outbox(aggregate_id, aggregate_type, type, payload)
         select *
           from jsonb_to_recordset($1::jsonb) as t(aggregate_id text, aggregate_type text, type text, payload jsonb)
      returning *
)
select json_agg(t) from inserted t;
