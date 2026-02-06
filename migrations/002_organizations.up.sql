-- 002_organizations.up.sql
-- Create organizations, users, and rate_limits tables.

-- =============================================================================
-- Organizations
-- =============================================================================
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

-- =============================================================================
-- Users
-- =============================================================================
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

-- =============================================================================
-- Rate Limits
-- =============================================================================
-- Separated from Organizations for high-churn write performance.
-- Supports per-source usage attribution for vertical apps (VERT-001).
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
