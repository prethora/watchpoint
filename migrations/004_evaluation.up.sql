-- 004_evaluation.up.sql
-- Create watchpoint_evaluation_state table.
-- Tracks the runtime state of each WatchPoint. Separated from watchpoints to allow
-- high-frequency updates without locking configuration.

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
