// Package scheduler implements scheduled job services for the WatchPoint platform.
//
// This file implements the maintenance services from architecture/09-scheduled-jobs.md
// Sections 7.4, 7.6, 7.8, and 7.9. It provides:
//
//   - CleanupService: Hard deletion of soft-deleted orgs, expired invites,
//     archived WatchPoints, old notifications, audit log archival, expired
//     idempotency keys, and stale security events.
//   - ArchiverService: Transitions expired Event Mode WatchPoints to archived
//     status and enqueues post-event summary generation.
//   - TierTransitionService: Enforces data retention policies on S3 forecast data,
//     deleting old objects and updating forecast_runs status.
//   - DeferredNotificationService: Re-queues notifications deferred by Quiet Hours
//     once the quiet period ends.
//
// All services use fixed batch sizes to prevent Lambda timeouts and follow the
// pattern of accepting a `now` parameter for deterministic testing and manual
// backfill via MaintenancePayload.ReferenceTime.
//
// Flows: WPLC-009, MAINT-001, MAINT-002, MAINT-003, MAINT-004, MAINT-005,
//
//	MAINT-006, MAINT-007, NOTIF-002, SCHED-006
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"watchpoint/internal/types"
)

// -----------------------------------------------------------------------------
// Cleanup Service
// -----------------------------------------------------------------------------

// CleanupDB defines the database operations needed by the CleanupService.
// Each method maps to a specific maintenance flow per the architecture spec.
type CleanupDB interface {
	// ListSoftDeletedOrgIDs returns organization IDs where deleted_at < cutoff.
	// Used by PurgeSoftDeletedOrgs (MAINT-005).
	//
	// SQL: SELECT id FROM organizations WHERE deleted_at IS NOT NULL
	//      AND deleted_at < $1 LIMIT $2
	ListSoftDeletedOrgIDs(ctx context.Context, cutoff time.Time, limit int) ([]string, error)

	// HardDeleteOrg permanently deletes an organization by ID.
	// Postgres ON DELETE CASCADE removes child records (users, watchpoints,
	// api_keys, sessions, rate_limits). Audit logs are SET NULL.
	//
	// SQL: DELETE FROM organizations WHERE id = $1
	HardDeleteOrg(ctx context.Context, orgID string) error

	// DeleteExpiredInvites removes user records with status='invited' past expiry.
	// Returns the count of deleted rows (MAINT-006).
	//
	// SQL: DELETE FROM users WHERE status = 'invited' AND invite_expires_at < $1
	DeleteExpiredInvites(ctx context.Context, now time.Time) (int, error)

	// ListArchivedWatchPointIDs returns IDs of WatchPoints archived before cutoff.
	// Used by PurgeArchivedWatchPoints (MAINT-002).
	//
	// SQL: SELECT id FROM watchpoints WHERE status = 'archived'
	//      AND archived_at < $1 LIMIT $2
	ListArchivedWatchPointIDs(ctx context.Context, cutoff time.Time, limit int) ([]string, error)

	// HardDeleteWatchPoints permanently deletes WatchPoints by ID.
	// Postgres ON DELETE CASCADE removes evaluation_state, notifications,
	// and notification_deliveries.
	//
	// SQL: DELETE FROM watchpoints WHERE id = ANY($1)
	HardDeleteWatchPoints(ctx context.Context, ids []string) (int, error)

	// DeleteNotificationsBefore removes notifications older than cutoff.
	// Postgres ON DELETE CASCADE removes notification_deliveries (MAINT-003).
	//
	// SQL: DELETE FROM notifications WHERE created_at < $1
	DeleteNotificationsBefore(ctx context.Context, cutoff time.Time) (int, error)

	// ListAuditLogsOlderThan returns audit log entries older than cutoff.
	// Used by ArchiveAuditLogs (MAINT-004).
	ListAuditLogsOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]AuditLogEntry, error)

	// DeleteAuditLogsByIDs removes audit log entries by their IDs.
	//
	// SQL: DELETE FROM audit_log WHERE id = ANY($1)
	DeleteAuditLogsByIDs(ctx context.Context, ids []int64) (int, error)

	// DeleteExpiredIdempotencyKeys removes idempotency records past expiration.
	// Returns the count of deleted rows (SCHED-006).
	//
	// SQL: DELETE FROM idempotency_keys WHERE expires_at < $1
	DeleteExpiredIdempotencyKeys(ctx context.Context, now time.Time) (int, error)

	// DeleteSecurityEventsBefore removes security events older than cutoff.
	// Returns the count of deleted rows (MAINT-007).
	//
	// SQL: DELETE FROM security_events WHERE attempted_at < $1
	DeleteSecurityEventsBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// AuditLogEntry represents a single audit log record used during archival.
// This is a local type scoped to the scheduler package for the MAINT-004 flow.
type AuditLogEntry struct {
	ID             int64           `json:"id"`
	OrganizationID *string         `json:"organization_id,omitempty"`
	ActorID        *string         `json:"actor_id,omitempty"`
	Action         string          `json:"action"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     string          `json:"resource_id"`
	Metadata       map[string]any  `json:"metadata,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// AuditLogArchiver abstracts the S3 upload for audit log archival.
// The CleanupService uses this to upload serialized audit logs to cold storage.
type AuditLogArchiver interface {
	// UploadArchive uploads a batch of audit logs to S3 (Glacier tier).
	// The key is generated by the service: "audit/YYYY/MM/batch_{uuid}.jsonl.gz"
	// Returns the S3 key on success.
	UploadArchive(ctx context.Context, key string, data []byte) error
}

// cleanupService implements CleanupService from architecture/09-scheduled-jobs.md
// Section 7.4.
type cleanupService struct {
	db       CleanupDB
	archiver AuditLogArchiver // nil if audit archival not configured
	logger   *slog.Logger
}

// NewCleanupService creates a new CleanupService.
// The archiver parameter may be nil if audit log archival to S3 is not configured.
func NewCleanupService(db CleanupDB, archiver AuditLogArchiver, logger *slog.Logger) *cleanupService {
	if logger == nil {
		logger = slog.Default()
	}
	return &cleanupService{
		db:       db,
		archiver: archiver,
		logger:   logger,
	}
}

// PurgeSoftDeletedOrgs permanently removes organizations that were soft-deleted
// more than `retention` time ago. Uses a batch limit to prevent runaway deletion.
//
// Per MAINT-005 flow simulation:
//  1. SELECT id FROM organizations WHERE deleted_at < cutoff LIMIT limit.
//  2. For each ID, DELETE FROM organizations WHERE id = $id.
//  3. Postgres CASCADE deletes users, watchpoints, api_keys, sessions, rate_limits.
//  4. Audit logs are SET NULL (preserved but anonymized).
//
// Returns the total number of organizations permanently deleted.
func (c *cleanupService) PurgeSoftDeletedOrgs(ctx context.Context, now time.Time, retention time.Duration, limit int) (int, error) {
	cutoff := now.Add(-retention)

	ids, err := c.db.ListSoftDeletedOrgIDs(ctx, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("listing soft-deleted orgs: %w", err)
	}

	if len(ids) == 0 {
		c.logger.InfoContext(ctx, "no soft-deleted orgs to purge")
		return 0, nil
	}

	c.logger.InfoContext(ctx, "purging soft-deleted orgs",
		"count", len(ids),
		"cutoff", cutoff.Format(time.RFC3339),
	)

	deleted := 0
	for _, orgID := range ids {
		if err := c.db.HardDeleteOrg(ctx, orgID); err != nil {
			c.logger.ErrorContext(ctx, "failed to hard delete org",
				"org_id", orgID,
				"error", err,
			)
			// Continue with other orgs; this one will be retried next run.
			continue
		}
		deleted++
	}

	c.logger.InfoContext(ctx, "soft-deleted org purge complete",
		"deleted", deleted,
	)

	return deleted, nil
}

// PurgeExpiredInvites removes user records with status='invited' that have passed
// their invite_expires_at time.
//
// Per MAINT-006 flow simulation:
//
//	DELETE FROM users WHERE status = 'invited' AND invite_expires_at < now
//
// Returns the count of deleted invite records.
func (c *cleanupService) PurgeExpiredInvites(ctx context.Context, now time.Time) (int, error) {
	count, err := c.db.DeleteExpiredInvites(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("deleting expired invites: %w", err)
	}

	if count > 0 {
		c.logger.InfoContext(ctx, "purged expired invites",
			"count", count,
		)
	}

	return count, nil
}

// PurgeArchivedWatchPoints hard-deletes WatchPoints that have been archived
// longer than the retention period.
//
// Per MAINT-002 flow simulation:
//  1. SELECT id FROM watchpoints WHERE status='archived' AND archived_at < cutoff LIMIT limit.
//  2. DELETE FROM watchpoints WHERE id = ANY(ids).
//  3. Postgres CASCADE deletes evaluation_state, notifications, notification_deliveries.
//
// Returns the count of permanently deleted WatchPoints.
func (c *cleanupService) PurgeArchivedWatchPoints(ctx context.Context, now time.Time, retention time.Duration, limit int) (int, error) {
	cutoff := now.Add(-retention)

	ids, err := c.db.ListArchivedWatchPointIDs(ctx, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("listing archived watchpoints: %w", err)
	}

	if len(ids) == 0 {
		c.logger.InfoContext(ctx, "no archived watchpoints to purge")
		return 0, nil
	}

	deleted, err := c.db.HardDeleteWatchPoints(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("hard deleting watchpoints: %w", err)
	}

	c.logger.InfoContext(ctx, "purged archived watchpoints",
		"deleted", deleted,
	)

	return deleted, nil
}

// PurgeNotifications hard-deletes notifications older than the retention period.
// Delegates to the database for the actual deletion. Cascade removes
// notification_deliveries automatically.
//
// Per MAINT-003 flow simulation:
//
//	DELETE FROM notifications WHERE created_at < cutoff
//
// Returns the count of deleted notification records.
func (c *cleanupService) PurgeNotifications(ctx context.Context, retention time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-retention)

	count, err := c.db.DeleteNotificationsBefore(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("deleting old notifications: %w", err)
	}

	if count > 0 {
		c.logger.InfoContext(ctx, "purged old notifications",
			"count", count,
		)
	}

	return count, nil
}

// ArchiveAuditLogs moves audit records older than retention to cold storage (S3).
// Orchestrates a fetch-upload-delete cycle in batches.
//
// Per MAINT-004 flow simulation:
//  1. Fetch batch via ListAuditLogsOlderThan.
//  2. Serialize to JSONL and compress (handled by archiver).
//  3. Upload to S3 ArchiveBucket (Glacier tier).
//  4. Delete archived records via DeleteAuditLogsByIDs.
//
// Returns the count of records successfully archived.
func (c *cleanupService) ArchiveAuditLogs(ctx context.Context, retention time.Duration, batchSize int) (int, error) {
	if c.archiver == nil {
		c.logger.WarnContext(ctx, "audit log archiver not configured, skipping")
		return 0, nil
	}

	cutoff := time.Now().UTC().Add(-retention)
	totalArchived := 0

	for {
		entries, err := c.db.ListAuditLogsOlderThan(ctx, cutoff, batchSize)
		if err != nil {
			return totalArchived, fmt.Errorf("listing audit logs for archival: %w", err)
		}

		if len(entries) == 0 {
			break
		}

		// Serialize to JSONL format.
		data, err := serializeAuditLogsJSONL(entries)
		if err != nil {
			return totalArchived, fmt.Errorf("serializing audit logs: %w", err)
		}

		// Generate the S3 key.
		key := fmt.Sprintf("audit/%d/%02d/batch_%d.jsonl",
			cutoff.Year(), cutoff.Month(), time.Now().UnixNano())

		// Upload to S3.
		if err := c.archiver.UploadArchive(ctx, key, data); err != nil {
			return totalArchived, fmt.Errorf("uploading audit archive to %s: %w", key, err)
		}

		// Delete archived records from DB.
		ids := make([]int64, len(entries))
		for i, e := range entries {
			ids[i] = e.ID
		}

		deleted, err := c.db.DeleteAuditLogsByIDs(ctx, ids)
		if err != nil {
			return totalArchived, fmt.Errorf("deleting archived audit logs: %w", err)
		}

		totalArchived += deleted

		c.logger.InfoContext(ctx, "archived audit log batch",
			"batch_size", deleted,
			"s3_key", key,
			"total_archived", totalArchived,
		)

		// If we got fewer than the batch size, we are done.
		if len(entries) < batchSize {
			break
		}
	}

	return totalArchived, nil
}

// PurgeExpiredIdempotencyKeys removes idempotency records past their expiration.
// Prevents unbounded growth of the idempotency_keys table.
//
// Per SCHED-006:
//
//	DELETE FROM idempotency_keys WHERE expires_at < now
//
// Returns the count of records deleted.
func (c *cleanupService) PurgeExpiredIdempotencyKeys(ctx context.Context, now time.Time) (int, error) {
	count, err := c.db.DeleteExpiredIdempotencyKeys(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("deleting expired idempotency keys: %w", err)
	}

	if count > 0 {
		c.logger.InfoContext(ctx, "purged expired idempotency keys",
			"count", count,
		)
	}

	return count, nil
}

// PurgeSecurityEvents removes security event records older than the retention period.
// Prevents unbounded growth of the security_events table used for abuse tracking.
//
// Per MAINT-007:
//
//	DELETE FROM security_events WHERE attempted_at < (now - retention)
//
// Default retention: 7 days.
// Returns the count of records deleted.
func (c *cleanupService) PurgeSecurityEvents(ctx context.Context, now time.Time, retention time.Duration) (int, error) {
	cutoff := now.Add(-retention)

	count, err := c.db.DeleteSecurityEventsBefore(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("deleting old security events: %w", err)
	}

	if count > 0 {
		c.logger.InfoContext(ctx, "purged old security events",
			"count", count,
			"cutoff", cutoff.Format(time.RFC3339),
		)
	}

	return count, nil
}

// serializeAuditLogsJSONL serializes audit log entries to newline-delimited JSON.
func serializeAuditLogsJSONL(entries []AuditLogEntry) ([]byte, error) {
	var buf []byte
	for i, entry := range entries {
		line, err := jsonMarshal(entry)
		if err != nil {
			return nil, fmt.Errorf("marshaling audit log entry %d: %w", entry.ID, err)
		}
		buf = append(buf, line...)
		if i < len(entries)-1 {
			buf = append(buf, '\n')
		}
	}
	return buf, nil
}

// jsonMarshal is a package-level var to allow testing without import cycles.
// In production this uses encoding/json.Marshal.
var jsonMarshal = json.Marshal

// -----------------------------------------------------------------------------
// Archiver Service
// -----------------------------------------------------------------------------

// ArchiverDB defines the database operations needed by the ArchiverService.
type ArchiverDB interface {
	// ArchiveExpiredWatchPoints marks expired Event Mode WatchPoints as archived.
	// Returns the archived WatchPoint IDs and their tile IDs for summary generation.
	//
	// SQL: UPDATE watchpoints
	//      SET status = 'archived', archived_at = $1, archived_reason = 'event_passed'
	//      WHERE status = 'active'
	//      AND time_window_end IS NOT NULL
	//      AND time_window_end < ($1 - $buffer)
	//      RETURNING id, tile_id, organization_id, name
	ArchiveExpiredWatchPoints(ctx context.Context, now time.Time, buffer time.Duration) ([]ArchivedWatchPoint, error)
}

// ArchivedWatchPoint holds the minimal data returned from the archive query.
// Used to construct summary generation messages.
type ArchivedWatchPoint struct {
	ID             string
	TileID         string
	OrganizationID string
	Name           string
}

// ArchiverSQSPublisher abstracts the SQS publish for archiver summary messages.
type ArchiverSQSPublisher interface {
	// PublishEvalMessage sends an EvalMessage to the evaluation queue.
	// Used to trigger post-event summary generation (OBS-007).
	PublishEvalMessage(ctx context.Context, msg types.EvalMessage) error
}

// archiverService implements ArchiverService from architecture/09-scheduled-jobs.md
// Section 7.6.
type archiverService struct {
	db        ArchiverDB
	publisher ArchiverSQSPublisher
	logger    *slog.Logger
}

// NewArchiverService creates a new ArchiverService.
func NewArchiverService(db ArchiverDB, publisher ArchiverSQSPublisher, logger *slog.Logger) *archiverService {
	if logger == nil {
		logger = slog.Default()
	}
	return &archiverService{
		db:        db,
		publisher: publisher,
		logger:    logger,
	}
}

// ArchiveExpired marks expired Event Mode WatchPoints as archived and enqueues
// EvalMessage with Action=generate_summary for post-event accuracy reports.
//
// Per WPLC-009 flow simulation:
//  1. UPDATE watchpoints SET status='archived' WHERE time_window_end < (now - buffer).
//  2. For each archived WP, enqueue EvalMessage with Action=generate_summary.
//  3. Emit audit events for visibility.
//
// The buffer parameter (typically 1 hour) provides a grace period after the event
// window ends before archiving.
//
// Returns the count of WatchPoints archived.
func (a *archiverService) ArchiveExpired(ctx context.Context, now time.Time, buffer time.Duration) (int64, error) {
	archived, err := a.db.ArchiveExpiredWatchPoints(ctx, now, buffer)
	if err != nil {
		return 0, fmt.Errorf("archiving expired watchpoints: %w", err)
	}

	if len(archived) == 0 {
		a.logger.InfoContext(ctx, "no expired watchpoints to archive")
		return 0, nil
	}

	a.logger.InfoContext(ctx, "archived expired watchpoints",
		"count", len(archived),
	)

	// Enqueue summary generation for each archived WatchPoint.
	enqueued := 0
	for _, wp := range archived {
		msg := types.EvalMessage{
			Action:                types.EvalActionGenerateSummary,
			TileID:                wp.TileID,
			SpecificWatchPointIDs: []string{wp.ID},
		}

		if err := a.publisher.PublishEvalMessage(ctx, msg); err != nil {
			a.logger.ErrorContext(ctx, "failed to enqueue summary generation",
				"watchpoint_id", wp.ID,
				"tile_id", wp.TileID,
				"error", err,
			)
			// Continue with other WPs; failed ones won't get summaries
			// but the archival is already committed.
			continue
		}
		enqueued++
	}

	a.logger.InfoContext(ctx, "summary generation messages enqueued",
		"enqueued", enqueued,
		"total_archived", len(archived),
	)

	return int64(len(archived)), nil
}

// -----------------------------------------------------------------------------
// Tier Transition Service (FCST-006: Forecast Cleanup)
// -----------------------------------------------------------------------------

// TierTransitionDB defines the database operations needed by the TierTransitionService.
type TierTransitionDB interface {
	// ListExpiredForecastRuns returns completed forecast runs that are older than
	// the given cutoff for a specific model type.
	//
	// SQL: SELECT id, storage_path FROM forecast_runs
	//      WHERE status = 'complete' AND model = $1 AND run_timestamp < $2
	//      LIMIT $3
	ListExpiredForecastRuns(ctx context.Context, model types.ForecastType, cutoff time.Time, limit int) ([]ExpiredForecastRun, error)

	// MarkForecastRunDeleted updates a forecast run's status to 'deleted' and
	// clears its storage_path.
	//
	// SQL: UPDATE forecast_runs SET status = 'deleted', storage_path = ''
	//      WHERE id = $1
	MarkForecastRunDeleted(ctx context.Context, id string) error
}

// ExpiredForecastRun holds the minimal data needed for tier transition.
type ExpiredForecastRun struct {
	ID          string
	StoragePath string
}

// S3Deleter abstracts S3 object deletion for the tier transition service.
type S3Deleter interface {
	// DeleteObjects removes objects from S3 by their storage path prefix.
	// The implementation should handle listing and deleting all objects under
	// the given prefix.
	DeleteObjects(ctx context.Context, storagePath string) error
}

// NowcastRetention is the default retention for nowcast forecast data (7 days).
// Per architecture/09-scheduled-jobs.md Section 7.8.
const NowcastRetention = 7 * 24 * time.Hour

// MediumRangeRetention is the default retention for medium-range forecast data (90 days).
// Per architecture/09-scheduled-jobs.md Section 7.8.
const MediumRangeRetention = 90 * 24 * time.Hour

// TierTransitionBatchLimit is the maximum number of forecast runs processed per
// model type per invocation.
const TierTransitionBatchLimit = 100

// tierTransitionService implements TierTransitionService from
// architecture/09-scheduled-jobs.md Section 7.8.
type tierTransitionService struct {
	db     TierTransitionDB
	s3     S3Deleter
	logger *slog.Logger
}

// NewTierTransitionService creates a new TierTransitionService.
func NewTierTransitionService(db TierTransitionDB, s3 S3Deleter, logger *slog.Logger) *tierTransitionService {
	if logger == nil {
		logger = slog.Default()
	}
	return &tierTransitionService{
		db:     db,
		s3:     s3,
		logger: logger,
	}
}

// EnforceRetention deletes old S3 forecast objects and marks their DB records
// as 'deleted'.
//
// Per MAINT-001 flow simulation:
//  1. Nowcast: delete if run_timestamp < (now - 7 days).
//  2. Medium-Range: delete if run_timestamp < (now - 90 days).
//  3. For each: Delete S3 objects, then update forecast_runs status to 'deleted'.
//
// Returns the total number of forecast runs transitioned.
func (t *tierTransitionService) EnforceRetention(ctx context.Context, now time.Time) (int, error) {
	totalDeleted := 0

	// Process Nowcast runs.
	nowcastDeleted, err := t.processModel(ctx, types.ForecastNowcast, now, NowcastRetention)
	if err != nil {
		t.logger.ErrorContext(ctx, "nowcast tier transition failed",
			"error", err,
		)
		// Continue to medium-range even if nowcast fails.
	} else {
		totalDeleted += nowcastDeleted
	}

	// Process Medium-Range runs.
	mrDeleted, err := t.processModel(ctx, types.ForecastMediumRange, now, MediumRangeRetention)
	if err != nil {
		t.logger.ErrorContext(ctx, "medium-range tier transition failed",
			"error", err,
		)
		// Don't fail the entire operation; partial progress is acceptable.
	} else {
		totalDeleted += mrDeleted
	}

	t.logger.InfoContext(ctx, "tier transition complete",
		"total_deleted", totalDeleted,
		"nowcast_deleted", nowcastDeleted,
	)

	return totalDeleted, nil
}

// processModel handles the tier transition for a single forecast model type.
func (t *tierTransitionService) processModel(
	ctx context.Context,
	model types.ForecastType,
	now time.Time,
	retention time.Duration,
) (int, error) {
	cutoff := now.Add(-retention)

	runs, err := t.db.ListExpiredForecastRuns(ctx, model, cutoff, TierTransitionBatchLimit)
	if err != nil {
		return 0, fmt.Errorf("listing expired %s runs: %w", model, err)
	}

	if len(runs) == 0 {
		t.logger.InfoContext(ctx, "no expired forecast runs to transition",
			"model", model,
		)
		return 0, nil
	}

	t.logger.InfoContext(ctx, "transitioning expired forecast runs",
		"model", model,
		"count", len(runs),
		"cutoff", cutoff.Format(time.RFC3339),
	)

	deleted := 0
	for _, run := range runs {
		// Step 1: Delete S3 objects.
		if run.StoragePath != "" {
			if err := t.s3.DeleteObjects(ctx, run.StoragePath); err != nil {
				t.logger.ErrorContext(ctx, "failed to delete S3 objects for forecast run",
					"run_id", run.ID,
					"storage_path", run.StoragePath,
					"error", err,
				)
				// Continue with other runs; this one can be retried.
				continue
			}
		}

		// Step 2: Update DB status to 'deleted'.
		if err := t.db.MarkForecastRunDeleted(ctx, run.ID); err != nil {
			t.logger.ErrorContext(ctx, "failed to mark forecast run as deleted",
				"run_id", run.ID,
				"error", err,
			)
			// S3 objects were deleted but DB not updated.
			// This is acceptable: S3 Lifecycle rules serve as backup,
			// and the run will be picked up again next invocation (still
			// has storage_path set). The MarkForecastRunDeleted will clear it.
			continue
		}

		deleted++
	}

	return deleted, nil
}

// -----------------------------------------------------------------------------
// Deferred Notification Service
// -----------------------------------------------------------------------------

// DeferredNotificationDB defines the database operations needed by the
// DeferredNotificationService.
type DeferredNotificationDB interface {
	// ListDeferredDeliveries returns notification deliveries with status='deferred'
	// and next_retry_at <= now.
	//
	// SQL: SELECT nd.id, nd.notification_id, n.payload
	//      FROM notification_deliveries nd
	//      JOIN notifications n ON n.id = nd.notification_id
	//      WHERE nd.status = 'deferred'
	//      AND nd.next_retry_at <= $1
	//      LIMIT $2
	ListDeferredDeliveries(ctx context.Context, now time.Time, limit int) ([]DeferredDelivery, error)

	// ResetDeliveryToPending atomically updates a delivery's status from
	// 'deferred' to 'pending'. Returns true if the update was applied
	// (status was actually 'deferred'), false if already changed by another process.
	//
	// SQL: UPDATE notification_deliveries
	//      SET status = 'pending'
	//      WHERE id = $1 AND status = 'deferred'
	ResetDeliveryToPending(ctx context.Context, deliveryID string) (bool, error)
}

// DeferredDelivery holds the data needed to re-enqueue a deferred notification.
type DeferredDelivery struct {
	DeliveryID     string
	NotificationID string
	Payload        map[string]any // Notification payload from the notifications table
}

// DeferredNotificationPublisher abstracts SQS publishing for deferred notifications.
type DeferredNotificationPublisher interface {
	// PublishNotification sends a notification message to the notification queue.
	// The payload is the original notification payload that was deferred.
	PublishNotification(ctx context.Context, payload map[string]any) error
}

// deferredNotificationService implements DeferredNotificationService from
// architecture/09-scheduled-jobs.md Section 7.9.
type deferredNotificationService struct {
	db        DeferredNotificationDB
	publisher DeferredNotificationPublisher
	logger    *slog.Logger
}

// NewDeferredNotificationService creates a new DeferredNotificationService.
func NewDeferredNotificationService(db DeferredNotificationDB, publisher DeferredNotificationPublisher, logger *slog.Logger) *deferredNotificationService {
	if logger == nil {
		logger = slog.Default()
	}
	return &deferredNotificationService{
		db:        db,
		publisher: publisher,
		logger:    logger,
	}
}

// RequeueDeferredNotifications finds deferred deliveries past their resume time
// and re-enqueues them to the notification queue for processing.
//
// Per NOTIF-002 flow simulation:
//  1. Query notification_deliveries WHERE status='deferred' AND next_retry_at <= now.
//  2. For each: reset status to 'pending' (atomic), re-enqueue to SQS.
//  3. Track count for metrics and job history.
//
// The status transition deferred -> pending is atomic. If the job runs twice,
// the second run finds no matching records (already transitioned).
//
// Returns the number of notifications successfully re-queued.
func (d *deferredNotificationService) RequeueDeferredNotifications(ctx context.Context, now time.Time, limit int) (int, error) {
	deliveries, err := d.db.ListDeferredDeliveries(ctx, now, limit)
	if err != nil {
		return 0, fmt.Errorf("listing deferred deliveries: %w", err)
	}

	if len(deliveries) == 0 {
		d.logger.InfoContext(ctx, "no deferred notifications to requeue")
		return 0, nil
	}

	d.logger.InfoContext(ctx, "requeuing deferred notifications",
		"count", len(deliveries),
	)

	requeued := 0
	for _, delivery := range deliveries {
		// Step 1: Atomically reset status from 'deferred' to 'pending'.
		updated, err := d.db.ResetDeliveryToPending(ctx, delivery.DeliveryID)
		if err != nil {
			d.logger.ErrorContext(ctx, "failed to reset delivery status",
				"delivery_id", delivery.DeliveryID,
				"error", err,
			)
			continue
		}

		if !updated {
			// Already processed by another invocation; skip.
			d.logger.InfoContext(ctx, "delivery already transitioned, skipping",
				"delivery_id", delivery.DeliveryID,
			)
			continue
		}

		// Step 2: Re-enqueue the notification to SQS.
		if err := d.publisher.PublishNotification(ctx, delivery.Payload); err != nil {
			d.logger.ErrorContext(ctx, "failed to re-enqueue deferred notification",
				"delivery_id", delivery.DeliveryID,
				"notification_id", delivery.NotificationID,
				"error", err,
			)
			// The status was already reset to 'pending'. The notification
			// worker will not find it via the deferred query again. However,
			// since we failed to enqueue, it will remain in 'pending' state.
			// The notification worker's normal processing should eventually
			// pick it up, or it will be caught on the next deferred sweep
			// if the status is reset by another mechanism.
			continue
		}

		requeued++
	}

	d.logger.InfoContext(ctx, "deferred notification requeue complete",
		"requeued", requeued,
		"total_found", len(deliveries),
	)

	return requeued, nil
}
