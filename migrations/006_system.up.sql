-- 006_system.up.sql
-- Create api_keys, sessions, security_events, audit_log, idempotency_keys, and password_resets tables.

-- =============================================================================
-- API Keys
-- =============================================================================
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

-- =============================================================================
-- Sessions
-- =============================================================================
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

-- =============================================================================
-- Security Events (Unified Abuse Protection)
-- =============================================================================
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

-- =============================================================================
-- Audit Log
-- =============================================================================
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

-- =============================================================================
-- Idempotency Keys
-- =============================================================================
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

-- =============================================================================
-- Password Resets (from 05f-api-auth.md, hoisted to 02-foundation-db.md Section 6.5)
-- =============================================================================
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
