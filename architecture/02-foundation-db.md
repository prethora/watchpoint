# 02 - Foundation Database

> **Purpose**: Defines the database schema, connection strategy, migration plan, and data access interfaces for the WatchPoint platform.
> **Package**: `package db`
> **Dependencies**: `01-foundation-types.md`

---

## Table of Contents

1. [Overview & Connection Strategy](#1-overview--connection-strategy)
2. [Extensions & Standards](#2-extensions--standards)
3. [Core Domain Schema](#3-core-domain-schema)
4. [Evaluation & Notification Schema](#4-evaluation--notification-schema)
5. [Auth, Security & Rate Limits](#5-auth-security--rate-limits)
6. [Forecast & Calibration Schema](#6-forecast--calibration-schema)
7. [Triggers & Functions](#7-triggers--functions)
8. [Migration Strategy](#8-migration-strategy)
9. [Repository Interfaces](#9-repository-interfaces)

---

## 1. Overview & Connection Strategy

The platform uses **PostgreSQL 15+** (via Supabase) as the primary data store. Due to the serverless nature of the compute layer (AWS Lambda), connection management is critical.

### 1.1 Connection Configuration

We connect via the **Supabase Transaction Pooler** (PgBouncer) running on port `6543`. This requires client-side configuration to disable prepared statements, as they are incompatible with transaction mode pooling.

```go
// DBConfig defines the connection parameters required for pgxpool
type DBConfig struct {
    Host              string        // "aws-0-us-east-1.pooler.supabase.com"
    Port              int           // Must be 6543 (Transaction Mode)
    User              string
    Password          string        // Loaded from SSM
    Database          string        // "postgres"
    MaxConns          int32         // Recommended: 1-2 per Lambda instance
    MinConns          int32         // Recommended: 0 for Lambda
    MaxConnLifetime   time.Duration // Recommended: 30m to recycle connections
    AcquireTimeout    time.Duration // Fail fast timeout (e.g. 2s) when pool is exhausted, preventing Lambda hangs.
    HealthCheckPeriod time.Duration // Frequency of ping checks to detect dead connections during failover (e.g. 1m).
}

// NewPool initializes the pgxpool.
// CRITICAL: Must set PreferSimpleProtocol=true in pgx config to disable
// prepared statements.
// MUST apply the following config fields to pgx pool settings:
// - AcquireTimeout: Sets the maximum duration to wait for a connection from the pool.
// - MaxConnLifetime: Sets the maximum lifetime of connections before recycling.
// - HealthCheckPeriod: Sets the frequency of background health checks to detect dead connections.
func NewPool(ctx context.Context, cfg DBConfig) (*pgxpool.Pool, error)
```

---

## 2. Extensions & Standards

### 2.1 Required Extensions

The following extensions must be enabled in the initial migration:

| Extension | Purpose |
|---|---|
| `uuid-ossp` | UUID v4 generation (`gen_random_uuid()`). |
| `cube` | Prerequisite for `earthdistance`. |
| `earthdistance` | Geospatial queries (finding WatchPoints near a location). |

### 2.2 Naming Conventions

*   **Tables**: `snake_case`, plural (e.g., `watchpoints`).
*   **Columns**: `snake_case` (e.g., `organization_id`).
*   **Primary Keys**: `id` (Text, prefixed UUID, e.g., `wp_...`).
*   **Indexes**: `idx_table_column` or `idx_table_purpose`.
*   **Timestamps**: Always `TIMESTAMPTZ` (UTC).

---

## 3. Core Domain Schema

### 3.1 Organizations & Users

```sql
CREATE TABLE organizations (
    id                  TEXT PRIMARY KEY, -- "org_..."
    name                VARCHAR(200) NOT NULL,
    billing_email       VARCHAR(255) NOT NULL,
    plan                VARCHAR(50) NOT NULL DEFAULT 'free',
    plan_limits         JSONB, -- { "watchpoints_max": 100, ... }
    stripe_customer_id  VARCHAR(100),
    notification_preferences JSONB, -- Quiet hours config

    -- Optimization for DigestScheduler (Flow NOTIF-003)
    -- Denormalized next run time calculated from preferences.
    -- Updated by: API (on config change) AND Scheduler (after run).
    -- Indexed for fast polling: WHERE next_digest_at <= NOW()
    next_digest_at      TIMESTAMPTZ,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ -- Soft delete support
);

CREATE UNIQUE INDEX idx_org_billing_email ON organizations(billing_email) WHERE deleted_at IS NULL;
CREATE INDEX idx_org_stripe ON organizations(stripe_customer_id);

-- Index for efficient DigestScheduler polling
CREATE INDEX idx_org_next_digest ON organizations(next_digest_at) WHERE deleted_at IS NULL;

CREATE TABLE users (
    id                  TEXT PRIMARY KEY, -- "user_..."
    organization_id     TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email               VARCHAR(255) NOT NULL,
    password_hash       TEXT, -- Nullable for OAuth users
    role                VARCHAR(20) NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at       TIMESTAMPTZ,
    deleted_at          TIMESTAMPTZ -- Soft delete cascade
);

CREATE UNIQUE INDEX idx_users_email ON users(email) WHERE deleted_at IS NULL;
CREATE INDEX idx_users_org ON users(organization_id);
```

### 3.2 WatchPoints

The central entity. Uses a generated column for tile-based sharding and specific indexes for the Batcher Lambda.

```sql
CREATE TABLE watchpoints (
    id                    TEXT PRIMARY KEY DEFAULT 'wp_' || gen_random_uuid()::text,
    organization_id       TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,

    -- Identity & Location
    name                  VARCHAR(200),
    location_lat          DOUBLE PRECISION NOT NULL CHECK (location_lat BETWEEN -90 AND 90),
    location_lon          DOUBLE PRECISION NOT NULL CHECK (location_lon BETWEEN -180 AND 180),
    location_display_name VARCHAR(200),
    timezone              VARCHAR(50) NOT NULL,
    
    -- Spatial Sharding (Deterministic Tile ID)
    -- Generates "lat_index.lon_index" for 22.5x45 degree tiles
    tile_id TEXT GENERATED ALWAYS AS (
        FLOOR((90.0 - location_lat) / 22.5)::INT::TEXT
        || '.'
        || FLOOR(
            CASE
                WHEN location_lon >= 0 THEN location_lon
                ELSE 360.0 + location_lon
            END / 45.0
        )::INT::TEXT
    ) STORED,

    -- Mode Configuration (Mutually Exclusive)
    time_window_start     TIMESTAMPTZ, -- Nullable for Monitor Mode
    time_window_end       TIMESTAMPTZ,
    monitor_config        JSONB,       -- Nullable for Event Mode
    
    -- Logic & Delivery
    conditions            JSONB NOT NULL, -- Array of Condition objects
    condition_logic       VARCHAR(10) NOT NULL DEFAULT 'ALL' CHECK (condition_logic IN ('ANY', 'ALL')),
    channels              JSONB NOT NULL, -- Array of Channel objects
    preferences           JSONB,
    template_set          VARCHAR(50) DEFAULT 'default',
    
    -- Meta
    status                VARCHAR(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'archived')),
    test_mode             BOOLEAN NOT NULL DEFAULT FALSE,
    tags                  TEXT[],
    config_version        INTEGER NOT NULL DEFAULT 1, -- Increments on logic change
    source                VARCHAR(100),
    
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    archived_at           TIMESTAMPTZ,
    archived_reason       VARCHAR(50),
    deleted_at            TIMESTAMPTZ, -- Soft delete

    -- Pause reason: Distinguishes user pauses from billing enforcements.
    paused_reason         VARCHAR(50),

    -- Post-Event Summary: Stores the accuracy report (Predicted vs Actual)
    -- generated by the Eval Worker upon archival. See OBS-007.
    summary               JSONB,

    -- Constraint: Enforce Mode Exclusivity
    CONSTRAINT valid_mode CHECK (
        (time_window_start IS NOT NULL AND time_window_end IS NOT NULL AND monitor_config IS NULL)
        OR
        (time_window_start IS NULL AND time_window_end IS NULL AND monitor_config IS NOT NULL)
    )
);

-- Indexes

-- 1. Batcher Query (Index-Only Scan): Critical for performance.
--    Must exclude soft-deleted items.
CREATE INDEX idx_watchpoints_tile_active ON watchpoints(tile_id, status) 
WHERE status = 'active' AND deleted_at IS NULL;

-- 2. Geospatial Lookup
CREATE INDEX idx_watchpoints_location ON watchpoints USING GIST (ll_to_earth(location_lat, location_lon));

-- 3. Dashboard/List Views
CREATE INDEX idx_watchpoints_org_status_test ON watchpoints(organization_id, status, test_mode) 
WHERE deleted_at IS NULL;

-- 4. Tag Filtering
CREATE INDEX idx_watchpoints_tags ON watchpoints USING GIN (tags);

-- 5. Archival Cleanup
CREATE INDEX idx_watchpoints_archived ON watchpoints(archived_at) WHERE status = 'archived';
```

---

## 4. Evaluation & Notification Schema

### 4.1 Evaluation State

Tracks the runtime state of each WatchPoint. Separated to allow high-frequency updates without locking configuration.

```sql
CREATE TABLE watchpoint_evaluation_state (
    watchpoint_id           TEXT PRIMARY KEY REFERENCES watchpoints(id) ON DELETE CASCADE,
    
    -- Evaluation Metadata
    last_evaluated_at       TIMESTAMPTZ,
    last_forecast_run       TIMESTAMPTZ,
    config_version          INTEGER NOT NULL, -- Snapshot of config used
    
    -- Trigger Logic & Hysteresis
    previous_trigger_state  BOOLEAN DEFAULT FALSE,
    trigger_state_changed_at TIMESTAMPTZ,
    trigger_value           DOUBLE PRECISION, -- Stores the value that originally triggered the alert (for clearance context)

    -- Escalation Logic
    escalation_level        INTEGER DEFAULT 0,
    last_escalated_at       TIMESTAMPTZ, -- Tracks specific escalation events for 2-hour cooldown enforcement
    
    -- Notification Tracking
    last_notified_at        TIMESTAMPTZ,
    last_notified_state     VARCHAR(20),
    notification_count_24h  INTEGER DEFAULT 0,
    
    -- Digest Support
    last_forecast_summary   JSONB,
    last_digest_sent_at     TIMESTAMPTZ,
    last_digest_content     JSONB,
    
    -- Monitor Mode Deduplication
    -- JSONB array of objects: [{"start": ts, "end": ts, "type": "..."}]
    -- Used for Temporal Overlap Detection (deduplication) instead of raw hashing
    seen_threats            JSONB DEFAULT '[]',
    event_sequence          BIGINT DEFAULT 0
);

CREATE INDEX idx_eval_state_time ON watchpoint_evaluation_state(last_evaluated_at);
```

### 4.2 Notifications & Deliveries

```sql
CREATE TABLE notifications (
    id                  TEXT PRIMARY KEY DEFAULT 'notif_' || gen_random_uuid()::text,
    watchpoint_id       TEXT NOT NULL REFERENCES watchpoints(id) ON DELETE CASCADE,
    organization_id     TEXT NOT NULL REFERENCES organizations(id),

    event_type          VARCHAR(50) NOT NULL, -- 'threshold_crossed', 'digest'
    urgency             VARCHAR(20) NOT NULL, -- 'routine', 'critical'
    payload             JSONB NOT NULL,
    test_mode           BOOLEAN DEFAULT FALSE,

    -- Snapshots the template set at the time of trigger to ensure consistent rendering
    -- even if WatchPoint config changes later. Enables audit trail for template usage.
    template_set        VARCHAR(50) NOT NULL DEFAULT 'default',

    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- BRIN index for append-only time-series data
CREATE INDEX idx_notifications_created_at_brin ON notifications USING BRIN(created_at);
CREATE INDEX idx_notif_wp_time ON notifications(watchpoint_id, created_at DESC);

-- Organization-scoped notification history (INFO-002 query pattern)
CREATE INDEX idx_notif_org ON notifications(organization_id, created_at DESC);

CREATE TABLE notification_deliveries (
    id                  TEXT PRIMARY KEY DEFAULT 'del_' || gen_random_uuid()::text,
    notification_id     TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    
    channel_type        VARCHAR(20) NOT NULL,
    channel_config      JSONB NOT NULL, -- Snapshot at time of send
    
    -- Status values: pending (awaiting), sent (success), failed (permanent), bounced (rejected),
    -- retrying (transient failure), skipped (suppressed), deferred (Quiet Hours - will resume)
    status              VARCHAR(20) NOT NULL CHECK (status IN ('pending', 'sent', 'failed', 'bounced', 'retrying', 'skipped', 'deferred')),
    attempt_count       INTEGER DEFAULT 0,
    next_retry_at       TIMESTAMPTZ,
    last_attempt_at     TIMESTAMPTZ,
    delivered_at        TIMESTAMPTZ,
    
    failure_reason      TEXT,
    provider_message_id TEXT
);

-- Worker Queue Index
CREATE INDEX idx_delivery_queue ON notification_deliveries(status, next_retry_at) 
WHERE status IN ('pending', 'retrying');
```

---

## 5. Auth, Security & Rate Limits

### 5.1 API Keys & Sessions

```sql
CREATE TABLE api_keys (
    id                  TEXT PRIMARY KEY DEFAULT 'key_' || gen_random_uuid()::text,
    organization_id     TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    created_by_user_id  TEXT REFERENCES users(id) ON DELETE SET NULL,

    key_hash            TEXT UNIQUE NOT NULL, -- bcrypt
    key_prefix          TEXT NOT NULL,        -- "sk_live_..."
    scopes              TEXT[] NOT NULL,
    test_mode           BOOLEAN NOT NULL DEFAULT FALSE, -- Aids query filtering
    source              VARCHAR(50),          -- Vertical app identity (e.g., "wedding_app") for usage attribution

    name                VARCHAR(100),
    last_used_at        TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ,
    revoked_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_apikeys_prefix ON api_keys(key_prefix) WHERE revoked_at IS NULL;
CREATE INDEX idx_apikeys_source ON api_keys(organization_id, source) WHERE revoked_at IS NULL;

CREATE TABLE sessions (
    id                  TEXT PRIMARY KEY DEFAULT 'sess_' || gen_random_uuid()::text,
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    organization_id     TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    csrf_token          TEXT NOT NULL,
    ip_address          INET,
    user_agent          TEXT,
    
    expires_at          TIMESTAMPTZ NOT NULL,
    last_activity_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_sessions_expiry ON sessions(expires_at);
```

### 5.2 Security Events (Unified Abuse Protection)

```sql
CREATE TABLE security_events (
    id            BIGSERIAL PRIMARY KEY,
    event_type    VARCHAR(50) NOT NULL,  -- 'login', 'api_auth'
    identifier    TEXT,                   -- e.g., email (nullable for IP-only events)
    ip_address    INET NOT NULL,
    attempted_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    success       BOOLEAN NOT NULL,
    failure_reason TEXT
);

-- Indexes for time-window lookups (IP-based blocking and identifier-based lockouts)
CREATE INDEX idx_security_ip ON security_events(ip_address, attempted_at DESC);
CREATE INDEX idx_security_id ON security_events(identifier, attempted_at DESC);
```

### 5.3 Audit, Idempotency & Rate Limits

```sql
CREATE TABLE audit_log (
    id                  TEXT PRIMARY KEY DEFAULT 'audit_' || gen_random_uuid()::text,
    organization_id     TEXT REFERENCES organizations(id) ON DELETE SET NULL,

    actor_id            TEXT NOT NULL,
    actor_type          VARCHAR(20) NOT NULL, -- 'user', 'api_key', 'system'
    action              VARCHAR(50) NOT NULL,
    resource_type       VARCHAR(50) NOT NULL,
    resource_id         TEXT NOT NULL,
    
    old_value           JSONB,
    new_value           JSONB,
    
    ip_address          INET,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_audit_org_time ON audit_log(organization_id, created_at DESC);

CREATE TABLE idempotency_keys (
    id                  TEXT PRIMARY KEY, -- Client provided key
    organization_id     TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    request_path        TEXT NOT NULL,
    request_params      JSONB,
    
    response_code       INTEGER,
    response_body       JSONB,
    
    status              VARCHAR(20) NOT NULL CHECK (status IN ('processing', 'completed', 'failed')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at          TIMESTAMPTZ NOT NULL
);

-- Separated from Organizations for high-churn write performance
-- Supports per-source usage attribution for vertical apps (VERT-001)
CREATE TABLE rate_limits (
    organization_id     TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    source              VARCHAR(50) NOT NULL DEFAULT 'default', -- Vertical app identity for usage attribution

    api_calls_count     INTEGER NOT NULL DEFAULT 0,
    watchpoints_count   INTEGER NOT NULL DEFAULT 0,

    period_start        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    period_end          TIMESTAMPTZ NOT NULL, -- Midnight UTC

    -- Patch 6.1: Support BILL-010 (Overage Warning tracking)
    warning_sent_at     TIMESTAMPTZ,          -- Set when 80% usage email sent
    last_reset_at       TIMESTAMPTZ,          -- Used to clear warning flag on cycle reset

    PRIMARY KEY (organization_id, source)
);

-- Index for efficient limit enforcement (aggregate across all sources)
CREATE INDEX idx_rate_limits_org ON rate_limits(organization_id);
```

---

## 6. Forecast & Calibration Schema

```sql
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

-- Calibration Candidates: Stores calibration updates that failed safety bounds checks.
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

---

## 6.5 Consolidated Schema (Hoisted from Sub-Documents)

The following tables are hoisted from sub-documents to establish a Single Source of Truth.

### Password Resets (from `05f-api-auth.md`)

```sql
CREATE TABLE password_resets (
    id            SERIAL PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash    TEXT NOT NULL,       -- bcrypt hash of the token sent via email
    expires_at    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    used          BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX idx_password_resets_user ON password_resets(user_id);
CREATE INDEX idx_password_resets_expiry ON password_resets(expires_at);
```

### OAuth Provider Support (from `05f-api-auth.md`)

```sql
ALTER TABLE users
ADD COLUMN auth_provider VARCHAR(20),       -- 'google', 'github', 'email' (default)
ADD COLUMN auth_provider_id VARCHAR(100),   -- External Subject ID
ADD COLUMN invite_token_hash TEXT,          -- bcrypt hash of invite token
ADD COLUMN invite_expires_at TIMESTAMPTZ;

CREATE INDEX idx_users_provider ON users(auth_provider, auth_provider_id);
```

### Billing Extensions (from `05e-api-billing.md`)

NOTE: Using VARCHAR with CHECK constraint instead of native ENUM for portability.

```sql
ALTER TABLE organizations
    -- Subscription status: VARCHAR with CHECK (No native ENUM - see policy in 01-foundation-types.md)
    ADD COLUMN subscription_status VARCHAR(30) DEFAULT 'active'
        CHECK (subscription_status IN ('active', 'past_due', 'canceled', 'incomplete', 'incomplete_expired', 'trialing', 'unpaid')),

    -- Used for Optimistic Locking to handle out-of-order webhooks
    ADD COLUMN last_subscription_event_at TIMESTAMPTZ,

    -- Used to track grace periods (e.g., 7 days after failure before pause)
    ADD COLUMN payment_failed_at TIMESTAMPTZ,

    -- Digest scheduling support (from 09-scheduled-jobs.md)
    ADD COLUMN last_digest_generated_at TIMESTAMPTZ,
    ADD COLUMN last_billing_sync_at TIMESTAMPTZ,

    -- Overage tracking: Set when usage exceeds plan limit.
    -- Tracks start of usage grace period (14 days). Set/Cleared by UsageAggregator.
    ADD COLUMN overage_started_at TIMESTAMPTZ;

CREATE INDEX idx_org_sub_status ON organizations(subscription_status);
CREATE INDEX idx_org_payment_failed ON organizations(payment_failed_at) WHERE payment_failed_at IS NOT NULL;
CREATE INDEX idx_org_digest_schedule ON organizations((notification_preferences->'digest'->>'enabled'), last_digest_generated_at);
```

### Usage History (from `09-scheduled-jobs.md`)

```sql
-- Usage History for Billing (SCHED-002)
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
```

### Verification Results (from `09-scheduled-jobs.md`)

```sql
-- Verification Results (OBS-005)
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
```

### Job Locks & History (from `09-scheduled-jobs.md`)

```sql
-- Job Locks for Idempotency
CREATE TABLE job_locks (
    id          TEXT PRIMARY KEY,   -- Deterministic Key (task_timestamp)
    worker_id   TEXT NOT NULL,      -- AWS Request ID
    locked_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL
);

-- Job History for Observability
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

-- Index for Archiver Performance
CREATE INDEX idx_watchpoints_archive_candidates
ON watchpoints(time_window_end)
WHERE status = 'active' AND time_window_end IS NOT NULL;
```

---

## 6.6 Roles & Permissions (from `12-operations.md`)

Support role for read-only access used by Support staff and debugging.

```sql
-- Admin Roles Migration: 099_admin_roles.up.sql
CREATE ROLE watchpoint_support_ro NOLOGIN;
GRANT CONNECT ON DATABASE watchpoint TO watchpoint_support_ro;
GRANT USAGE ON SCHEMA public TO watchpoint_support_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO watchpoint_support_ro;
```
```

---

## 7. Triggers & Functions

### 7.1 Config Version Auto-Increment

Ensures `config_version` increments only when logic-affecting fields change.

**IMPORTANT**: This trigger MUST EXCLUDE the `channels` column. Updates to delivery
configuration (such as rotating a webhook secret or updating channel URLs) do NOT
invalidate meteorological evaluation state. The `config_version` is used by the Eval
Worker to detect when conditions/logic have changed and cached evaluation results
should be discarded. Channel configuration changes affect only delivery, not evaluation.

```sql
CREATE OR REPLACE FUNCTION increment_config_version()
RETURNS TRIGGER AS $$
BEGIN
    -- NOTE: The 'channels' column is intentionally EXCLUDED from this check.
    -- Channel config updates (e.g., secret rotation) do not affect evaluation logic
    -- and should not invalidate cached evaluation state.
    IF (OLD.conditions IS DISTINCT FROM NEW.conditions) OR
       (OLD.condition_logic IS DISTINCT FROM NEW.condition_logic) OR
       (OLD.time_window_start IS DISTINCT FROM NEW.time_window_start) OR
       (OLD.time_window_end IS DISTINCT FROM NEW.time_window_end) OR
       (OLD.monitor_config IS DISTINCT FROM NEW.monitor_config) OR
       (OLD.preferences IS DISTINCT FROM NEW.preferences)
    THEN
        NEW.config_version := OLD.config_version + 1;
    END IF;
    NEW.updated_at := NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_watchpoints_config_version
BEFORE UPDATE ON watchpoints
FOR EACH ROW
EXECUTE FUNCTION increment_config_version();
```

---

## 8. Migration Strategy

Migrations are managed via `golang-migrate`. Files are versioned sequentially.

| File | Purpose |
|---|---|
| `001_extensions.up.sql` | Enable `uuid-ossp`, `cube`, `earthdistance`. |
| `002_organizations.up.sql` | Create `organizations`, `users`, `rate_limits`. |
| `003_watchpoints.up.sql` | Create `watchpoints` table and indexes. |
| `004_evaluation.up.sql` | Create `watchpoint_evaluation_state`. |
| `005_notifications.up.sql` | Create `notifications`, `notification_deliveries`. |
| `006_system.up.sql` | Create `api_keys`, `sessions`, `security_events`, `audit_log`, `idempotency_keys`. |
| `007_forecasts.up.sql` | Create `forecast_runs`, `calibration_coefficients`. |
| `008_triggers.up.sql` | Create `increment_config_version` function and trigger. |

---

## 9. Repository Interfaces

These interfaces define the contract between the application layer and the database.

### 9.1 Transaction Management

```go
type TransactionManager interface {
    // RunInTx executes fn within a transaction. Rolls back on error.
    RunInTx(ctx context.Context, fn func(ctx context.Context, repos RepositoryRegistry) error) error
}

type RepositoryRegistry interface {
    WatchPoints() WatchPointRepository
    Organizations() OrganizationRepository
    Users() UserRepository
    Notifications() NotificationRepository
    ForecastRuns() ForecastRunRepository
    Audit() AuditRepository
    Verification() VerificationRepository
}
```

### 9.2 Domain Repositories

```go
type WatchPointRepository interface {
    Create(ctx context.Context, wp *types.WatchPoint) error
    GetByID(ctx context.Context, id string) (*types.WatchPoint, error)
    Update(ctx context.Context, wp *types.WatchPoint) error
    Delete(ctx context.Context, id string) error // Soft delete

    // Dashboard Optimization (DASH-008)
    // ListSummaries returns lightweight WatchPointSummary DTOs for dashboard performance.
    // Uses LEFT JOIN on watchpoint_evaluation_state to include runtime state.
    // Excludes heavy JSONB fields (conditions, channels) from the query.
    ListSummaries(ctx context.Context, orgID string, filter ListWatchPointsParams) ([]types.WatchPointSummary, types.PageInfo, error)

    // GetStats returns aggregated counts for dashboard summary cards.
    // Query: SELECT status, previous_trigger_state, COUNT(*) ... GROUP BY ...
    GetStats(ctx context.Context, orgID string) (*types.WatchPointStats, error)

    // UpdateChannelConfig performs atomic JSONB update on a specific channel.
    // Uses Postgres jsonb_set function to update channel config without overwriting the entire array.
    // The mutationFn receives the current channel config and returns the updated config.
    // This enables zero-downtime secret rotation without race conditions.
    //
    // Example SQL pattern:
    //   UPDATE watchpoints SET channels = jsonb_set(
    //     channels,
    //     (SELECT ('{' || idx-1 || ',config}')::text[] FROM ... WHERE channels->idx-1->>'id' = $channelID),
    //     $newConfig
    //   ) WHERE id = $wpID
    UpdateChannelConfig(ctx context.Context, wpID string, channelID string, mutationFn func(map[string]any) (map[string]any, error)) error

    // Batcher & Worker Methods
    GetTileCounts(ctx context.Context) (map[string]int, error)
    ListByTile(ctx context.Context, tileID string, limit, offset int) ([]*types.WatchPoint, error)

    // Lifecycle
    ArchiveExpired(ctx context.Context, cutoff time.Time) (int64, error)

    // PruneSeenThreats removes expired threat entries from the seen_threats JSONB array
    // in watchpoint_evaluation_state. Supports MAINT-007 by trimming stale threat hashes
    // older than maxAge to prevent unbounded growth of deduplication state.
    PruneSeenThreats(ctx context.Context, maxAge time.Duration) (int64, error)

    // Bulk Operations
    // PauseAllByOrgID sets status='paused' for all active WatchPoints belonging to the org.
    // Used during organization deletion to immediately stop evaluation overhead.
    // The reason parameter distinguishes user pauses from billing enforcements.
    PauseAllByOrgID(ctx context.Context, orgID string, reason string) error

    // PauseExcessByOrgID pauses the most recently created WatchPoints until count <= limit.
    // Uses **LIFO (Last-In-First-Out)** strategy, pausing the *most recently created*
    // WatchPoints first to preserve long-standing monitors.
    // Sets paused_reason='billing_delinquency'. Used by SubscriptionEnforcer for overage enforcement.
    PauseExcessByOrgID(ctx context.Context, orgID string, limit int) error

    // ResumeAllByOrgID resumes only WatchPoints paused for the specific reason
    // (e.g., billing_delinquency) to prevent reactivating user-paused items.
    ResumeAllByOrgID(ctx context.Context, orgID string, reason string) error

    // UpdateStatusBatch updates the status of multiple WatchPoints in a single transaction.
    // Executes: UPDATE watchpoints SET status=$status, updated_at=NOW()
    //           WHERE id = ANY($ids) AND organization_id = $orgID AND test_mode = $testMode
    // The testMode parameter enforces strict environment isolation during batch operations,
    // ensuring Live and Test Mode WatchPoints are never mixed in a single batch update.
    UpdateStatusBatch(ctx context.Context, ids []string, orgID string, status types.Status, testMode bool) (int64, error)

    // DeleteBatch soft-deletes multiple WatchPoints in a single transaction.
    // Executes: UPDATE watchpoints SET deleted_at=NOW(), status='archived', updated_at=NOW()
    //           WHERE id = ANY($ids) AND organization_id = $orgID AND test_mode = $testMode
    // Setting status to 'archived' ensures immediate exclusion from Batcher active indexes.
    // The testMode parameter enforces strict environment isolation during batch operations,
    // ensuring Live and Test Mode WatchPoints are never mixed in a single batch deletion.
    DeleteBatch(ctx context.Context, ids []string, orgID string, testMode bool) (int64, error)

    // UpdateTagsBatch atomically updates tags for WatchPoints matching the given filter.
    // Generates SQL: UPDATE watchpoints SET tags = (tags || $addTags) - $removeTags, updated_at=NOW()
    //                WHERE organization_id = $orgID AND [filter conditions]
    // Uses PostgreSQL array operators for atomic tag manipulation without fetch-modify-save loops.
    // Returns the count of WatchPoints updated.
    UpdateTagsBatch(ctx context.Context, orgID string, filter types.BulkFilter, addTags []string, removeTags []string) (int64, error)

    // GetBatch performs a vectorized fetch of multiple WatchPoints by ID.
    // Executes: SELECT * FROM watchpoints WHERE id = ANY($ids) AND organization_id = $orgID
    // Used by bulk cloning operations to efficiently retrieve source WatchPoints in a single query
    // rather than N individual fetches.
    GetBatch(ctx context.Context, ids []string, orgID string) ([]*types.WatchPoint, error)
}

type OrganizationRepository interface {
    Create(ctx context.Context, org *types.Organization) error
    GetByID(ctx context.Context, id string) (*types.Organization, error)
    UpdatePlan(ctx context.Context, id string, plan types.PlanTier, limits types.PlanLimits) error
    IncrementRateLimit(ctx context.Context, id string) (int, error)
}

type UserRepository interface {
    GetByID(ctx context.Context, id string, orgID string) (*types.User, error)
    GetByEmail(ctx context.Context, email string) (*types.User, error)

    // GetOwnerEmail returns the email address of an Owner-role user for the given organization.
    // Used for system-level alerts (e.g., notifying owner of email channel failures).
    GetOwnerEmail(ctx context.Context, orgID string) (string, error)
}

type NotificationRepository interface {
    Create(ctx context.Context, n *types.Notification) error
    CreateDelivery(ctx context.Context, d *types.NotificationDelivery) error
    UpdateDeliveryStatus(ctx context.Context, deliveryID string, status string, reason string) error
    GetPendingDeliveries(ctx context.Context, limit int) ([]*types.NotificationDelivery, error)

    // List retrieves notification history with filtering support.
    // Joins `notifications` with `notification_deliveries` to populate the Channels list.
    // Supports both WatchPoint-scoped (filter.WatchPointID set) and Organization-scoped
    // (filter.WatchPointID empty) queries.
    List(ctx context.Context, filter types.NotificationFilter) ([]*types.NotificationHistoryItem, types.PageInfo, error)

    // CancelDeferredDeliveries sets status='skipped' for all 'deferred' deliveries
    // associated with this WatchPoint. Used during Resume to prevent stale alert floods.
    CancelDeferredDeliveries(ctx context.Context, watchpointID string) error

    // DeleteBefore hard-deletes notifications older than the cutoff time.
    // Used for retention cleanup (MAINT-003). Returns the count of deleted records.
    DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

type ForecastRunRepository interface {
    Create(ctx context.Context, run *types.ForecastRun) error
    MarkComplete(ctx context.Context, id string, storagePath string, durationMs int) error
    MarkFailed(ctx context.Context, id string, reason string) error
    // GetLatestServing finds the most recent 'complete' run. Used to check staleness.
    GetLatestServing(ctx context.Context, model types.ForecastType) (*types.ForecastRun, error)
    // UpdateExternalID is used when re-triggering a job to save the new RunPod ID
    UpdateExternalID(ctx context.Context, id string, externalID string) error
}

type AuditRepository interface {
    Log(ctx context.Context, entry *types.AuditEvent) error
    List(ctx context.Context, params types.AuditQueryFilters) ([]*types.AuditEvent, types.PageInfo, error)

    // ListOlderThan retrieves audit events older than cutoff for archival.
    // Used by ArchiveAuditLogs to fetch records before uploading to cold storage (MAINT-004).
    ListOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]*types.AuditEvent, error)

    // DeleteIDs hard-deletes audit records by ID after successful archival to S3.
    // Used as part of the fetch-upload-delete cycle in ArchiveAuditLogs.
    DeleteIDs(ctx context.Context, ids []string) error
}

// VerificationRepository provides access to aggregated forecast verification metrics.
type VerificationRepository interface {
    // GetAggregatedMetrics performs SQL-level aggregation (AVG, GROUP BY) on the
    // verification_results table to produce metrics for the dashboard.
    // Returns aggregated metrics grouped by variable and metric type for the specified
    // model and time range.
    GetAggregatedMetrics(ctx context.Context, model types.ForecastType, start, end time.Time) ([]types.VerificationMetric, error)
}

// SecurityRepository provides access to security event tracking for abuse prevention.
// Operates on the 'security_events' table defined in Section 5.2.
type SecurityRepository interface {
    // LogAttempt records a security event (login attempt, API auth attempt, etc.)
    LogAttempt(ctx context.Context, event *types.SecurityEvent) error

    // CountRecentFailuresByIP returns the count of failed attempts from an IP
    // address within the specified time window. Used for IP-based blocking.
    CountRecentFailuresByIP(ctx context.Context, ip string, since time.Time) (int, error)

    // CountRecentFailuresByIdentifier returns the count of failed attempts for
    // a specific identifier (e.g., email) within the specified time window.
    // Used for account-level lockouts.
    CountRecentFailuresByIdentifier(ctx context.Context, identifier string, since time.Time) (int, error)
}
```