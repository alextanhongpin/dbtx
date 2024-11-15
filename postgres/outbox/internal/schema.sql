CREATE TABLE outbox (
	id bigint GENERATED ALWAYS AS IDENTITY,
	aggregate_id text NOT NULL,
	aggregate_type text NOT NULL,
	type text NOT NULL,
	payload jsonb NOT NULL DEFAULT '{}',
	created_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (id)
);
