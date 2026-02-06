-- 008_triggers.down.sql
-- Drop the config version trigger and function.

DROP TRIGGER IF EXISTS trg_watchpoints_config_version ON watchpoints;
DROP FUNCTION IF EXISTS increment_config_version();
