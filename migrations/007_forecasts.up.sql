-- 007_forecasts.up.sql
-- Create forecast_runs, calibration_coefficients, calibration_candidates,
-- verification_results, job_locks, job_history, and usage_history tables.

-- =============================================================================
-- Forecast Runs
-- =============================================================================
CREATE TABLE forecast_runs (
    id                    TEXT PRIMARY KEY DEFAULT 'run_' || gen_random_uuid()::text,
    model                 VARCHAR(50) NOT NULL,   -- 'medium_range', 'nowcast'
    run_timestamp         TIMESTAMPTZ NOT NULL,
    source_data_timestamp TIMESTAMPTZ NOT NULL,
    storage_path          TEXT NOT NULL,
    status                VARCHAR(20) NOT NULL CHECK (status IN ('running', 'complete', 'failed', 'deleted')),
    external_id           VARCHAR(100),           -- RunPod Job ID for tracking/cancellation
    retry_count           INTEGER DEFAULT 0,      -- For FCST-004 recovery logic
    failure_reason        TEXT,
    inference_duration_ms INTEGER,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_forecast_runs_latest ON forecast_runs(model, run_timestamp DESC) WHERE status = 'complete';

-- =============================================================================
-- Calibration Candidates
-- =============================================================================
-- Stores calibration updates that failed safety bounds checks.
-- Requires manual promotion by Ops. See OBS-006.
CREATE TABLE calibration_candidates (
    id                    BIGSERIAL PRIMARY KEY,
    location_id           TEXT NOT NULL,
    proposed_coefficients JSONB NOT NULL,
    generated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    violation_reason      TEXT,                -- e.g., "delta_exceeded_15_percent"
    status                TEXT DEFAULT 'pending' CHECK (status IN ('pending', 'promoted', 'rejected'))
);

CREATE INDEX idx_calibration_candidates_status ON calibration_candidates(status) WHERE status = 'pending';

-- =============================================================================
-- Calibration Coefficients
-- =============================================================================
CREATE TABLE calibration_coefficients (
    location_id     TEXT PRIMARY KEY, -- Geohash or Tile ID
    high_threshold  DOUBLE PRECISION NOT NULL,
    high_slope      DOUBLE PRECISION NOT NULL,
    high_base       DOUBLE PRECISION NOT NULL,
    mid_threshold   DOUBLE PRECISION NOT NULL,
    mid_slope       DOUBLE PRECISION NOT NULL,
    mid_base        DOUBLE PRECISION NOT NULL,
    low_base        DOUBLE PRECISION NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- =============================================================================
-- Verification Results (OBS-005)
-- =============================================================================
CREATE TABLE verification_results (
    id              BIGSERIAL PRIMARY KEY,
    forecast_run_id TEXT NOT NULL REFERENCES forecast_runs(id),
    location_id     TEXT NOT NULL,          -- TileID or Region Code
    metric_type     VARCHAR(50) NOT NULL,   -- 'rmse', 'bias', 'brier'
    variable        VARCHAR(50) NOT NULL,   -- 'temperature_c', etc.
    value           DOUBLE PRECISION NOT NULL,
    computed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_verification_run ON verification_results(forecast_run_id);

-- =============================================================================
-- Job Locks for Idempotency
-- =============================================================================
CREATE TABLE job_locks (
    id          TEXT PRIMARY KEY,   -- Deterministic Key (task_timestamp)
    worker_id   TEXT NOT NULL,      -- AWS Request ID
    locked_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL
);

-- =============================================================================
-- Job History for Observability
-- =============================================================================
CREATE TABLE job_history (
    id          BIGSERIAL PRIMARY KEY,
    job_type    VARCHAR(50) NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    status      VARCHAR(20) NOT NULL CHECK (status IN ('running', 'success', 'failed')),
    items_count INTEGER DEFAULT 0,
    error       TEXT,
    metadata    JSONB
);

CREATE INDEX idx_job_history_type_time ON job_history(job_type, started_at DESC);

-- =============================================================================
-- Usage History for Billing (SCHED-002)
-- =============================================================================
-- Supports per-source usage attribution for vertical apps (VERT-001)
CREATE TABLE usage_history (
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    date            DATE NOT NULL,  -- UTC Date
    source          VARCHAR(50) NOT NULL DEFAULT 'default', -- Vertical app identity for usage attribution
    api_calls       INTEGER NOT NULL DEFAULT 0,
    watchpoints     INTEGER NOT NULL DEFAULT 0,
    notifications   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (organization_id, date, source)
);

-- Index for efficient organization-wide aggregation
CREATE INDEX idx_usage_history_org_date ON usage_history(organization_id, date);

-- =============================================================================
-- Index for Archiver Performance (on watchpoints table)
-- =============================================================================
CREATE INDEX idx_watchpoints_archive_candidates
ON watchpoints(time_window_end)
WHERE status = 'active' AND time_window_end IS NOT NULL;
