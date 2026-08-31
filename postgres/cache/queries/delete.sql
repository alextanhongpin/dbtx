-- name: DeleteMany :many
delete from dbtx.cache where key = ANY($1::text[]) returning *;
