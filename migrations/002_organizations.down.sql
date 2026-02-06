-- 002_organizations.down.sql
-- Drop tables in reverse dependency order.

DROP TABLE IF EXISTS rate_limits;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS organizations;
