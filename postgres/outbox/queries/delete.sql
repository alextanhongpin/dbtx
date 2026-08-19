WITH deleted AS (
	DELETE FROM outbox.messages
	WHERE id = (
		SELECT id
		FROM outbox.messages
		ORDER BY id
		FOR UPDATE
		SKIP LOCKED
		LIMIT 1
	)
	RETURNING *
)
SELECT row_to_json(t)
FROM deleted t;
