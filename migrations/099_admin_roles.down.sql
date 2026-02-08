-- 099_admin_roles.down.sql
-- Revoke grants and drop the support role.

REVOKE SELECT ON ALL TABLES IN SCHEMA public FROM watchpoint_support_ro;
REVOKE USAGE ON SCHEMA public FROM watchpoint_support_ro;
DO $$
BEGIN
    EXECUTE format('REVOKE CONNECT ON DATABASE %I FROM watchpoint_support_ro', current_database());
END
$$;
DROP ROLE IF EXISTS watchpoint_support_ro;
