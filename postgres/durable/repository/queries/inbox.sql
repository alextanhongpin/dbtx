-- name: RecordMessage :execrows
-- execution_id is set afterwards by SetMessageExecution (inbox Enqueue).
INSERT INTO durable.inbox_messages (source, message_id, aggregate_key, seq, version, status)
VALUES (sqlc.arg(source), sqlc.arg(message_id), sqlc.narg(aggregate_key), sqlc.narg(seq),
        sqlc.narg(version), sqlc.arg(status))
ON CONFLICT (source, message_id) DO NOTHING;

-- name: EnsureInboxStream :exec
INSERT INTO durable.inbox_streams (source, aggregate_key) VALUES (sqlc.arg(source), sqlc.arg(aggregate_key))
ON CONFLICT DO NOTHING;

-- name: LockInboxStream :one
SELECT last_applied_seq, last_version FROM durable.inbox_streams
WHERE source = sqlc.arg(source) AND aggregate_key = sqlc.arg(aggregate_key) FOR UPDATE;

-- name: BufferMessage :exec
INSERT INTO durable.inbox_pending (source, aggregate_key, seq, message_id, type, payload, is_gap)
VALUES (sqlc.arg(source), sqlc.arg(aggregate_key), sqlc.arg(seq), sqlc.arg(message_id),
        sqlc.arg(type), sqlc.narg(payload)::jsonb, sqlc.arg(is_gap))
ON CONFLICT DO NOTHING;

-- name: MarkInboxGap :exec
UPDATE durable.inbox_streams SET gap_since = coalesce(gap_since, now()), updated_at = now()
WHERE source = sqlc.arg(source) AND aggregate_key = sqlc.arg(aggregate_key);

-- name: GetBuffered :one
SELECT seq, message_id, type, payload, is_gap FROM durable.inbox_pending
WHERE source = sqlc.arg(source) AND aggregate_key = sqlc.arg(aggregate_key) AND seq = sqlc.arg(seq);

-- name: DeleteBuffered :exec
DELETE FROM durable.inbox_pending
WHERE source = sqlc.arg(source) AND aggregate_key = sqlc.arg(aggregate_key) AND seq = sqlc.arg(seq);

-- name: SetInboxApplied :exec
UPDATE durable.inbox_streams
SET last_applied_seq = sqlc.arg(seq)::bigint, updated_at = now(),
    gap_since = CASE WHEN EXISTS (SELECT 1 FROM durable.inbox_pending p
                                  WHERE p.source = sqlc.arg(source) AND p.aggregate_key = sqlc.arg(aggregate_key))
                     THEN coalesce(gap_since, now()) END
WHERE source = sqlc.arg(source) AND aggregate_key = sqlc.arg(aggregate_key);

-- name: SetInboxVersion :exec
UPDATE durable.inbox_streams SET last_version = sqlc.arg(version), updated_at = now()
WHERE source = sqlc.arg(source) AND aggregate_key = sqlc.arg(aggregate_key);

-- name: SetMessageExecution :exec
UPDATE durable.inbox_messages SET execution_id = sqlc.arg(execution_id)::uuid
WHERE source = sqlc.arg(source) AND message_id = sqlc.arg(message_id);

-- name: ListInboxGaps :many
SELECT s.source, s.aggregate_key, s.last_applied_seq,
       coalesce((SELECT min(p.seq) FROM durable.inbox_pending p
                 WHERE p.source = s.source AND p.aggregate_key = s.aggregate_key), 0)::bigint AS next_buffered,
       s.gap_since::timestamptz AS gap_since
FROM durable.inbox_streams s
WHERE s.gap_since IS NOT NULL
  AND s.gap_since <= clock_timestamp() - sqlc.arg(min_age_us)::bigint * interval '1 microsecond'
ORDER BY s.gap_since;

-- name: MinBufferedSeq :one
SELECT coalesce(min(seq), 0)::bigint AS seq FROM durable.inbox_pending
WHERE source = sqlc.arg(source) AND aggregate_key = sqlc.arg(aggregate_key);

-- name: PurgeInboxMessages :execrows
DELETE FROM durable.inbox_messages m WHERE (m.source, m.message_id) IN (
    SELECT x.source, x.message_id FROM durable.inbox_messages x
    WHERE x.source = sqlc.arg(source) AND x.received_at < sqlc.arg(cutoff)::timestamptz
      AND x.status <> 'buffered'
    LIMIT sqlc.arg(lim)::integer);
