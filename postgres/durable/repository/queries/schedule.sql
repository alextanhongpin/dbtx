-- name: UpsertSchedule :exec
INSERT INTO durable.schedules AS s (name, spec, time_zone, queue, kind, payload, missed, overlap, next_run_at)
VALUES (sqlc.arg(name), sqlc.arg(spec), sqlc.arg(time_zone), sqlc.arg(queue), sqlc.arg(kind),
        sqlc.arg(payload)::jsonb, sqlc.arg(missed), sqlc.arg(overlap), sqlc.arg(next_run_at))
ON CONFLICT (name) DO UPDATE SET
    queue = EXCLUDED.queue, kind = EXCLUDED.kind, payload = EXCLUDED.payload,
    missed = EXCLUDED.missed, overlap = EXCLUDED.overlap,
    next_run_at = CASE WHEN s.spec = EXCLUDED.spec AND s.time_zone = EXCLUDED.time_zone
                       THEN s.next_run_at ELSE EXCLUDED.next_run_at END,
    spec = EXCLUDED.spec, time_zone = EXCLUDED.time_zone, updated_at = now();

-- name: SetSchedulePaused :execrows
UPDATE durable.schedules SET paused = sqlc.arg(paused), updated_at = now() WHERE name = sqlc.arg(name);

-- name: LockSchedule :one
SELECT spec, time_zone, clock_timestamp()::timestamptz AS now FROM durable.schedules
WHERE name = sqlc.arg(name) FOR UPDATE;

-- name: ResumeSchedule :exec
UPDATE durable.schedules SET paused = false, next_run_at = sqlc.arg(next_run_at), updated_at = now()
WHERE name = sqlc.arg(name);

-- name: DeleteSchedule :exec
DELETE FROM durable.schedules WHERE name = sqlc.arg(name);

-- name: NextDueSchedule :one
SELECT name, spec, time_zone, kind, queue, payload, missed, overlap, next_run_at,
       last_execution_id, clock_timestamp()::timestamptz AS now
FROM durable.schedules
WHERE NOT paused AND next_run_at <= clock_timestamp()
  AND name NOT IN (SELECT jsonb_array_elements_text(sqlc.arg(skip)::jsonb))
ORDER BY next_run_at LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: AdvanceSchedule :exec
UPDATE durable.schedules
SET next_run_at = sqlc.arg(next_run_at),
    last_run_at = CASE WHEN sqlc.arg(fired)::boolean THEN sqlc.arg(last_tick)::timestamptz ELSE last_run_at END,
    -- The zero UUID means "no execution yet".
    last_execution_id = nullif(sqlc.arg(last_execution_id)::uuid, '00000000-0000-0000-0000-000000000000'),
    updated_at = now()
WHERE name = sqlc.arg(name);
