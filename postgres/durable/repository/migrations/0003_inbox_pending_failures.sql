-- Inbox: count failed drain attempts of buffered sequenced messages and park
-- poison messages so operators can see them (inbox.Parked) and act on them
-- (inbox.Unpark, inbox.Discard). Safe to apply more than once.

ALTER TABLE durable.inbox_pending
    ADD COLUMN IF NOT EXISTS attempts   integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_error text,
    ADD COLUMN IF NOT EXISTS failed_at  timestamptz,
    ADD COLUMN IF NOT EXISTS parked_at  timestamptz;

CREATE INDEX IF NOT EXISTS inbox_pending_parked_idx
    ON durable.inbox_pending (parked_at) WHERE parked_at IS NOT NULL;
