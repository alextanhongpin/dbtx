CREATE SCHEMA IF NOT EXISTS dbtx;

CREATE TABLE IF NOT EXISTS dbtx.bandit_experiments (
    id UUID PRIMARY KEY,
    definition JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE IF NOT EXISTS dbtx.bandit_arms (
    experiment_id UUID NOT NULL REFERENCES dbtx.bandit_experiments(id),
    arm_key TEXT NOT NULL CHECK (arm_key <> ''),
    label TEXT NOT NULL,
    ordinal INT NOT NULL CHECK (ordinal >= 0),
    PRIMARY KEY (experiment_id, arm_key),
    UNIQUE (experiment_id, ordinal)
);
CREATE TABLE IF NOT EXISTS dbtx.bandit_runs (
    id UUID PRIMARY KEY,
    experiment_id UUID NOT NULL REFERENCES dbtx.bandit_experiments(id),
    definition JSONB NOT NULL,
    sequence BIGINT NOT NULL DEFAULT 0 CHECK (sequence >= 0),
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    closed_at TIMESTAMPTZ,
    UNIQUE (id, experiment_id),
    CHECK ((status = 'closed') = (closed_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS bandit_runs_experiment ON dbtx.bandit_runs(experiment_id, created_at, id);
CREATE TABLE IF NOT EXISTS dbtx.bandit_statistics (
    run_id UUID NOT NULL,
    experiment_id UUID NOT NULL,
    arm_key TEXT NOT NULL,
    selections BIGINT NOT NULL DEFAULT 0 CHECK (selections >= 0),
    observations BIGINT NOT NULL DEFAULT 0 CHECK (observations BETWEEN 0 AND selections),
    successes BIGINT NOT NULL DEFAULT 0 CHECK (successes BETWEEN 0 AND observations),
    PRIMARY KEY (run_id, arm_key),
    FOREIGN KEY (run_id, experiment_id) REFERENCES dbtx.bandit_runs(id, experiment_id),
    FOREIGN KEY (experiment_id, arm_key) REFERENCES dbtx.bandit_arms(experiment_id, arm_key)
);
CREATE TABLE IF NOT EXISTS dbtx.bandit_decisions (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL,
    arm_key TEXT NOT NULL,
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    request_key TEXT NOT NULL CHECK (request_key <> '' AND octet_length(request_key) <= 1024),
    diagnostics JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (run_id, sequence),
    UNIQUE (run_id, request_key),
    FOREIGN KEY (run_id, arm_key) REFERENCES dbtx.bandit_statistics(run_id, arm_key)
);
CREATE TABLE IF NOT EXISTS dbtx.bandit_feedback (
    decision_id UUID PRIMARY KEY REFERENCES dbtx.bandit_decisions(id),
    success BOOLEAN NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
