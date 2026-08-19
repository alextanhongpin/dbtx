CREATE SCHEMA outbox;
CREATE TABLE outbox.messages (
	id uuid NOT NULL DEFAULT uuidv7(),
	aggregate_id text NOT NULL,
	aggregate_type text NOT NULL,
	type text NOT NULL,
	payload jsonb NOT NULL DEFAULT '{}',
	created_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (id)
);
