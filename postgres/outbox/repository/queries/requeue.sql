-- name: Requeue :execrows
select 1 from dbtx.outbox_retry(sqlc.arg(id)::uuid, 'requeue', null::text, '{}'::jsonb);
