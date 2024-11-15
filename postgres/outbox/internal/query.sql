-- name: Create :exec
INSERT INTO outbox (
	aggregate_id,
	aggregate_type,
	type,
	payload
) VALUES (
	UNNEST(@aggregate_ids::text[]),
	UNNEST(@aggregate_types::text[]),
	UNNEST(@types::text[]),
	UNNEST(@payloads::text[])::jsonb
);

-- name: Delete :one
DELETE FROM outbox
WHERE id = (
	SELECT id
	FROM outbox
	ORDER BY id
	FOR UPDATE
	SKIP LOCKED
	LIMIT 1
)
RETURNING *;

-- name: Count :one
SELECT COUNT(*)
FROM outbox;
