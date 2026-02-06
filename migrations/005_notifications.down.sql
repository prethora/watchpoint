-- 005_notifications.down.sql
-- Drop notification tables in reverse dependency order.

DROP TABLE IF EXISTS notification_deliveries;
DROP TABLE IF EXISTS notifications;
