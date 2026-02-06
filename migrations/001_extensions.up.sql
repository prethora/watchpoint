-- 001_extensions.up.sql
-- Enable required PostgreSQL extensions for the WatchPoint platform.

-- UUID generation (gen_random_uuid())
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- Prerequisite for earthdistance
CREATE EXTENSION IF NOT EXISTS "cube";

-- Geospatial queries (finding WatchPoints near a location)
CREATE EXTENSION IF NOT EXISTS "earthdistance";
