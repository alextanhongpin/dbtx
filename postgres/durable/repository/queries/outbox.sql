-- name: NextHeadSeq :one
INSERT INTO durable.outbox_heads AS h (topic, aggregate_key, head_seq)
VALUES (sqlc.arg(topic), sqlc.arg(aggregate_key), 1)
ON CONFLICT (topic, aggregate_key) DO UPDATE SET head_seq = h.head_seq + 1
RETURNING head_seq;

-- name: InsertOrderedEvent :exec
INSERT INTO durable.outbox_events (id, topic, type, aggregate_key, seq, headers, payload, max_attempts)
VALUES (sqlc.arg(id), sqlc.arg(topic), sqlc.arg(type), sqlc.arg(aggregate_key), sqlc.arg(seq),
        sqlc.arg(headers)::jsonb, sqlc.arg(payload)::jsonb, sqlc.arg(max_attempts));

-- name: InsertUnorderedEvent :exec
INSERT INTO durable.outbox_events (id, topic, type, partition_key, headers, payload, max_attempts, run_at)
VALUES (sqlc.arg(id), sqlc.arg(topic), sqlc.arg(type), sqlc.narg(partition_key),
        sqlc.arg(headers)::jsonb, sqlc.arg(payload)::jsonb, sqlc.arg(max_attempts),
        coalesce(sqlc.narg(run_at)::timestamptz, clock_timestamp()));

-- name: ExpiredUnorderedEvents :many
-- Unordered events whose lease expired on the final attempt.
SELECT id, owner_id, token FROM durable.outbox_events
WHERE aggregate_key IS NULL AND status = 'running'
  AND lease_expires_at <= now() AND attempts >= max_attempts
LIMIT 100 FOR UPDATE SKIP LOCKED;

-- name: ClaimUnorderedEvents :many
-- Columns other than token and attempts are immutable, so reading them
-- next to the grant in one statement is safe.
SELECT g.id::uuid AS id, g.token::bigint AS token, g.attempts::integer AS attempts,
       e.topic, e.type, coalesce(e.partition_key, '')::text AS partition_key,
       e.headers, e.payload, e.max_attempts, e.run_at
FROM durable.lease_grant('durable.outbox_events', ARRAY(
        SELECT c.id FROM durable.outbox_events c
        WHERE c.aggregate_key IS NULL
          AND c.topic IN (SELECT jsonb_array_elements_text(sqlc.arg(topics)::jsonb))
          AND ((c.status = 'pending' AND c.run_at <= now())
            OR (c.status = 'running' AND c.lease_expires_at <= now() AND c.attempts < c.max_attempts))
        ORDER BY c.run_at, c.id
        LIMIT sqlc.arg(lim)::integer
        FOR UPDATE SKIP LOCKED),
    sqlc.arg(owner_id)::text, sqlc.arg(ttl_us)::bigint, 'pending', 'running', true) g
JOIN durable.outbox_events e ON e.id = g.id
ORDER BY e.run_at, e.id;

-- name: MarkEventsPublished :execrows
-- Owner match is enough (D7): while it matches, no other relay reclaimed them.
UPDATE durable.outbox_events
SET status = 'published', published_at = now(), owner_id = NULL, last_error = NULL, updated_at = now()
WHERE id IN (SELECT jsonb_array_elements_text(sqlc.arg(ids)::jsonb)::uuid)
  AND owner_id = sqlc.arg(owner_id) AND status = 'running';

-- name: EnsureStreams :exec
INSERT INTO durable.outbox_streams (topic, aggregate_key)
SELECT DISTINCT e.topic, e.aggregate_key FROM durable.outbox_events e
WHERE e.aggregate_key IS NOT NULL AND e.status = 'pending'
  AND e.topic IN (SELECT jsonb_array_elements_text(sqlc.arg(topics)::jsonb))
  AND NOT EXISTS (SELECT 1 FROM durable.outbox_streams s
                  WHERE s.topic = e.topic AND s.aggregate_key = e.aggregate_key)
ON CONFLICT DO NOTHING;

-- name: ClaimStreams :many
-- A stream is held while active with a live owner; grant counts no attempt
-- (attempts counts consecutive failures of the head event).
SELECT g.id::uuid AS id, g.token::bigint AS token, s.topic, s.aggregate_key,
       s.last_published_seq, s.attempts
FROM durable.lease_grant('durable.outbox_streams', ARRAY(
        SELECT c.id FROM durable.outbox_streams c
        WHERE c.topic IN (SELECT jsonb_array_elements_text(sqlc.arg(topics)::jsonb))
          AND c.status = 'active' AND c.run_at <= now()
          AND (c.owner_id IS NULL OR c.lease_expires_at <= now())
          AND EXISTS (SELECT 1 FROM durable.outbox_events e
                      WHERE e.topic = c.topic AND e.aggregate_key = c.aggregate_key
                        AND e.status = 'pending' AND e.seq > c.last_published_seq)
        ORDER BY c.run_at
        LIMIT sqlc.arg(lim)::integer
        FOR UPDATE SKIP LOCKED),
    sqlc.arg(owner_id)::text, sqlc.arg(ttl_us)::bigint, 'active', 'active', false) g
JOIN durable.outbox_streams s ON s.id = g.id;

-- name: LoadStreamEvents :many
SELECT id, type, seq::bigint AS seq, headers, payload, is_gap FROM durable.outbox_events
WHERE topic = sqlc.arg(topic) AND aggregate_key = sqlc.arg(aggregate_key)::text
  AND seq > sqlc.arg(after_seq)::bigint AND status = 'pending'
ORDER BY seq LIMIT sqlc.arg(lim)::integer;

-- name: SetStreamPosition :execrows
-- Run after durable.lease_lock in the same transaction.
UPDATE durable.outbox_streams
SET last_published_seq = sqlc.arg(seq)::bigint, attempts = 0, last_error = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND last_published_seq < sqlc.arg(seq)::bigint;

-- name: MarkStreamPublished :exec
UPDATE durable.outbox_events SET status = 'published', published_at = now(), updated_at = now()
WHERE topic = sqlc.arg(topic) AND aggregate_key = sqlc.arg(aggregate_key)::text
  AND seq > sqlc.arg(after_seq)::bigint AND seq <= sqlc.arg(up_to_seq)::bigint AND status = 'pending';

-- name: MarkEventGap :execrows
UPDATE durable.outbox_events SET is_gap = true, last_error = sqlc.arg(reason)::text, updated_at = now()
WHERE topic = sqlc.arg(topic) AND aggregate_key = sqlc.arg(aggregate_key)::text
  AND seq = sqlc.arg(seq)::bigint AND status = 'pending';

-- name: GetStream :one
SELECT status, last_published_seq, attempts, coalesce(last_error, '')::text AS last_error, run_at
FROM durable.outbox_streams WHERE topic = sqlc.arg(topic) AND aggregate_key = sqlc.arg(aggregate_key)::text;

-- name: GetStreamHead :one
SELECT coalesce((SELECT h.head_seq FROM durable.outbox_heads h
                 WHERE h.topic = sqlc.arg(topic) AND h.aggregate_key = sqlc.arg(aggregate_key)::text), 0)::bigint AS head_seq,
       (SELECT count(*) FROM durable.outbox_events e
        WHERE e.topic = sqlc.arg(topic) AND e.aggregate_key = sqlc.arg(aggregate_key)::text
          AND e.status = 'pending')::integer AS pending;

-- name: ActivateStream :execrows
-- Admin change: a new token fences out any relay holding the stream (D18).
UPDATE durable.outbox_streams
SET status = 'active', attempts = 0, run_at = clock_timestamp(), updated_at = now(),
    owner_id = NULL, lease_expires_at = NULL, token = nextval('durable.fencing_seq')
WHERE topic = sqlc.arg(topic) AND aggregate_key = sqlc.arg(aggregate_key)::text
  AND (NOT sqlc.arg(only_inactive)::boolean OR status IN ('blocked', 'stopped'));

-- name: OldestRetainedSeq :one
SELECT coalesce(min(seq), 0)::bigint AS seq FROM durable.outbox_events
WHERE topic = sqlc.arg(topic) AND aggregate_key = sqlc.arg(aggregate_key)::text AND seq >= sqlc.arg(from_seq)::bigint;

-- name: RequeuePublishedFrom :execrows
UPDATE durable.outbox_events SET status = 'pending', published_at = NULL, updated_at = now()
WHERE topic = sqlc.arg(topic) AND aggregate_key = sqlc.arg(aggregate_key)::text
  AND seq >= sqlc.arg(from_seq)::bigint AND status = 'published';

-- name: RewindStream :exec
UPDATE durable.outbox_streams
SET last_published_seq = LEAST(last_published_seq, sqlc.arg(from_seq)::bigint - 1),
    run_at = clock_timestamp(), updated_at = now(),
    owner_id = NULL, lease_expires_at = NULL, token = nextval('durable.fencing_seq')
WHERE topic = sqlc.arg(topic) AND aggregate_key = sqlc.arg(aggregate_key)::text;

-- name: PurgePublishedEvents :execrows
DELETE FROM durable.outbox_events WHERE id IN (
    SELECT e.id FROM durable.outbox_events e
    WHERE e.status = 'published' AND e.published_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(lim)::integer);

-- name: DeadEvents :many
SELECT id, topic, type, attempts, coalesce(last_error, '')::text AS last_error
FROM durable.outbox_events WHERE status = 'dead' ORDER BY created_at LIMIT sqlc.arg(lim)::integer;

-- name: RequeueDeadEvent :execrows
UPDATE durable.outbox_events SET status = 'pending', attempts = 0, run_at = clock_timestamp(), updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'dead';
