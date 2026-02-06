// Package main implements the job-runner CLI tool for invoking archiver
// maintenance tasks directly, bypassing the AWS Lambda shim.
//
// This tool is intended for local development, manual backfilling, and
// operational debugging. It constructs a scheduler.MaintenancePayload and
// invokes the archiver handler's core dispatch logic directly.
//
// Usage:
//
//	go run ./cmd/tools/job-runner --task=aggregate_usage
//	go run ./cmd/tools/job-runner --task=cleanup_soft_deletes --reference-time=2026-01-15T02:00:00Z
//	go run ./cmd/tools/job-runner --dry-run --task=trigger_digests
//	go run ./cmd/tools/job-runner --list
//
// The tool reads DATABASE_URL from environment variables (or .env file via
// godotenv). In --dry-run mode, it prints the constructed JSON payload
// without executing. When DATABASE_URL is available, it acquires the
// distributed job lock, records job history, and dispatches to the
// appropriate scheduler service.
//
// Architecture reference: architecture/09-scheduled-jobs.md Section 6
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"watchpoint/internal/db"
	"watchpoint/internal/scheduler"
)

// validTasks is the exhaustive set of TaskType values the archiver supports.
// This is maintained in sync with the constants in internal/scheduler/types.go
// and the dispatch table in cmd/archiver/main.go.
var validTasks = map[scheduler.TaskType]string{
	scheduler.TaskArchiveWatchPoints:     "Archive expired Event Mode WatchPoints",
	scheduler.TaskRequeueDeferredNotifs:  "Re-queue notifications deferred by Quiet Hours",
	scheduler.TaskCleanupSoftDeletes:     "Hard delete orgs soft-deleted > 30 days + related cleanup",
	scheduler.TaskCleanupIdempotencyKeys: "Purge expired idempotency keys",
	scheduler.TaskCleanupSecurityEvents:  "Purge security events older than 7 days",
	scheduler.TaskTriggerDigests:         "Generate digests for users in current local hour",
	scheduler.TaskAggregateUsage:         "Snapshot daily API/WatchPoint usage",
	scheduler.TaskSyncStripe:             "Reconcile Stripe subscriptions for at-risk accounts",
	scheduler.TaskVerification:           "Trigger RunPod forecast verification",
	scheduler.TaskReconcileForecasts:     "Mark stuck running forecast jobs as failed",
	scheduler.TaskForecastTier:           "Move old forecasts to cold storage or delete",
}

// tasksRequiringExternalServices lists tasks that cannot be executed locally
// because they depend on external services (SQS, Stripe, RunPod, S3) that
// are not available in the CLI context.
var tasksRequiringExternalServices = map[scheduler.TaskType]string{
	scheduler.TaskArchiveWatchPoints:    "SQS publisher (summary generation)",
	scheduler.TaskRequeueDeferredNotifs: "SQS publisher (notification re-queuing)",
	scheduler.TaskTriggerDigests:        "SQS publisher (digest messages)",
	scheduler.TaskSyncStripe:            "Stripe API client",
	scheduler.TaskVerification:          "RunPod client + observation probe",
	scheduler.TaskReconcileForecasts:    "RunPod client (cancel/retry)",
	scheduler.TaskForecastTier:          "AWS S3 client (object deletion)",
	scheduler.TaskAggregateUsage:        "UsageAggregator repository (not yet wired)",
}

// Operational constants matching cmd/archiver/main.go.
// Duplicated here because cmd/archiver is a main package and cannot be imported.
const (
	softDeleteRetention    = 30 * 24 * time.Hour
	archivedWPRetention    = 90 * 24 * time.Hour
	securityEventRetention = 7 * 24 * time.Hour
	notificationRetention  = 90 * 24 * time.Hour
	auditLogRetention      = 365 * 24 * time.Hour
	auditLogBatchSize      = 500
	cleanupBatchLimit      = 50
	lockTTL                = 15 * time.Minute
)

func main() {
	// Parse command-line flags.
	taskFlag := flag.String("task", "", "Task type to execute (e.g., aggregate_usage)")
	refTimeFlag := flag.String("reference-time", "", "Override reference time (RFC3339, e.g., 2026-01-15T02:00:00Z)")
	listFlag := flag.Bool("list", false, "List all available task types and exit")
	dryRunFlag := flag.Bool("dry-run", false, "Print the JSON payload without executing")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: job-runner [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Invoke archiver maintenance tasks directly, bypassing Lambda.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nUse --list to see all available task types.\n")
	}

	flag.Parse()

	// Handle --list: print available tasks and exit.
	if *listFlag {
		printAvailableTasks()
		return
	}

	// Validate --task is provided.
	if *taskFlag == "" {
		fmt.Fprintf(os.Stderr, "error: --task is required\n\n")
		flag.Usage()
		os.Exit(1)
	}

	// Validate the task type.
	taskType := scheduler.TaskType(*taskFlag)
	if _, ok := validTasks[taskType]; !ok {
		fmt.Fprintf(os.Stderr, "error: unknown task type %q\n\n", *taskFlag)
		printAvailableTasks()
		os.Exit(1)
	}

	// Parse optional reference time.
	var refTime *time.Time
	if *refTimeFlag != "" {
		t, err := time.Parse(time.RFC3339, *refTimeFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid --reference-time %q: %v\n", *refTimeFlag, err)
			fmt.Fprintf(os.Stderr, "  expected RFC3339 format, e.g., 2026-01-15T02:00:00Z\n")
			os.Exit(1)
		}
		refTime = &t
	}

	// Construct the maintenance payload.
	payload := scheduler.MaintenancePayload{
		Task:          taskType,
		ReferenceTime: refTime,
	}

	// Initialize structured logger.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// If dry-run, print the JSON payload and exit.
	if *dryRunFlag {
		printPayload(payload)
		return
	}

	// Check if the task requires external services that are unavailable locally.
	if reason, ok := tasksRequiringExternalServices[taskType]; ok {
		fmt.Fprintf(os.Stderr, "error: task %q requires %s which is not available in CLI context\n", taskType, reason)
		fmt.Fprintf(os.Stderr, "  use --dry-run to generate the JSON payload for manual invocation\n")
		os.Exit(1)
	}

	// Load .env file for local development (non-fatal if missing).
	if err := godotenv.Load(); err != nil {
		logger.Info("no .env file loaded (this is fine in production)", "error", err)
	}

	// Set up cancellation context with signal handling.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Execute the task.
	result, err := executeTask(ctx, payload, logger)
	if err != nil {
		logger.Error("task execution failed",
			"task", string(payload.Task),
			"error", err,
		)
		os.Exit(1)
	}

	logger.Info("task execution succeeded",
		"task", string(payload.Task),
		"result", result,
	)
}

// executeTask wires up the database and service dependencies, then invokes
// the archiver handler logic directly (bypassing Lambda).
//
// This function mirrors the cold-start wiring in cmd/archiver/main.go and
// the Handle method flow:
//  1. Connect to the database.
//  2. Determine reference time.
//  3. Acquire distributed job lock.
//  4. Record job history start.
//  5. Dispatch to the appropriate service.
//  6. Record job history completion.
func executeTask(ctx context.Context, payload scheduler.MaintenancePayload, logger *slog.Logger) (string, error) {
	// Read DATABASE_URL from environment.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return "", fmt.Errorf("DATABASE_URL environment variable is required")
	}

	// Initialize database connection pool.
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return "", fmt.Errorf("creating database pool: %w", err)
	}
	defer pool.Close()

	// Verify database connectivity.
	if err := pool.Ping(ctx); err != nil {
		return "", fmt.Errorf("pinging database: %w", err)
	}

	logger.Info("database connection established")

	// Initialize job infrastructure repositories.
	jobLockRepo := db.NewJobLockRepository(pool)
	jobHistoryRepo := db.NewJobHistoryRepository(pool)

	// Generate a unique worker ID for lock ownership.
	workerID := fmt.Sprintf("job-runner-%s", uuid.New().String())

	// Determine reference time.
	now := time.Now().UTC()
	if payload.ReferenceTime != nil {
		now = payload.ReferenceTime.UTC()
	}

	taskStr := string(payload.Task)
	logger.Info("executing task",
		"task", taskStr,
		"reference_time", now.Format(time.RFC3339),
		"worker_id", workerID,
	)

	// Acquire distributed lock (same pattern as the archiver handler).
	lockID := fmt.Sprintf("%s:%s", payload.Task, now.Truncate(time.Hour).Format("2006-01-02T15"))
	acquired, err := jobLockRepo.Acquire(ctx, lockID, workerID, lockTTL)
	if err != nil {
		return "", fmt.Errorf("acquiring job lock %s: %w", lockID, err)
	}
	if !acquired {
		return fmt.Sprintf("skipped: lock %s held by another worker", lockID), nil
	}
	logger.Info("job lock acquired", "lock_id", lockID)

	// Record job start.
	jobID, err := jobHistoryRepo.Start(ctx, taskStr)
	if err != nil {
		logger.Warn("failed to record job start (continuing anyway)", "error", err)
		jobID = 0
	}

	// Dispatch to the appropriate service.
	items, execErr := dispatch(ctx, payload.Task, now, pool, logger)

	// Record job completion.
	status := "success"
	if execErr != nil {
		status = "failed"
	}
	if jobID != 0 {
		if finishErr := jobHistoryRepo.Finish(ctx, jobID, status, items, execErr); finishErr != nil {
			logger.Error("failed to record job completion", "job_id", jobID, "error", finishErr)
		}
	}

	if execErr != nil {
		return "", fmt.Errorf("task %s failed: %w", taskStr, execErr)
	}

	return fmt.Sprintf("task %s complete: %d items processed", taskStr, items), nil
}

// dispatch routes a TaskType to the appropriate scheduler service method.
//
// This mirrors the routing logic in cmd/archiver/main.go Handler.dispatch.
// Services that only require database access are wired here using the pool.
// Tasks requiring external services (SQS, Stripe, RunPod, S3) are blocked
// at the CLI argument validation stage before reaching this function.
//
// The scheduler service constructors accept interface types for their DB
// dependencies. The *pgxpool.Pool satisfies the DBTX interface used by
// concrete db.* repositories, but the scheduler services expect their own
// specialized DB interfaces (CleanupDB, UsageAggregatorDB, etc.). The
// concrete repositories implementing these interfaces are wired during
// the integration phase. Until then, the DB-only tasks use inline SQL
// via the pool directly for the operations they need.
func dispatch(ctx context.Context, task scheduler.TaskType, now time.Time, pool *pgxpool.Pool, logger *slog.Logger) (int, error) {
	switch task {
	case scheduler.TaskCleanupSoftDeletes:
		return dispatchCleanupSoftDeletes(ctx, now, pool, logger)

	case scheduler.TaskCleanupIdempotencyKeys:
		return dispatchCleanupIdempotencyKeys(ctx, now, pool, logger)

	case scheduler.TaskCleanupSecurityEvents:
		return dispatchCleanupSecurityEvents(ctx, now, pool, logger)

	default:
		// All tasks requiring external services are caught in main() before
		// reaching here. This is a defensive fallback.
		return 0, fmt.Errorf("task %q cannot be dispatched in CLI context", task)
	}
}

// dispatchCleanupSoftDeletes executes the cleanup_soft_deletes task using
// direct SQL queries against the pool. This replicates the CleanupService
// logic from internal/scheduler/maintenance.go.
func dispatchCleanupSoftDeletes(ctx context.Context, now time.Time, pool *pgxpool.Pool, logger *slog.Logger) (int, error) {
	total := 0

	// 1. Purge soft-deleted orgs (MAINT-005).
	cutoff := now.Add(-softDeleteRetention)
	rows, err := pool.Query(ctx,
		`SELECT id FROM organizations
		 WHERE deleted_at IS NOT NULL AND deleted_at < $1
		 LIMIT $2`,
		cutoff, cleanupBatchLimit)
	if err != nil {
		return 0, fmt.Errorf("listing soft-deleted orgs: %w", err)
	}
	var orgIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scanning org id: %w", err)
		}
		orgIDs = append(orgIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating soft-deleted orgs: %w", err)
	}

	for _, orgID := range orgIDs {
		tag, err := pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, orgID)
		if err != nil {
			logger.Error("failed to hard delete org", "org_id", orgID, "error", err)
			continue
		}
		total += int(tag.RowsAffected())
	}
	logger.Info("purged soft-deleted orgs", "count", total)

	// 2. Purge expired invites (MAINT-006).
	tag, err := pool.Exec(ctx,
		`DELETE FROM users WHERE status = 'invited' AND invite_expires_at < $1`,
		now)
	if err != nil {
		return total, fmt.Errorf("purging expired invites: %w", err)
	}
	invites := int(tag.RowsAffected())
	total += invites
	if invites > 0 {
		logger.Info("purged expired invites", "count", invites)
	}

	// 3. Purge archived watchpoints (MAINT-002).
	wpCutoff := now.Add(-archivedWPRetention)
	wpRows, err := pool.Query(ctx,
		`SELECT id FROM watchpoints
		 WHERE status = 'archived' AND archived_at < $1
		 LIMIT $2`,
		wpCutoff, cleanupBatchLimit)
	if err != nil {
		return total, fmt.Errorf("listing archived watchpoints: %w", err)
	}
	var wpIDs []string
	for wpRows.Next() {
		var id string
		if err := wpRows.Scan(&id); err != nil {
			wpRows.Close()
			return total, fmt.Errorf("scanning watchpoint id: %w", err)
		}
		wpIDs = append(wpIDs, id)
	}
	wpRows.Close()
	if err := wpRows.Err(); err != nil {
		return total, fmt.Errorf("iterating archived watchpoints: %w", err)
	}

	if len(wpIDs) > 0 {
		wpTag, err := pool.Exec(ctx,
			`DELETE FROM watchpoints WHERE id = ANY($1)`,
			wpIDs)
		if err != nil {
			return total, fmt.Errorf("hard deleting watchpoints: %w", err)
		}
		deleted := int(wpTag.RowsAffected())
		total += deleted
		logger.Info("purged archived watchpoints", "count", deleted)
	}

	// 4. Purge old notifications (MAINT-003).
	notifCutoff := now.Add(-notificationRetention)
	notifTag, err := pool.Exec(ctx,
		`DELETE FROM notifications WHERE created_at < $1`,
		notifCutoff)
	if err != nil {
		return total, fmt.Errorf("purging old notifications: %w", err)
	}
	notifs := int(notifTag.RowsAffected())
	total += notifs
	if notifs > 0 {
		logger.Info("purged old notifications", "count", notifs)
	}

	// 5. Audit log archival is skipped in CLI context (requires S3 archiver).
	logger.Info("audit log archival skipped (no S3 archiver in CLI context)")

	return total, nil
}

// dispatchCleanupIdempotencyKeys executes the cleanup_idempotency_keys task.
func dispatchCleanupIdempotencyKeys(ctx context.Context, now time.Time, pool *pgxpool.Pool, logger *slog.Logger) (int, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE expires_at < $1`,
		now)
	if err != nil {
		return 0, fmt.Errorf("deleting expired idempotency keys: %w", err)
	}
	count := int(tag.RowsAffected())
	if count > 0 {
		logger.Info("purged expired idempotency keys", "count", count)
	}
	return count, nil
}

// dispatchCleanupSecurityEvents executes the cleanup_security_events task.
func dispatchCleanupSecurityEvents(ctx context.Context, now time.Time, pool *pgxpool.Pool, logger *slog.Logger) (int, error) {
	cutoff := now.Add(-securityEventRetention)
	tag, err := pool.Exec(ctx,
		`DELETE FROM security_events WHERE attempted_at < $1`,
		cutoff)
	if err != nil {
		return 0, fmt.Errorf("deleting old security events: %w", err)
	}
	count := int(tag.RowsAffected())
	if count > 0 {
		logger.Info("purged old security events", "count", count, "cutoff", cutoff.Format(time.RFC3339))
	}
	return count, nil
}

// printAvailableTasks prints all valid task types and their descriptions to
// stderr, sorted alphabetically by task name.
func printAvailableTasks() {
	fmt.Fprintf(os.Stderr, "Available task types:\n\n")

	// Sort task types for stable output.
	tasks := make([]scheduler.TaskType, 0, len(validTasks))
	for t := range validTasks {
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return string(tasks[i]) < string(tasks[j])
	})

	// Find the longest task name for alignment.
	maxLen := 0
	for _, t := range tasks {
		if len(string(t)) > maxLen {
			maxLen = len(string(t))
		}
	}

	for _, t := range tasks {
		fmt.Fprintf(os.Stderr, "  %-*s  %s\n", maxLen, string(t), validTasks[t])
	}
	fmt.Fprintln(os.Stderr)
}

// printPayload marshals the MaintenancePayload to pretty-printed JSON and
// writes it to stdout for inspection or piping.
func printPayload(payload scheduler.MaintenancePayload) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to marshal payload: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(string(data))

	// Also log the description for context on stderr.
	if desc, ok := validTasks[payload.Task]; ok {
		fmt.Fprintf(os.Stderr, "\nTask: %s\nDescription: %s\n", payload.Task, desc)
		if payload.ReferenceTime != nil {
			fmt.Fprintf(os.Stderr, "Reference time: %s\n", payload.ReferenceTime.Format(time.RFC3339))
		} else {
			fmt.Fprintf(os.Stderr, "Reference time: (current UTC time will be used)\n")
		}
	}
}
