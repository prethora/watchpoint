-- 099_admin_roles.down.sql
-- Revoke grants and drop the support role.

REVOKE SELECT ON ALL TABLES IN SCHEMA public FROM watchpoint_support_ro;
REVOKE USAGE ON SCHEMA public FROM watchpoint_support_ro;
REVOKE CONNECT ON DATABASE watchpoint FROM watchpoint_support_ro;
DROP ROLE IF EXISTS watchpoint_support_ro;
