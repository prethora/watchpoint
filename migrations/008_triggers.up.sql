-- 008_triggers.up.sql
-- Create the increment_config_version trigger function and attach it to watchpoints.

-- =============================================================================
-- Config Version Auto-Increment Function
-- =============================================================================
-- Ensures config_version increments only when logic-affecting fields change.
--
-- IMPORTANT: The 'channels' column is intentionally EXCLUDED from this check.
-- Updates to delivery configuration (such as rotating a webhook secret or
-- updating channel URLs) do NOT invalidate meteorological evaluation state.
-- The config_version is used by the Eval Worker to detect when conditions/logic
-- have changed and cached evaluation results should be discarded. Channel
-- configuration changes affect only delivery, not evaluation.

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
