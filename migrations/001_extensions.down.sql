-- 001_extensions.down.sql
-- Remove extensions in reverse dependency order.

-- earthdistance depends on cube, so drop it first
DROP EXTENSION IF EXISTS "earthdistance";
DROP EXTENSION IF EXISTS "cube";
DROP EXTENSION IF EXISTS "uuid-ossp";
