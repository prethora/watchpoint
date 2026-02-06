-- 005_notifications.up.sql
-- Create notifications and notification_deliveries tables.

-- =============================================================================
-- Notifications
-- =============================================================================
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

-- =============================================================================
-- Notification Deliveries
-- =============================================================================
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
