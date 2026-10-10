-- name: CreateExperiment :one
INSERT INTO dbtx.bandit_experiments (id, definition) VALUES ($1,$2) RETURNING *;
-- name: CreateArm :exec
INSERT INTO dbtx.bandit_arms (experiment_id,arm_key,label,ordinal) VALUES ($1,$2,$3,$4);
-- name: Experiment :one
SELECT * FROM dbtx.bandit_experiments WHERE id=$1;
-- name: Experiments :many
SELECT * FROM dbtx.bandit_experiments ORDER BY created_at DESC,id LIMIT $1 OFFSET $2;
-- name: CreateRun :one
INSERT INTO dbtx.bandit_runs (id,experiment_id,definition) VALUES ($1,$2,$3) RETURNING *;
-- name: CreateStatistics :exec
INSERT INTO dbtx.bandit_statistics (run_id,experiment_id,arm_key) VALUES ($1,$2,$3);
-- name: Run :one
SELECT * FROM dbtx.bandit_runs WHERE id=$1;
-- name: LockRun :one
SELECT * FROM dbtx.bandit_runs WHERE id=$1 FOR UPDATE;
-- name: Runs :many
SELECT * FROM dbtx.bandit_runs WHERE experiment_id=$1 ORDER BY created_at DESC,id LIMIT $2 OFFSET $3;
-- name: Statistics :many
SELECT s.*,a.label,a.ordinal FROM dbtx.bandit_statistics s JOIN dbtx.bandit_arms a USING(experiment_id,arm_key) WHERE s.run_id=$1 ORDER BY a.ordinal;
-- name: DecisionByKey :one
SELECT d.*,f.success,f.observed_at FROM dbtx.bandit_decisions d LEFT JOIN dbtx.bandit_feedback f ON f.decision_id=d.id WHERE d.run_id=$1 AND d.request_key=$2;
-- name: Decision :one
SELECT d.*,f.success,f.observed_at FROM dbtx.bandit_decisions d LEFT JOIN dbtx.bandit_feedback f ON f.decision_id=d.id WHERE d.id=$1;
-- name: History :many
SELECT d.*,f.success,f.observed_at FROM dbtx.bandit_decisions d LEFT JOIN dbtx.bandit_feedback f ON f.decision_id=d.id WHERE d.run_id=$1 ORDER BY d.sequence LIMIT $2 OFFSET $3;
-- name: CreateDecision :one
INSERT INTO dbtx.bandit_decisions (id,run_id,arm_key,sequence,request_key,diagnostics) VALUES ($1,$2,$3,$4,$5,$6) RETURNING *;
-- name: Advance :exec
UPDATE dbtx.bandit_runs SET sequence=sequence+1 WHERE id=$1;
-- name: SelectArm :exec
UPDATE dbtx.bandit_statistics SET selections=selections+1 WHERE run_id=$1 AND arm_key=$2;
-- name: CreateFeedback :exec
INSERT INTO dbtx.bandit_feedback (decision_id,success) VALUES ($1,$2);
-- name: ObserveArm :exec
UPDATE dbtx.bandit_statistics SET observations=observations+1,successes=successes+sqlc.arg(reward)::bigint WHERE run_id=sqlc.arg(run_id) AND arm_key=sqlc.arg(arm_key);
-- name: CloseRun :exec
UPDATE dbtx.bandit_runs SET status='closed',closed_at=COALESCE(closed_at,clock_timestamp()) WHERE id=$1;
-- name: FinishSimulation :exec
UPDATE dbtx.bandit_runs SET sequence=$2,status='closed',closed_at=clock_timestamp() WHERE id=$1;
-- name: SaveStatistics :exec
UPDATE dbtx.bandit_statistics SET selections=$3,observations=$4,successes=$5 WHERE run_id=$1 AND arm_key=$2;

-- name: Trajectory :many
SELECT d.id,d.run_id,d.arm_key,d.sequence,d.request_key,d.created_at,f.success,f.observed_at FROM dbtx.bandit_decisions d LEFT JOIN dbtx.bandit_feedback f ON f.decision_id=d.id WHERE d.run_id=$1 ORDER BY d.sequence;
