SELECT json_agg(t)
FROM outbox.messages t;
