CREATE SCHEMA IF NOT EXISTS dbtx;

CREATE TABLE dbtx.ab_experiments (
    id text PRIMARY KEY CHECK (btrim(id) <> ''),
    control text NOT NULL CHECK (btrim(control) <> ''),
    treatment text NOT NULL CHECK (btrim(treatment) <> '' AND treatment <> control),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    closed_at timestamptz
);

CREATE TABLE dbtx.ab_assignments (
    experiment_id text NOT NULL REFERENCES dbtx.ab_experiments(id),
    subject_id text NOT NULL CHECK (btrim(subject_id) <> ''),
    variant text NOT NULL,
    assigned_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    converted_at timestamptz,
    PRIMARY KEY (experiment_id, subject_id)
);
