-- 007_forecasts.down.sql
-- Drop forecast and system tables in reverse dependency order.

-- Drop the archiver index on watchpoints (added by this migration)
DROP INDEX IF EXISTS idx_watchpoints_archive_candidates;

DROP TABLE IF EXISTS usage_history;
DROP TABLE IF EXISTS job_history;
DROP TABLE IF EXISTS job_locks;
DROP TABLE IF EXISTS verification_results;
DROP TABLE IF EXISTS calibration_coefficients;
DROP TABLE IF EXISTS calibration_candidates;
DROP TABLE IF EXISTS forecast_runs;
