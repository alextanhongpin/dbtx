-- jobs: archive table for finished executions (jobs.Client.Archive).

-- Finished executions moved out of the hot table by jobs.Client.Archive, so
-- durable.executions stays small while idempotency keys (which keep pointing
-- at the execution ID) are remembered for longer. attempt_log holds the
-- execution_attempts rows as a JSON array; steps are not kept.
CREATE TABLE IF NOT EXISTS durable.executions_archive (
    id               uuid        PRIMARY KEY,
    queue            text        NOT NULL,
    kind             text        NOT NULL,
    scope            text,
    key              text,
    payload          jsonb       NOT NULL,
    payload_version  integer     NOT NULL,
    mode             text,
    status           text        NOT NULL CHECK (status IN ('succeeded', 'failed', 'cancelled')),
    attempts         integer     NOT NULL,
    max_attempts     integer     NOT NULL,
    run_at           timestamptz NOT NULL,
    token            bigint      NOT NULL,
    checkpoint_name  text        NOT NULL DEFAULT '',
    checkpoint_data  jsonb,
    last_error       text,
    output           jsonb,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    finished_at      timestamptz NOT NULL,
    attempt_log      jsonb       NOT NULL DEFAULT '[]',
    archived_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS executions_archive_archived_idx
    ON durable.executions_archive (archived_at);
CREATE INDEX IF NOT EXISTS executions_archive_key_idx
    ON durable.executions_archive (scope, key) WHERE key IS NOT NULL;
