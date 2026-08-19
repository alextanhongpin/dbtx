WITH inserted AS (
	INSERT INTO outbox.messages (
		aggregate_id,
		aggregate_type,
		type,
		payload
	)
	SELECT *
	FROM jsonb_to_recordset($1::jsonb) AS t(
		aggregate_id text,
		aggregate_type text,
		type text,
		payload jsonb
	)
	RETURNING *
)
SELECT json_agg(t)
FROM inserted t;
