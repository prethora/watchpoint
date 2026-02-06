# 09 - Scheduled Jobs & Background Maintenance

> **Purpose**: Defines the architecture for all time-based and background maintenance processes. This includes data polling, billing aggregation, digest generation, and system cleanup. It establishes the "Multiplexer" pattern to consolidate low-frequency maintenance tasks into a single Lambda to reduce cold starts and infrastructure sprawl.
> **Package**: `package scheduler`
> **Dependencies**: `01-foundation-types.md`, `02-foundation-db.md`, `03-config.md`, `04-sam-template.md`

---

## Table of Contents

1. [Overview & Infrastructure Strategy](#1-overview--infrastructure-strategy)
2. [Database Extensions (Migrations)](#2-database-extensions-migrations)
3. [Domain Types](#3-domain-types)
4. [Concurrency & Idempotency](#4-concurrency--idempotency)
5. [Data Poller Service](#5-data-poller-service)
6. [Maintenance Multiplexer](#6-maintenance-multiplexer)
7. [Core Service Logic](#7-core-service-logic)
8. [Flow Coverage](#8-flow-coverage)

---

## 1. Overview & Infrastructure Strategy

The platform utilizes AWS EventBridge to trigger background tasks. To optimize for cost and reduce cold starts, tasks are divided into two Lambda functions based on frequency and resource profile.

### 1.1 Function Split

1.  **DataPollerFunction (`cmd/data-poller`)**:
    *   **Frequency**: High (Every 15 minutes).
    *   **Profile**: Network-bound (S3 ListObjects), stateless.
    *   **Responsibility**: Detecting new forecast data and triggering the generation pipeline.

2.  **ArchiverFunction (`cmd/archiver`)**:
    *   **Frequency**: Varied (Hourly to Daily).
    *   **Profile**: Database-bound, batch processing.
    *   **Architecture**: Acts as a **Multiplexer**. EventBridge rules send a JSON payload indicating the `TaskType`, and the handler routes execution to the appropriate service logic.

### 1.2 Job Inventory

| Task Type | Function | Schedule | Flow ID | Description |
| :--- | :--- | :--- | :--- | :--- |
| `poll_data` | DataPoller | Rate(15 min) | `FCST-003` | Check NOAA S3 buckets for new runs. |
| `archive_watchpoints` | Archiver | Rate(15 min) | `WPLC-009` | Mark expired Event Mode WPs as archived. |
| `requeue_deferred` | Archiver | Rate(15 min) | `NOTIF-002` | Re-queue notifications deferred by Quiet Hours. |
| `reconcile_forecasts` | Archiver | Rate(1 hour) | `FCST-004` | Mark stuck `running` jobs as failed. |
| `trigger_digests` | Archiver | Rate(1 hour) | `NOTIF-003` | Generate digests for users in current local hour. |
| `cleanup_idempotency_keys` | Archiver | Cron(0 3 * * ? *) | `SCHED-006` | Purge expired idempotency records. |
| `cleanup_soft_deletes` | Archiver | Cron(0 2 * * ? *) | `MAINT-005` | Hard delete data for orgs deleted > 30 days. |
| `aggregate_usage` | Archiver | Cron(0 1 * * ? *) | `SCHED-002` | Snapshot daily API/WatchPoint usage. |
| `sync_stripe` | Archiver | Cron(0 0 * * ? *) | `SCHED-003` | Reconcile subscriptions for at-risk accounts. |
| `verification` | Archiver | Cron(0 3 * * ? *) | `OBS-005` | Trigger RunPod to verify forecast accuracy. |
| `transition_forecasts`| Archiver | Cron(0 4 * * ? *) | `MAINT-001` | Move old forecasts to cold storage or delete. |
| `cleanup_security_events` | Archiver | Cron(0 5 * * ? *) | `MAINT-007` | Purge security events older than 7 days. |

---

## 2. Database Extensions (Migrations)

*All scheduler-related schema is consolidated in `02-foundation-db.md` (Section 6.5).*

The following tables and indexes support scheduler operations:
- `usage_history` - Daily usage snapshots for billing (SCHED-002)
- `verification_results` - Forecast accuracy metrics (OBS-005)
- `job_locks` - Idempotent job execution locks
- `job_history` - Operational visibility and audit trail
- `idx_watchpoints_archive_candidates` - Efficient archiver queries
- Organization columns: `last_digest_generated_at`, `last_billing_sync_at`

---

## 3. Domain Types

*Shared types are consolidated in `01-foundation-types.md` (Section 10.5).*

This module uses the following types from `package types`:
- `types.JobRun` - Job execution history and status tracking
- `types.VerificationResult` - Forecast accuracy metrics

### Local Types

```go
// JobLock is local to the scheduler package (not shared externally)
type JobLock struct {
    ID        string    `db:"id"`
    WorkerID  string    `db:"worker_id"`
    LockedAt  time.Time `db:"locked_at"`
    ExpiresAt time.Time `db:"expires_at"`
}
```

---

## 4. Concurrency & Idempotency

To prevent race conditions from EventBridge retries or overlapping executions, strictly defined locking and history tracking are used.

### 4.1 Job Lock Repository

```go
type JobLockRepository interface {
    // Acquire attempts to insert the lock. Returns true if acquired, false if exists.
    // lockID is usually "task_type:timestamp_hour".
    Acquire(ctx context.Context, lockID string, workerID string, ttl time.Duration) (bool, error)
}
```

### 4.2 Job History Repository

Used for operational visibility.

```go
type JobHistoryRepository interface {
    Start(ctx context.Context, jobType string) (int64, error)
    Finish(ctx context.Context, id int64, status string, items int, err error) error
}
```

---

## 5. Data Poller Service

**Flow**: `FCST-003`

Detects new data in upstream buckets (NOAA) and triggers the internal forecast pipeline. It does NOT download the data itself; it passes references to RunPod.

### Input Payload

```go
// DataPollerInput defines the input for manual Lambda invocation.
// Used for disaster recovery and forcing retries.
type DataPollerInput struct {
    ForceRetry    bool `json:"force_retry"`
    BackfillHours int  `json:"backfill_hours"`
    Limit         int  `json:"limit"` // Limits RunPod triggers per execution to prevent timeout
}
```

### 5.1 Interfaces

```go
// UpstreamSource defines a remote data source (NOAA GFS/GOES).
type UpstreamSource interface {
    Name() types.ForecastType
    // CheckAvailability returns timestamps of runs available in S3 since the given time.
    CheckAvailability(ctx context.Context, since time.Time) ([]time.Time, error)
}

type RunPodClient interface {
    // TriggerInference calls the RunPod API.
    // Returns the external Job ID (e.g., RunPod ID) on success.
    TriggerInference(ctx context.Context, payload types.InferencePayload) (string, error)
    // CancelJob terminates a running job on the external provider.
    // Used by reconciliation to stop hanging jobs and prevent cost accumulation.
    CancelJob(ctx context.Context, externalID string) error
}
```

### 5.2 Logic
1.  Query `forecast_runs` for the latest `run_timestamp` for the given model.
2.  Call `UpstreamSource.CheckAvailability`. **Mirror Failover**: The implementation MUST iterate through the configured `UpstreamMirrors` (from `ForecastConfig`) sequentially. For each mirror, attempt to check availability. If a mirror fails (network error, access denied, timeout), log a warning and proceed to the next mirror. Only return an error if ALL configured mirrors fail.
3.  **Nowcast Synchronization**: For Nowcast model specifically, manage two distinct `UpstreamSource` instances (GOES for satellite imagery, MRMS for radar). Fetch available timestamps from both sources, calculate the intersection of timestamps (within a 5-minute tolerance window), and trigger inference ONLY for data points where both sources have aligned temporal coverage.
4.  **Rate Limiting**: If a `Limit` is provided in the `DataPollerInput`, the poller MUST stop triggering new jobs once the limit is reached to prevent Lambda timeouts during large backfills.
5.  If new runs found (and limit not reached):
    *   Insert into `forecast_runs` with `status='running'`.
    *   Construct `types.InferencePayload`.
    *   **For Nowcast**: Query `calibration_coefficients` (map of tiles) and inject into `InputConfig.Calibration`.
    *   Call `RunPodClient.TriggerInference`, receiving the `external_id`.
    *   Update `forecast_runs` with the `external_id`.

---

## 6. Maintenance Multiplexer

**Entrypoint**: `ArchiverFunction`

This Lambda receives a JSON payload from EventBridge and routes it to the correct service.

### 6.1 Payload Definition

```go
type TaskType string

const (
    TaskArchiveWatchPoints      TaskType = "archive_watchpoints"
    TaskRequeueDeferredNotifs   TaskType = "requeue_deferred"
    TaskCleanupSoftDeletes      TaskType = "cleanup_soft_deletes"
    TaskCleanupIdempotencyKeys  TaskType = "cleanup_idempotency_keys"
    TaskCleanupSecurityEvents   TaskType = "cleanup_security_events"
    TaskTriggerDigests          TaskType = "trigger_digests"
    TaskSyncStripe              TaskType = "sync_stripe"
    TaskAggregateUsage          TaskType = "aggregate_usage"
    TaskVerification            TaskType = "verification"
    TaskReconcileForecasts      TaskType = "reconcile_forecasts"
    TaskForecastTier            TaskType = "transition_forecasts"
)

type MaintenancePayload struct {
    Task TaskType `json:"task"`
    // Optional override for manual invocation/backfilling
    ReferenceTime *time.Time `json:"reference_time,omitempty"`
}
```

### 6.2 Handler Logic
1.  **Parse Payload**: Extract task and time. If `ReferenceTime` is provided, use it as `now`; otherwise use `time.Now().UTC()`.
2.  **Acquire Lock**: Generate Lock ID `fmt.Sprintf("%s:%s", task, time.Truncate(time.Hour))`.
3.  **Execute**: Switch on `TaskType` and call specific service interface, passing `now` to ensure deterministic execution.
4.  **Record History**: Update `job_history`.

---

## 7. Core Service Logic

All service interfaces accept a `now` parameter to support manual backfilling/testing via the `MaintenancePayload.ReferenceTime`.

### 7.1 Digest Scheduler (Hourly Tick)

**Flow**: `NOTIF-003`

Responsible for triggering daily digests at the user's preferred local time.

*   **Logic**:
    1.  Query `organizations` using index: `SELECT id, notification_preferences FROM organizations WHERE next_digest_at <= $now AND deleted_at IS NULL`.
    2.  For each org:
        a.  Enqueue "Generate Digest" message to the Notification Queue.
        b.  Calculate *next* occurrence based on `preferences.digest.schedule` and `timezone`.
        c.  UPDATE `organizations` SET `next_digest_at` = new_time.

```go
type DigestScheduler interface {
    TriggerDigests(ctx context.Context, currentUTC time.Time) (int, error)
}
```

### 7.2 Billing Aggregator (Lazy Reset)

**Flow**: `SCHED-001`, `SCHED-002`

Usage tracking relies on `rate_limits` (current day) and `usage_history` (past days).

*   **Middleware Strategy (Hot Path)**:
    *   On API request, if `rate_limits.period_end < NOW`, perform atomic **Snapshot & Reset**: Move current counts to `usage_history` and reset `rate_limits` to 0.
*   **Scheduled Job (Catch-all)**:
    *   Runs daily at 01:00 UTC.
    *   Finds any `rate_limits` records that haven't been touched (and thus reset) by the middleware.
    *   Performs the Snapshot & Reset logic using `targetDate`.

**Composite Key Processing (VERT-001)**: The aggregator MUST process `rate_limits` rows based on the composite primary key `(organization_id, source)`:

1.  **Snapshot**: For each distinct `(organization_id, source)` row in `rate_limits`, insert a corresponding row into `usage_history` preserving the `source` value:
    ```sql
    INSERT INTO usage_history (organization_id, date, source, api_calls, watchpoints, notifications)
    SELECT organization_id, $targetDate, source, api_calls_count, watchpoints_count, 0
    FROM rate_limits WHERE period_end < $now
    ON CONFLICT (organization_id, date, source) DO UPDATE SET ...
    ```

2.  **Reset**: Reset counters for specific source rows:
    ```sql
    UPDATE rate_limits
    SET api_calls_count = 0, watchpoints_count = 0, period_start = $now, period_end = $nextMidnight
    WHERE organization_id = $1 AND source = $2
    ```

**Compliance Check Logic (Overage Resolution)**: In addition to snapshots, the aggregator compares the previous day's **aggregated** usage (SUM across all sources) against the organization's plan limits. If usage is now within limits, it MUST set `organizations.overage_started_at = NULL`. This effectively clears the 14-day grace period timer, validating that the user has remediated their overage (e.g., by deleting WatchPoints or upgrading).

```go
type UsageAggregator interface {
    SnapshotDailyUsage(ctx context.Context, targetDate time.Time) (int, error)
}
```

### 7.3 Verification Orchestrator

**Flow**: `OBS-005`

Verifies forecast accuracy against ground truth without bloating the Go Lambda.

*   **Logic**:
    1.  Identify forecast runs from 24-48 hours relative to `now`.
    2.  **Probe**: Check if NOAA observational data (ISD/MRMS) exists for that period via `HeadObject`.
    3.  If data exists, construct an `InferencePayload` with `TaskType=verification` and call the RunPod Client.
    4.  The RunPod worker downloads heavy data, computes metrics, and writes to `verification_results`.

*   **Canary Validation**: After verification completes, query WatchPoints where `metadata->>'system_role' = 'canary'`.
    For each Canary, compare expected vs actual trigger state and emit appropriate alerts.

```go
type VerificationService interface {
    // TriggerVerification constructs an InferencePayload with TaskType=verification
    // and dispatches it to the RunPod Client for processing.
    TriggerVerification(ctx context.Context, windowStart, windowEnd time.Time) (int, error)
}
```

### 7.4 Maintenance & Cleanup

**Flow**: `MAINT-005`, `MAINT-002`, `MAINT-006`, `FCST-004`

*   **CleanupService**: Performs hard deletes on organizations soft-deleted > 30 days ago, expired invites, and archived data. Enforces a **Batch Limit** (e.g., 50 per run) to prevent runaway deletion bugs.
*   **ForecastReconciler**: Manages the recovery state machine for forecast generation (`FCST-004`).
    1.  Checks for `forecast_runs` in `running` state exceeding the configured model-specific timeout: `TimeoutNowcast` (default 20m) for nowcast models, `TimeoutMediumRange` (default 3h) for medium-range models. These timeouts are loaded from `ForecastConfig`.
    2.  Verifies status with external provider (RunPod) using `external_id`.
    3.  If job is hanging (running > timeout): Call `RunPodClient.CancelJob(external_id)` to terminate the job and stop cost accumulation, then mark status as `failed` with reason 'Timeout'.
    4.  If failed/dead (non-timeout): Increments `retry_count`.
    5.  If `retry_count < 3`: Re-submits job and updates `external_id`.
    6.  If `retry_count >= 3`: Marks status as `failed` and logs `failure_reason`.

```go
type CleanupService interface {
    PurgeSoftDeletedOrgs(ctx context.Context, now time.Time, retention time.Duration, limit int) (int, error)
    PurgeExpiredInvites(ctx context.Context, now time.Time) (int, error)

    // PurgeArchivedWatchPoints hard-deletes archived WatchPoints older than retention.
    // Note: With ON DELETE CASCADE on watchpoints.organization_id, associated evaluation
    // state and notifications are automatically cleaned up by the database.
    PurgeArchivedWatchPoints(ctx context.Context, now time.Time, retention time.Duration, limit int) (int, error)

    // PurgeNotifications hard-deletes notifications older than retention (MAINT-003).
    // Delegates to NotificationRepository.DeleteBefore for the actual deletion.
    PurgeNotifications(ctx context.Context, retention time.Duration) (int, error)

    // ArchiveAuditLogs moves audit records older than retention to cold storage (MAINT-004).
    // Orchestrates a fetch-upload-delete cycle:
    //   1. Fetch batch via AuditRepository.ListOlderThan
    //   2. Serialize and upload to S3 ArchiveBucket (Glacier tier)
    //   3. Delete archived records via AuditRepository.DeleteIDs
    // Returns the count of records successfully archived.
    ArchiveAuditLogs(ctx context.Context, retention time.Duration, batchSize int) (int, error)

    // PurgeExpiredIdempotencyKeys removes idempotency records past their expiration.
    // Prevents unbounded growth of the idempotency_keys table.
    // Executes: DELETE FROM idempotency_keys WHERE expires_at < $now
    // Returns the count of records deleted.
    PurgeExpiredIdempotencyKeys(ctx context.Context, now time.Time) (int, error)

    // PurgeSecurityEvents removes security event records older than the retention period.
    // Prevents unbounded growth of the security_events table used for abuse tracking.
    // Executes: DELETE FROM security_events WHERE attempted_at < ($now - $retention)
    // Default retention: 7 days.
    // Returns the count of records deleted.
    PurgeSecurityEvents(ctx context.Context, now time.Time, retention time.Duration) (int, error)
}

type ForecastReconciler interface {
    ReconcileStaleRuns(ctx context.Context, now time.Time, threshold time.Duration) (int64, error)
}
```

### 7.5 Calibration Service

**Flow**: `OBS-006`

Updates `calibration_coefficients` based on recent verification results.

*   **Logic**:
    1.  Identify locations with sufficient verification data for regression analysis.
    2.  Construct an `InferencePayload` with `TaskType=calibration` and appropriate `CalibrationTimeRange`.
    3.  Dispatch to RunPod Client. The worker performs regression analysis using historical data.
    4.  **Bounded Automation**: The RunPod worker checks if proposed coefficient changes are within safe bounds (< 15% delta).
        - If within bounds: Updates `calibration_coefficients` directly.
        - If exceeds bounds: Inserts into `calibration_candidates` with `violation_reason` for manual Ops review.

```go
type CalibrationService interface {
    // UpdateCoefficients triggers a RunPod task (TaskType=calibration) to perform
    // regression analysis, rather than computing locally. Results are either
    // auto-applied or flagged for manual review based on safety bounds.
    UpdateCoefficients(ctx context.Context, now time.Time) (int, error)
}
```

### 7.6 Archiver Service

**Flow**: `WPLC-009`, `OBS-007`

Moves expired Event Mode WatchPoints to archived status and triggers post-event summary generation.

*   **Query**:
    ```sql
    UPDATE watchpoints
    SET status = 'archived', archived_at = $1, archived_reason = 'event_passed'
    WHERE status = 'active'
    AND time_window_end IS NOT NULL
    AND time_window_end < ($1 - INTERVAL '1 hour')
    RETURNING id, tile_id
    ```
*   **Logic**:
    1.  Uses the `idx_watchpoints_archive_candidates` index for efficiency.
    2.  For each archived WatchPoint, enqueue an `EvalMessage` with `Action=generate_summary` to the Standard Evaluation Queue.
    3.  The Eval Worker's `SummaryGenerator` will compute and persist the post-event accuracy report.
    4.  Emits audit events for visibility.

```go
type ArchiverService interface {
    // ArchiveExpired marks expired WatchPoints as archived and enqueues
    // EvalMessage with Action=generate_summary for post-event accuracy reports.
    ArchiveExpired(ctx context.Context, now time.Time, buffer time.Duration) (int64, error)
}
```

### 7.10 Subscription Enforcer

**Flow**: `BILL-005`, `BILL-011`

Manages negative lifecycle events for billing delinquency and usage overages.

```go
// SubscriptionEnforcer manages negative lifecycle events for billing.
type SubscriptionEnforcer interface {
    // EnforcePaymentFailure checks for payment_failed_at > 7 days.
    // Action: Calls WatchPointRepo.PauseAllByOrgID(..., PausedReasonBillingDelinquency).
    EnforcePaymentFailure(ctx context.Context, now time.Time) (int, error)

    // EnforceOverage checks for overage_started_at > 14 days.
    // Action: Calls WatchPointRepo.PauseExcessByOrgID.
    EnforceOverage(ctx context.Context, now time.Time) (int, error)
}
```

---

### 7.9 Deferred Notification Recovery

**Flow**: `NOTIF-002`

Re-queues notifications that were deferred due to Quiet Hours once the quiet period ends.

*   **Schedule**: Rate(15 minutes) - Multiplexed via Archiver
*   **Query**:
    ```sql
    SELECT nd.id, nd.notification_id, n.payload
    FROM notification_deliveries nd
    JOIN notifications n ON n.id = nd.notification_id
    WHERE nd.status = 'deferred'
    AND nd.next_retry_at <= $1
    LIMIT 100
    ```

*   **Logic**:
    1.  Query `notification_deliveries` where `status='deferred'` AND `next_retry_at <= NOW()`.
    2.  For each matching delivery:
        a.  Reset status to `pending` via atomic UPDATE.
        b.  Construct `NotificationMessage` from the stored notification payload.
        c.  Publish to SQS `notification-queue` with no delay.
    3.  Track count for metrics and job history.

*   **Idempotency**: The status transition `deferred -> pending` is atomic. If the job
    runs twice, the second run will find no matching records (already transitioned).

```go
type DeferredNotificationService interface {
    // RequeueDeferredNotifications finds deferred deliveries past their resume time
    // and re-enqueues them to the notification queue for processing.
    RequeueDeferredNotifications(ctx context.Context, now time.Time, limit int) (int, error)
}
```

### 7.7 Stripe Syncer

**Flow**: `SCHED-003`

Ensures local billing state matches Stripe, correcting any missed webhooks.

*   **Logic**:
    1.  Select organizations where `last_billing_sync_at < (now - 24 hours)`.
    2.  For each org, fetch the Subscription object from the Stripe API.
    3.  Compare local `plan` and `status` with Stripe's values.
    4.  If mismatched, perform an update (similar to the Webhook Handler) and log a warning metric `BillingStateDrift`.
    5.  Update `last_billing_sync_at`.
    6.  **Headless Repair**: Scan active organizations where `stripe_customer_id` is NULL. Call `BillingService.EnsureCustomer` to backfill the Stripe customer ID. This handles edge cases where customer creation failed during signup but the organization was still created.

```go
type StripeSyncer interface {
    SyncAtRisk(ctx context.Context, now time.Time, stalenessThreshold time.Duration, limit int) (int, error)
}
```

### 7.8 Forecast Tier Transition

**Flow**: `MAINT-001`

Enforces data retention policies on S3 to manage costs.

*   **Logic**:
    1.  Query `forecast_runs` where status is 'complete'.
    2.  **Nowcast Rule**: If `run_timestamp < (now - 7 days)`, delete S3 objects and **update `forecast_runs` status to 'deleted'**.
    3.  **Medium-Range Rule**: If `run_timestamp < (now - 90 days)`, delete S3 objects and **update `forecast_runs` status to 'deleted'**.
    4.  Uses S3 Batch Delete or concurrent DeleteObject calls.

```go
type TierTransitionService interface {
    EnforceRetention(ctx context.Context, now time.Time) (int, error)
}
```

---

## 8. Flow Coverage

| Flow ID | Description | Component | Method |
|---|---|---|---|
| `FCST-003` | Data Polling | `DataPoller` | `Poll` |
| `FCST-004` | Forecast Retry/Fail | `ForecastReconciler` | `ReconcileStaleRuns` |
| `WPLC-009` | Archive WatchPoints | `ArchiverService` | `ArchiveExpired` |
| `NOTIF-002` | Quiet Hours Recovery | `DeferredNotificationService` | `RequeueDeferredNotifications` |
| `NOTIF-003` | Digest Generation | `DigestScheduler` | `TriggerDigests` |
| `SCHED-001` | Rate Limit Reset | `UsageAggregator` | `SnapshotDailyUsage` |
| `SCHED-002` | Usage Aggregation | `UsageAggregator` | `SnapshotDailyUsage` |
| `SCHED-003` | Stripe Sync | `StripeSyncer` | `SyncAtRisk` |
| `MAINT-001` | Forecast Tier Transition | `TierTransitionService` | `EnforceRetention` |
| `MAINT-002` | Archived WP Cleanup | `CleanupService` | `PurgeArchivedWatchPoints` |
| `MAINT-005` | Soft Delete Cleanup | `CleanupService` | `PurgeSoftDeletedOrgs` |
| `MAINT-006` | Expired Invite Cleanup | `CleanupService` | `PurgeExpiredInvites` |
| `OBS-005` | Verification Pipeline | `VerificationService` | `TriggerVerification` |
| `OBS-006` | Calibration Update | `CalibrationService` | `UpdateCoefficients` |
| `SCHED-006` | Idempotency Cleanup | `CleanupService` | `PurgeExpiredIdempotencyKeys` |
| `MAINT-007` | Security Events Cleanup | `CleanupService` | `PurgeSecurityEvents` |