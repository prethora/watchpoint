// Package main is the entrypoint for the Archiver Lambda function.
//
// The Archiver acts as a Maintenance Multiplexer. EventBridge rules send JSON
// payloads indicating the TaskType, and the handler routes execution to the
// appropriate scheduler service. This consolidates low-frequency maintenance
// tasks into a single Lambda to reduce cold starts and infrastructure sprawl.
//
// Handler flow per architecture/09-scheduled-jobs.md Section 6.2:
//  1. Parse MaintenancePayload from EventBridge.
//  2. Acquire a distributed job lock to prevent concurrent execution.
//  3. Switch on TaskType and call the appropriate service method.
//  4. Record job history for operational visibility.
//
// Architecture reference: architecture/09-scheduled-jobs.md Section 6
// SAM template reference: architecture/04-sam-template.md
// Flow reference: SCHED-001, SCHED-002, SCHED-003, WPLC-009, NOTIF-002,
//
//	NOTIF-003, FCST-004, MAINT-001, MAINT-005, MAINT-007, SCHED-006
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/google/uuid"

	"watchpoint/internal/config"
	"watchpoint/internal/scheduler"
)

// Default operational constants for service method calls.
// These match the architecture specification and flow simulations.
const (
	// softDeleteRetention is 30 days per MAINT-005: "hard delete data for orgs
	// deleted > 30 days."
	softDeleteRetention = 30 * 24 * time.Hour

	// archivedWPRetention is the retention period for archived WatchPoints
	// before hard deletion (MAINT-002). Using 90 days as a conservative default.
	archivedWPRetention = 90 * 24 * time.Hour

	// securityEventRetention is 7 days per MAINT-007: "Purge security events
	// older than 7 days."
	securityEventRetention = 7 * 24 * time.Hour

	// notificationRetention is the retention period for old notifications
	// before hard deletion (MAINT-003). Using 90 days as a conservative default.
	notificationRetention = 90 * 24 * time.Hour

	// auditLogRetention is the retention period for audit logs before archival
	// to cold storage (MAINT-004). Using 365 days as a reasonable default.
	auditLogRetention = 365 * 24 * time.Hour

	// auditLogBatchSize is the number of audit log entries processed per
	// archival cycle.
	auditLogBatchSize = 500

	// cleanupBatchLimit is the maximum number of orgs/WPs processed per cleanup
	// run to prevent runaway deletion (Section 7.4).
	cleanupBatchLimit = 50

	// deferredNotifLimit is the maximum deferred deliveries re-queued per run
	// per the NOTIF-002 flow simulation (LIMIT 100).
	deferredNotifLimit = 100

	// reconcileThreshold is the general cutoff for finding stale forecast runs.
	// The reconciler applies model-specific timeouts internally. We use the
	// nowcast timeout (20m) as the floor since it is the most conservative.
	reconcileThreshold = 20 * time.Minute

	// stripeStalenessThreshold is 24 hours per SCHED-003: "last_billing_sync_at
	// < NOW() - INTERVAL '24 hours'."
	stripeStalenessThreshold = 24 * time.Hour

	// stripeSyncLimit is the max orgs processed per sync run (matches flow sim).
	stripeSyncLimit = 50

	// archiveBuffer is the grace period after event window ends before archiving
	// per architecture/09-scheduled-jobs.md Section 7.6: "time_window_end <
	// ($1 - INTERVAL '1 hour')".
	archiveBuffer = 1 * time.Hour

	// lockTTL is the time-to-live for job locks. Set to 15 minutes to cover
	// the typical Lambda execution duration with margin.
	lockTTL = 15 * time.Minute
)

// ServiceRegistry holds all the service implementations that the multiplexer
// can route to. Each field corresponds to one or more TaskTypes. Services are
// eagerly initialized during cold start and reused across invocations.
//
// Fields are interfaces to enable testing. In production, they are backed by
// concrete implementations from the scheduler package.
type ServiceRegistry struct {
	Cleanup              CleanupService
	Archiver             ArchiverService
	TierTransition       TierTransitionService
	DeferredNotification DeferredNotificationService
	Digest               DigestSchedulerService
	Usage                UsageAggregatorService
	Verification         VerificationService
	Reconciler           ForecastReconcilerService
	StripeSyncer         StripeSyncerService
	Enforcer             SubscriptionEnforcerService
}

// Service interfaces define the subset of methods the handler calls.
// These allow clean decoupling from the concrete scheduler types.

// CleanupService provides maintenance cleanup operations.
type CleanupService interface {
	PurgeSoftDeletedOrgs(ctx context.Context, now time.Time, retention time.Duration, limit int) (int, error)
	PurgeExpiredInvites(ctx context.Context, now time.Time) (int, error)
	PurgeArchivedWatchPoints(ctx context.Context, now time.Time, retention time.Duration, limit int) (int, error)
	PurgeNotifications(ctx context.Context, retention time.Duration) (int, error)
	ArchiveAuditLogs(ctx context.Context, retention time.Duration, batchSize int) (int, error)
	PurgeExpiredIdempotencyKeys(ctx context.Context, now time.Time) (int, error)
	PurgeSecurityEvents(ctx context.Context, now time.Time, retention time.Duration) (int, error)
}

// ArchiverService archives expired Event Mode WatchPoints.
type ArchiverService interface {
	ArchiveExpired(ctx context.Context, now time.Time, buffer time.Duration) (int64, error)
}

// TierTransitionService enforces data retention policies on S3.
type TierTransitionService interface {
	EnforceRetention(ctx context.Context, now time.Time) (int, error)
}

// DeferredNotificationService re-queues deferred notifications.
type DeferredNotificationService interface {
	RequeueDeferredNotifications(ctx context.Context, now time.Time, limit int) (int, error)
}

// DigestSchedulerService triggers digest generation.
type DigestSchedulerService interface {
	TriggerDigests(ctx context.Context, currentUTC time.Time) (int, error)
}

// UsageAggregatorService snapshots daily usage.
type UsageAggregatorService interface {
	SnapshotDailyUsage(ctx context.Context, targetDate time.Time) (int, error)
}

// VerificationService triggers forecast verification.
type VerificationService interface {
	TriggerVerification(ctx context.Context, windowStart, windowEnd time.Time) (int, error)
}

// ForecastReconcilerService reconciles stale forecast runs.
type ForecastReconcilerService interface {
	ReconcileStaleRuns(ctx context.Context, now time.Time, threshold time.Duration) (int64, error)
}

// StripeSyncerService reconciles billing state with Stripe.
type StripeSyncerService interface {
	SyncAtRisk(ctx context.Context, now time.Time, stalenessThreshold time.Duration, limit int) (int, error)
}

// SubscriptionEnforcerService enforces billing delinquency and overage.
type SubscriptionEnforcerService interface {
	EnforcePaymentFailure(ctx context.Context, now time.Time) (int, error)
	EnforceOverage(ctx context.Context, now time.Time) (int, error)
}

// JobLocker abstracts the distributed lock acquisition.
type JobLocker interface {
	Acquire(ctx context.Context, lockID string, workerID string, ttl time.Duration) (bool, error)
}

// JobHistorian abstracts the job history recording.
type JobHistorian interface {
	Start(ctx context.Context, jobType string) (int64, error)
	Finish(ctx context.Context, id int64, status string, items int, err error) error
}

// Handler holds the dependencies for the archiver Lambda handler function.
type Handler struct {
	Services   ServiceRegistry
	JobLock    JobLocker
	JobHistory JobHistorian
	WorkerID   string
	Logger     *slog.Logger
}

// Handle processes a MaintenancePayload from EventBridge, routing to the
// appropriate service method based on the TaskType.
//
// Per architecture/09-scheduled-jobs.md Section 6.2:
//  1. Parse payload and determine reference time.
//  2. Acquire distributed lock: "task_type:timestamp_hour".
//  3. Record job start in job_history.
//  4. Switch on TaskType and call service method.
//  5. Record job completion with status and item count.
func (h *Handler) Handle(ctx context.Context, payload scheduler.MaintenancePayload) (string, error) {
	logger := h.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Step 1: Determine reference time.
	now := time.Now().UTC()
	if payload.ReferenceTime != nil {
		now = payload.ReferenceTime.UTC()
	}

	taskStr := string(payload.Task)
	logger.InfoContext(ctx, "archiver handler invoked",
		"task", taskStr,
		"reference_time", now.Format(time.RFC3339),
		"worker_id", h.WorkerID,
	)

	// Validate task type.
	if payload.Task == "" {
		return "", fmt.Errorf("empty task type in maintenance payload")
	}

	// Step 2: Acquire distributed lock.
	lockID := fmt.Sprintf("%s:%s", payload.Task, now.Truncate(time.Hour).Format("2006-01-02T15"))
	acquired, err := h.JobLock.Acquire(ctx, lockID, h.WorkerID, lockTTL)
	if err != nil {
		logger.ErrorContext(ctx, "failed to acquire job lock",
			"lock_id", lockID,
			"error", err,
		)
		return "", fmt.Errorf("acquiring job lock %s: %w", lockID, err)
	}
	if !acquired {
		logger.InfoContext(ctx, "job lock not acquired, another worker is processing",
			"lock_id", lockID,
		)
		return fmt.Sprintf("skipped: lock %s held by another worker", lockID), nil
	}

	logger.InfoContext(ctx, "job lock acquired",
		"lock_id", lockID,
	)

	// Step 3: Record job start in history.
	jobID, err := h.JobHistory.Start(ctx, taskStr)
	if err != nil {
		logger.ErrorContext(ctx, "failed to start job history",
			"task", taskStr,
			"error", err,
		)
		// Non-fatal: proceed with execution even if history tracking fails.
		// We use jobID=0 to signal that Finish should be skipped.
		jobID = 0
	}

	// Step 4: Route to the appropriate service.
	items, execErr := h.dispatch(ctx, payload.Task, now)

	// Step 5: Record job completion.
	status := "success"
	if execErr != nil {
		status = "failed"
	}

	if jobID != 0 {
		if finishErr := h.JobHistory.Finish(ctx, jobID, status, items, execErr); finishErr != nil {
			logger.ErrorContext(ctx, "failed to finish job history",
				"job_id", jobID,
				"task", taskStr,
				"error", finishErr,
			)
		}
	}

	if execErr != nil {
		logger.ErrorContext(ctx, "task execution failed",
			"task", taskStr,
			"error", execErr,
			"items_before_error", items,
		)
		return "", fmt.Errorf("task %s failed: %w", taskStr, execErr)
	}

	result := fmt.Sprintf("task %s complete: %d items processed", taskStr, items)
	logger.InfoContext(ctx, result,
		"task", taskStr,
		"items", items,
	)

	return result, nil
}

// dispatch routes a TaskType to the appropriate service method.
// Returns the number of items processed and any error.
func (h *Handler) dispatch(ctx context.Context, task scheduler.TaskType, now time.Time) (int, error) {
	switch task {
	case scheduler.TaskArchiveWatchPoints:
		count, err := h.Services.Archiver.ArchiveExpired(ctx, now, archiveBuffer)
		return int(count), err

	case scheduler.TaskRequeueDeferredNotifs:
		return h.Services.DeferredNotification.RequeueDeferredNotifications(ctx, now, deferredNotifLimit)

	case scheduler.TaskCleanupSoftDeletes:
		// Per MAINT-005: also purge expired invites, archived WPs, old
		// notifications, and audit logs in the same maintenance window.
		total := 0

		orgs, err := h.Services.Cleanup.PurgeSoftDeletedOrgs(ctx, now, softDeleteRetention, cleanupBatchLimit)
		if err != nil {
			return total, fmt.Errorf("purging soft-deleted orgs: %w", err)
		}
		total += orgs

		invites, err := h.Services.Cleanup.PurgeExpiredInvites(ctx, now)
		if err != nil {
			return total, fmt.Errorf("purging expired invites: %w", err)
		}
		total += invites

		wps, err := h.Services.Cleanup.PurgeArchivedWatchPoints(ctx, now, archivedWPRetention, cleanupBatchLimit)
		if err != nil {
			return total, fmt.Errorf("purging archived watchpoints: %w", err)
		}
		total += wps

		notifs, err := h.Services.Cleanup.PurgeNotifications(ctx, notificationRetention)
		if err != nil {
			return total, fmt.Errorf("purging old notifications: %w", err)
		}
		total += notifs

		audits, err := h.Services.Cleanup.ArchiveAuditLogs(ctx, auditLogRetention, auditLogBatchSize)
		if err != nil {
			return total, fmt.Errorf("archiving audit logs: %w", err)
		}
		total += audits

		return total, nil

	case scheduler.TaskCleanupIdempotencyKeys:
		return h.Services.Cleanup.PurgeExpiredIdempotencyKeys(ctx, now)

	case scheduler.TaskCleanupSecurityEvents:
		return h.Services.Cleanup.PurgeSecurityEvents(ctx, now, securityEventRetention)

	case scheduler.TaskTriggerDigests:
		return h.Services.Digest.TriggerDigests(ctx, now)

	case scheduler.TaskAggregateUsage:
		return h.Services.Usage.SnapshotDailyUsage(ctx, now)

	case scheduler.TaskSyncStripe:
		// Per SCHED-003: sync billing state, then enforce payment failures
		// and overage violations in the same daily run.
		total := 0

		synced, err := h.Services.StripeSyncer.SyncAtRisk(ctx, now, stripeStalenessThreshold, stripeSyncLimit)
		if err != nil {
			return total, fmt.Errorf("syncing stripe: %w", err)
		}
		total += synced

		paymentEnforced, err := h.Services.Enforcer.EnforcePaymentFailure(ctx, now)
		if err != nil {
			return total, fmt.Errorf("enforcing payment failure: %w", err)
		}
		total += paymentEnforced

		overageEnforced, err := h.Services.Enforcer.EnforceOverage(ctx, now)
		if err != nil {
			return total, fmt.Errorf("enforcing overage: %w", err)
		}
		total += overageEnforced

		return total, nil

	case scheduler.TaskVerification:
		windowStart := now.Add(-scheduler.VerificationWindowStart)
		windowEnd := now.Add(-scheduler.VerificationWindowEnd)
		return h.Services.Verification.TriggerVerification(ctx, windowStart, windowEnd)

	case scheduler.TaskReconcileForecasts:
		count, err := h.Services.Reconciler.ReconcileStaleRuns(ctx, now, reconcileThreshold)
		return int(count), err

	case scheduler.TaskForecastTier:
		return h.Services.TierTransition.EnforceRetention(ctx, now)

	default:
		return 0, fmt.Errorf("unknown task type: %q", task)
	}
}

func main() {
	// Initialize structured logger at startup.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	logger.Info("Archiver Lambda initializing (cold start)")

	// Resolve SSM secrets into environment variables before reading config.
	// In non-local environments, secrets like DATABASE_URL are stored in SSM
	// Parameter Store and referenced via _SSM_PARAM suffix variables.
	if err := config.ResolveSecrets(config.NewSSMProvider(os.Getenv("AWS_REGION"))); err != nil {
		logger.Error("Failed to resolve SSM secrets", "error", err)
		os.Exit(1)
	}

	// Generate a unique worker ID for this Lambda instance.
	// Used for distributed lock ownership tracking.
	workerID := uuid.New().String()

	// In production, this is where we would:
	// 1. Load AWS SDK configuration
	// 2. Initialize database connection pool from DATABASE_URL
	// 3. Initialize S3 client for tier transition and audit archival
	// 4. Initialize SQS client for deferred notification re-queuing
	// 5. Initialize Stripe client for billing sync
	// 6. Initialize RunPod client for verification/calibration
	// 7. Create repository instances (JobLockRepository, JobHistoryRepository, etc.)
	// 8. Create service instances and wire them into the ServiceRegistry
	//
	// These dependencies will be wired when the integration tasks are implemented.
	// For now, the handler structure and routing logic are complete and tested.

	handler := &Handler{
		Services:   ServiceRegistry{},
		JobLock:    nil, // Wired during integration
		JobHistory: nil, // Wired during integration
		WorkerID:   workerID,
		Logger:     logger,
	}

	logger.Info("Archiver Lambda initialized",
		"worker_id", workerID,
	)

	lambda.Start(handler.Handle)
}
