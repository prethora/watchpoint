-- 006_system.down.sql
-- Drop system tables in reverse dependency order.

DROP TABLE IF EXISTS password_resets;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS security_events;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS api_keys;
