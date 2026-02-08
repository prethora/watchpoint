-- 099_admin_roles.up.sql
-- Create the watchpoint_support_ro role for read-only support access.

-- =============================================================================
-- Support Role (Read-Only)
-- =============================================================================
-- Used by Support staff and for debugging. See 12-operations.md.
-- DO NOT REPLACE IF EXISTS is used to make this migration idempotent;
-- the role may already exist in shared database environments.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'watchpoint_support_ro') THEN
        CREATE ROLE watchpoint_support_ro NOLOGIN;
    END IF;
END
$$;

DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO watchpoint_support_ro', current_database());
END
$$;
GRANT USAGE ON SCHEMA public TO watchpoint_support_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO watchpoint_support_ro;
