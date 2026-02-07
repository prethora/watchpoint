# Flow Simulations: Forecast Generation (FCST)

> **Range**: FCST-001 to FCST-005
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## FCST-003: Data Poller Execution

**Primary Components**: `cmd/data-poller`, `internal/db`, `internal/forecast`, `external/runpod`

**Trigger**: `EventBridge Rule: rate(15 minutes)`

**Preconditions**:
*   Upstream NOAA S3 buckets are accessible.
*   Database connection is healthy.
*   `RUNPOD_API_KEY` is configured.

**Execution Sequence**:
1.  **[DataPoller]** `main.go` starts, initializes `RunRepository` and `RunPodClient`.
2.  **[DataPoller]** Loops through configured models (`medium_range`, `nowcast`).
3.  **[DataPoller]** Calls `RunRepository.GetLatest(ctx, model)`.
4.  **[DB]** Executes `SELECT * FROM forecast_runs WHERE model=$1 ORDER BY run_timestamp DESC LIMIT 1`. Returns `last_run_ts`.
5.  **[DataPoller]** Calls `UpstreamSource.CheckAvailability(ctx, last_run_ts)`.
    *   *Logic*: Lists objects in NOAA S3 bucket. Parses timestamps from keys (e.g., `gfs.20231027.06z`).
6.  **[Logic]** If `upstream_ts <= last_run_ts`: Log "No new data" and exit loop.
7.  **[Logic]** If `upstream_ts > last_run_ts`:
    *   **[DataPoller]** Prepares `InferencePayload`.
    *   *Branch (Nowcast)*:
        *   Calls `CalibrationRepo.GetAll(ctx)`.
        *   **[DB]** `SELECT * FROM calibration_coefficients`.
        *   Injects map into `Payload.InputConfig.Calibration`.
    *   *Branch (Medium Range)*:
        *   Injects GFS S3 paths into `Payload.InputConfig.SourcePaths`.
8.  **[DataPoller]** Calls `RunPodClient.TriggerInference(ctx, payload)`.
    *   **[HTTP]** POST `https://api.runpod.io/v2/{endpoint_id}/run`.
    *   **[Response]** `{"id": "runpod_job_123", "status": "IN_QUEUE"}`.
9.  **[DataPoller]** Calls `RunRepository.Create(ctx, &ForecastRun{...})`.
10. **[DB]** Executes:
    ```sql
    INSERT INTO forecast_runs (
        id, model, run_timestamp, status, external_id, created_at
    ) VALUES (
        'run_uuid', 'nowcast', '2023-10-27T12:00:00Z', 'running', 'runpod_job_123', NOW()
    );
    ```

**Data State Changes**:
*   `forecast_runs`: New row inserted with `status='running'`, `external_id='runpod_job_123'`.

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: `external_id` column and `InferencePayload` struct).

---

## FCST-001: Medium-Range Forecast Generation (Async Execution)

**Primary Components**: `worker/runpod`, `cmd/batcher`, `internal/db`

**Trigger**: `DataPoller` trigger (from FCST-003)

**Preconditions**:
*   RunPod worker successfully triggered.
*   Target S3 bucket `watchpoint-forecasts` exists.

**Execution Sequence**:
1.  **[RunPod Worker]** Starts container. Receives `InferencePayload`.
2.  **[RunPod Worker]** Calls `ZarrWriter.clean_slate(s3_path)`.
    *   *Action*: `s3.list_objects_v2(prefix=...)` then `s3.delete_objects(...)`. Ensures no zombie chunks.
3.  **[RunPod Worker]** Runs `InferenceHandler`.
    *   Downloads GFS data using `s3fs`.
    *   Runs Atlas model inference on GPU.
    *   Translates variables (Kelvin -> Celsius).
4.  **[RunPod Worker]** Writes Zarr chunks to S3.
5.  **[RunPod Worker]** Writes `_SUCCESS` marker file to S3 root.
6.  **[RunPod Worker]** Exits with success.
7.  **[AWS S3]** Emits `s3:ObjectCreated:Put` event for key `.../_SUCCESS`.
8.  **[Batcher]** Lambda invoked by S3 event.
9.  **[Batcher]** Parses S3 Key to extract `model` and `run_timestamp`.
10. **[Batcher]** Calls `RunRepository.MarkComplete(ctx, timestamp, s3_path)`.
11. **[DB]** Executes:
    ```sql
    UPDATE forecast_runs
    SET status = 'complete', storage_path = $2, updated_at = NOW()
    WHERE model = $1 AND run_timestamp = $3;
    ```
12. **[Batcher]** Proceeds to Evaluation Logic (Flow EVAL-001).

**Data State Changes**:
*   **S3**: Zarr store created + `_SUCCESS`.
*   **DB**: `forecast_runs` status transitions `running` -> `complete`.

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: Batcher write responsibility).

---

## FCST-002: Nowcast Forecast Generation (Async Execution)

**Primary Components**: `worker/runpod`, `cmd/batcher`, `internal/db`

**Trigger**: `DataPoller` trigger (from FCST-003)

**Preconditions**:
*   Payload includes `InputConfig.Calibration` map.

**Execution Sequence**:
1.  **[RunPod Worker]** Starts. Receives `InferencePayload` containing `Calibration` map.
2.  **[RunPod Worker]** Calls `ZarrWriter.clean_slate(s3_path)`.
3.  **[RunPod Worker]** Runs `InferenceHandler`.
    *   Fetches GOES/MRMS data.
    *   Runs StormScope model.
4.  **[RunPod Worker]** **Calibration Step**:
    *   Iterates over output grid points.
    *   Determines `TileID` for point (lat/lon math).
    *   Lookups coefficients in injected `Calibration` map.
    *   Computes: `prob = base + (reflectivity * slope)`.
5.  **[RunPod Worker]** Writes Zarr chunks and `_SUCCESS` marker.
6.  **[AWS S3]** Emits event -> **[Batcher]** invoked.
7.  **[Batcher]** Calls `RunRepository.MarkComplete(...)`.
8.  **[DB]** `UPDATE forecast_runs SET status='complete' ...`

**Data State Changes**:
*   **S3**: Zarr store created.
*   **DB**: `forecast_runs` status `running` -> `complete`.

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: `CalibrationCoefficients` JSON tags).

---

## FCST-004: Forecast Retry (Recovery)

**Primary Components**: `cmd/archiver` (Multiplexer), `internal/scheduler`

**Trigger**: `EventBridge Rule: rate(1 hour)` with payload `{"task": "reconcile_forecasts"}`

**Preconditions**:
*   A job initiated > 2 hours ago is still in `running` state (e.g., RunPod crashed or timed out silently).

**Execution Sequence**:
1.  **[Archiver]** Receives payload, calls `ForecastReconciler.ReconcileStaleRuns`.
2.  **[Reconciler]** Queries stale runs:
    *   **[DB]** `SELECT * FROM forecast_runs WHERE status='running' AND created_at < NOW() - INTERVAL '2 hours'`.
3.  **[Reconciler]** Loop over stuck runs.
4.  **[Reconciler]** Calls `RunPodClient.GetJobStatus(ctx, run.ExternalID)`.
    *   **[HTTP]** GET `https://api.runpod.io/v2/{endpoint_id}/status/{external_id}`.
5.  **[Logic]**
    *   *Case A (Still Running)*: Log and skip (RunPod might be slow).
    *   *Case B (Failed/Dead)*:
        *   Increment `RetryCount`.
        *   If `RetryCount < 3`:
            *   Prepare new `InferencePayload` (same timestamp).
            *   Set `Options.ForceRebuild = true`.
            *   **[DataPoller/Reconciler]** Call `RunPodClient.TriggerInference`.
            *   **[DB]** `UPDATE forecast_runs SET retry_count = retry_count + 1, external_id = $new_id WHERE id = $id`.
        *   If `RetryCount >= 3`:
            *   **[DB]** `UPDATE forecast_runs SET status = 'failed', failure_reason = 'Max retries exhausted' WHERE id = $id`.
            *   Triggers Flow FCST-005.

**Data State Changes**:
*   **DB**: `forecast_runs` updates (`retry_count`, `external_id`, or `status`).

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: `retry_count`, `external_id`, `failure_reason` columns).

---

## FCST-005: Fallback to Stale (Serve Stale)

**Primary Components**: `cmd/api`, `internal/forecasts`, `internal/db`

**Trigger**: API Request `GET /v1/forecasts/point` (User Request)

**Preconditions**:
*   Latest run is `failed` (via FCST-004) or simply missing (Upstream delay).
*   Previous run exists and is `complete`.

**Execution Sequence**:
1.  **[API]** `ForecastHandler` calls `ForecastService.GetPointForecast`.
2.  **[Service]** Calls `RunRepository.GetLatestServing(ctx, model)`.
    *   **[DB]** `SELECT * FROM forecast_runs WHERE model=$1 AND status='complete' ORDER BY run_timestamp DESC LIMIT 1`.
3.  **[Service]** Checks staleness.
    *   Logic: `time.Since(run.RunTimestamp) > Threshold` (e.g., 8 hours for Medium Range).
4.  **[Logic]** Staleness detected (e.g., 12 hours old).
5.  **[Service]** Fetches data from S3 using the *stale* `run.StoragePath`.
6.  **[Service]** Populates `ForecastResponse`.
    *   Sets `Metadata.Status = "stale"`.
    *   Adds `Metadata.Warnings`: `[{Code: "forecast_stale", Message: "Data is 12h old..."}]`.
7.  **[API]** Returns HTTP 200 with the stale data + warning.

**Data State Changes**:
*   None (Read-only).

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: `GetLatestServing` repository method and `Metadata` response struct).


---

## FCST-006: Forecast Data Retention (Cleanup)

**Primary Components**: `cmd/archiver` (Multiplexer), `internal/scheduler` (TierTransitionService)

**Trigger**: `EventBridge Rule: cron(0 4 * * ? *)` (Daily 04:00 UTC) with payload `{"task": "transition_forecasts"}`

**Preconditions**:
*   `forecast_runs` table contains old records.
*   S3 bucket contains corresponding Zarr stores.

**Execution Sequence**:
1.  **[Archiver]** Receives payload, acquires lock `transition_forecasts:2026-02-03`, calls `TierTransitionService.EnforceRetention(ctx, now)`.
2.  **[Service]** Identifies candidates for deletion.
    *   **[DB]** `SELECT * FROM forecast_runs WHERE status='complete' AND ((model='nowcast' AND run_timestamp < NOW() - INTERVAL '7 days') OR (model='medium_range' AND run_timestamp < NOW() - INTERVAL '90 days'))`.
3.  **[Service]** Iterates over candidates.
4.  **[Service]** Deletes S3 objects.
    *   *Action*: `s3.DeleteObject` (or BatchDelete) for the prefix `run.StoragePath`.
    *   *Resilience*: Ignores `404 Not Found` (already deleted).
5.  **[Service]** Updates Database.
    *   **[DB]** `UPDATE forecast_runs SET status = 'deleted', storage_path = '' WHERE id = $id`.
6.  **[Service]** Emits metrics: `ForecastsDeleted`.

**Data State Changes**:
*   **DB**: `forecast_runs.status` transitions from `'complete'` to `'deleted'`.
*   **S3**: Objects removed.

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: `status` CHECK constraint update).

---

## EVAL-001: Evaluation Pipeline (Core Loop)

**Primary Components**: `cmd/batcher`, `worker/eval`, `internal/db`

**Trigger**: `S3 Event: ObjectCreated` (Key: `.../_SUCCESS`)

**Preconditions**:
*   `watchpoints` table populated.
*   Zarr store exists in S3 (with Write-Side Halo).

**Execution Sequence**:
1.  **[Batcher]** `Handler` invoked. Parses S3 Key to get `ForecastType`, `RunTimestamp`.
2.  **[Batcher]** Checks idempotency.
    *   **[DB]** `SELECT id, status FROM forecast_runs WHERE ...`
    *   If `status` is `'complete'` or `'running'`, logs and exits (unless `force_rebuild` flag implied).
3.  **[Batcher]** Generates Traceability IDs.
    *   `BatchID`: `uuid.New()`
    *   `TraceID`: `uuid.New()`
4.  **[Batcher]** Queries Active Tiles.
    *   **[DB]** `SELECT tile_id, COUNT(*) FROM watchpoints WHERE status='active' GROUP BY tile_id` (Index-Only Scan).
5.  **[Batcher]** Iterates Tiles & Enqueues.
    *   Constructs `EvalMessage` with `BatchID`, `TraceID`.
    *   **[SQS]** Sends batch to `eval-queue-{type}`.
6.  **[Batcher]** Emits `ForecastReady` metric.
7.  **[Eval Worker]** Lambda invoked by SQS.
8.  **[Eval Worker]** Idempotency Check.
    *   **[DB]** Checks `watchpoint_evaluation_state.last_forecast_run` vs message timestamp.
    *   If matched, checks `notifications` table. If found, re-publishes SQS without DB write.
9.  **[Eval Worker]** Reads Forecast.
    *   **[S3]** Reads **single** Zarr chunk for `tile_id` (relies on Write-Side Halo).
    *   *Error Handling*: If `FileNotFound` (expired), log warning, ACK message, exit.
10. **[Eval Worker]** Fetches WatchPoints.
    *   **[DB]** `SELECT * FROM watchpoints WHERE tile_id=$1 ...`
11. **[Eval Worker]** Evaluates Conditions.
    *   Uses **Bilinear Interpolation** to extract point data.
    *   Checks conditions (Event Mode) or Rolling Window (Monitor Mode - see EVAL-003).
    *   Applies **Hysteresis** (Clear if `< Threshold - Buffer`).
12. **[Eval Worker]** Commits State.
    *   **[DB Transaction]**:
        *   Update `watchpoint_evaluation_state`.
        *   Insert `notifications` (if triggered).
        *   Increment `event_sequence`.
    *   **[SQS]** Sends `NotificationMessage` (if triggered).

**Data State Changes**:
*   **DB**: `evaluation_state` updated (`last_evaluated_at`, `event_sequence`). New `notifications` row.
*   **SQS**: Message added to `notification-queue`.

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: `BatchID` generation, `FileNotFound` handling, Hysteresis constants).

---

## EVAL-002: Hot Tile Pagination

**Primary Components**: `cmd/batcher`, `worker/eval`

**Trigger**: `EVAL-001` step 4 returns a tile count > 500.

**Preconditions**:
*   A single tile contains e.g., 1200 WatchPoints.

**Execution Sequence**:
1.  **[Batcher]** Detects `count=1200`.
2.  **[Batcher]** Calculates pages: `ceil(1200 / 500) = 3`.
3.  **[Batcher]** Loop `p` from 0 to 2:
    *   Create `EvalMessage`: `{TileID: "X", Page: p, PageSize: 500, TotalItems: 1200}`.
    *   **[SQS]** Send message.
4.  **[Eval Worker]** Receives message (e.g., Page 1).
5.  **[Eval Worker]** Fetches WatchPoints.
    *   **[DB]** `SELECT * FROM watchpoints WHERE tile_id=$1 ... ORDER BY id LIMIT 500 OFFSET 500`.
    *   *Note*: `OFFSET` is efficient enough given max pages is low (~10-20 max practical).
6.  **[Eval Worker]** Processes 500 items normally.

**Data State Changes**:
*   No specific state change; purely operational flow.

**Gap Analysis**:
*   None.

---

## EVAL-003: Monitor Mode Evaluation (Combined)

**Primary Components**: `worker/eval`

**Trigger**: `EVAL-001` worker processes a WatchPoint with `monitor_config != null`.

**Preconditions**:
*   Forecast data loaded.
*   WatchPoint has `active_hours` defined (e.g., 08:00-18:00 Local).

**Execution Sequence**:
1.  **[Eval Worker]** Calculates Rolling Window.
    *   Start: `Now`. End: `Now + WindowHours`.
2.  **[Eval Worker]** **Timezone Conversion** (EVAL-003a).
    *   Converts Forecast Time Index (UTC) -> WatchPoint Local Time.
3.  **[Eval Worker]** **Active Hours Filter** (EVAL-003b).
    *   Masks forecast data to exclude hours outside 08:00-18:00 Local.
4.  **[Eval Worker]** Identifies Violation Periods.
    *   Scans window for contiguous blocks where conditions are met.
    *   Result: `[{"start": "T14:00", "end": "T16:00", "val": 80}]`.
5.  **[Eval Worker]** **Temporal Deduplication** (EVAL-003c).
    *   Fetches `state.seen_threats` (JSONB).
    *   Iterates new violations. Checks for **Overlap** with existing threats.
    *   *Scenario*:
        *   Existing: `13:00-15:00`. New: `14:00-16:00`.
        *   Result: Overlap detected. Update existing entry to `13:00-16:00`. **No Notification**.
    *   *Scenario*:
        *   Existing: `None`. New: `14:00-16:00`.
        *   Result: New Threat. **Trigger Notification**.
6.  **[Eval Worker]** **Pruning**.
    *   Removes entries from `seen_threats` where `end_time < Now`.
7.  **[Eval Worker]** Digest Data Prep.
    *   Populates `MonitorSummary` struct (MaxValues, TriggeredPeriods).
    *   Stores in `state.last_forecast_summary` (JSONB).
8.  **[Eval Worker]** Persists State.
    *   **[DB]** `UPDATE watchpoint_evaluation_state SET seen_threats = $jsonb ...`.

**Data State Changes**:
*   **DB**: `seen_threats` (JSONB) updated. `last_forecast_summary` updated.

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: `seen_threats` JSONB schema, `MonitorSummary` type, Timezone logic).

---

## EVAL-004: Config Version Mismatch (Race Condition Handling)

**Primary Components**: `worker/eval`, `internal/db`

**Trigger**: `EVAL-001` (Eval Worker) processes a WatchPoint where `wp.config_version != state.config_version`.

**Preconditions**:
*   User updated WatchPoint (e.g., changed location or threshold) via API *after* the Batcher generated the SQS message but *before* the Worker processed it.

**Execution Sequence**:
1.  **[Eval Worker]** Fetches WatchPoint (`v2`) and EvaluationState (`v1`).
2.  **[Eval Worker]** Detects version mismatch (`2 != 1`).
3.  **[Eval Worker]** **Geometric Consistency Check**:
    *   Calculates `current_tile_id` based on `wp.Location`.
    *   Compares with `message.TileID` (the Zarr chunk currently loaded in memory).
4.  **[Logic]**
    *   *Case A (Same Tile)*: Safe to evaluate, but logic dictates "Reset".
    *   *Case B (Different Tile)*: **Unsafe**. Data in memory is for Tile X, WP is now in Tile Y.
5.  **[Eval Worker]** Executes **Fast Fail & Reset**:
    *   Skips evaluation logic (no condition check).
    *   **[DB]** Updates state to sync version only.
    ```sql
    UPDATE watchpoint_evaluation_state
    SET config_version = $wp_version,
        trigger_state_changed_at = NULL, -- Reset hysteresis baseline
        previous_trigger_state = FALSE
    WHERE watchpoint_id = $id;
    ```
6.  **[Eval Worker]** ACKs SQS message.
7.  **[Result]** The WatchPoint is not evaluated this cycle. It will be picked up correctly (in the new tile if moved) by the *next* Batcher run (`FCST-001/002`).

**Data State Changes**:
*   **DB**: `evaluation_state` synchronized to new version. Hysteresis reset.

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: Explicit "Fast Fail" logic in worker).

---

## EVAL-005: First Evaluation (Baseline Establishment)

**Primary Components**: `worker/eval`, `internal/db`

**Trigger**: `EVAL-001` processes a WatchPoint where `last_evaluated_at` is NULL.

**Preconditions**:
*   WatchPoint recently created.

**Execution Sequence**:
1.  **[Eval Worker]** Detects `last_evaluated_at IS NULL`.
2.  **[Eval Worker]** Performs full evaluation against forecast data.
3.  **[Logic - Monitor Mode]**:
    *   Calculates `MonitorSummary` (Max/Min values over window).
    *   Identifies `TriggeredPeriods`.
    *   **Action**: Suppress Notification.
    *   **Reasoning**: Avoid "Alert Shock". Establish baseline so *next* run detects changes.
4.  **[Logic - Event Mode]**:
    *   Checks conditions.
    *   **Action**: If triggered, Enqueue Notification immediately.
5.  **[Eval Worker]** Persists State.
    *   **[DB]** Updates `last_forecast_summary` with the `MonitorSummary` struct.
    *   **[DB]** Updates `seen_threats` (Monitor Mode).

**Data State Changes**:
*   **DB**: `evaluation_state` populated. `notifications` inserted (only if Event Mode triggered).

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: `MonitorSummary` struct).

---

## EVAL-006: Queue Backlog Recovery (Procedural)

**Primary Components**: `CloudWatch`, `PagerDuty`, `Ops CLI`

**Trigger**: `CloudWatch Alarm`: `EvalQueueDepth > 1000` OR `EvaluationLag > 1800s`.

**Preconditions**:
*   Workers are falling behind forecast generation rate.

**Execution Sequence**:
1.  **[CloudWatch]** Alarm state transitions to ALARM. Sends SNS -> PagerDuty.
2.  **[Operator]** Receives page. Checks Dashboard.
    *   *metric*: `WatchPoint/EvaluationLag` (Average lag per tile).
3.  **[Operator]** Diagnoses issue:
    *   *Scenario A*: RunPod churn causing empty Zarr files? (Check logs).
    *   *Scenario B*: Spike in traffic? (Check `ActiveWatchPoints`).
4.  **[Operator]** Executes Recovery Action (Scenario B):
    *   *Command*: Increase Lambda Provisioned Concurrency.
    *   `aws lambda put-provisioned-concurrency-config --function-name eval-worker-standard --provisioned-concurrent-executions 20`
5.  **[Alternative - Purge]**:
    *   If lag > 6 hours (data useless), Operator runs purge script.
    *   `aws sqs purge-queue --queue-url ...`
    *   *Result*: System skips to next fresh forecast.

**Data State Changes**:
*   **AWS Config**: Lambda concurrency updated.

**Gap Analysis**:
*   None. (Procedural flow confirmed).

---

## NOTIF-001: Immediate Notification Delivery

**Primary Components**: `worker/email`, `worker/webhook`, `internal/db`

**Trigger**: `SQS: notification-queue` message received.

**Preconditions**:
*   `notification_deliveries` row exists (created by Eval Worker).

**Execution Sequence**:
1.  **[Worker]** Receives message. Reads `RetryCount` from payload.
2.  **[Worker]** Fetches `Organization` (for Timezone) and `WatchPoint`.
3.  **[Worker]** Resolves Template.
    *   *Logic*: Lookup `wp.TemplateSet`. If missing/invalid, fallback to `"default"`.
    *   *Audit*: Logs warning if fallback occurs.
4.  **[Worker]** Prepares Payload.
    *   Reads `notifications.payload` (Forecast Snapshot).
    *   Formats for channel (e.g., Slack JSON).
5.  **[Worker]** Attempts Delivery (`Channel.Deliver`).
    *   **[HTTP/SMTP]** calls provider.
6.  **[Branch - Success]**:
    *   **[DB]** `UPDATE notification_deliveries SET status='sent', provider_message_id=$id`.
    *   *Note*: For generic webhooks, generates `generic-{ts}-{uuid}`.
7.  **[Branch - Retryable Failure]** (e.g., HTTP 500, 429):
    *   Calculate Backoff: `5s * 2^RetryCount`.
    *   **[SQS]** Publish **NEW** message with `DelaySeconds=Backoff` and `RetryCount++`.
    *   **[DB]** `UPDATE notification_deliveries SET status='retrying', next_retry_at=$future`.
    *   **[SQS]** ACK original message.
8.  **[Branch - Terminal Failure]** (e.g., 401, Blocked):
    *   **[DB]** `UPDATE notification_deliveries SET status='failed', failure_reason='...'`.
    *   **[Logic]** Check if all channels failed (`CheckAggregateFailure`). If so, emit metric `NotificationFailed`.

**Data State Changes**:
*   **DB**: Delivery status updated.
*   **SQS**: New delayed message published (on retry).

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: `RetryCount` field, `CheckAggregateFailure` method).

---

## NOTIF-002: Quiet Hours Suppression & Deferral

**Primary Components**: `worker/email` (or webhook), `internal/db`, `cmd/archiver`

**Trigger**: `NOTIF-001` worker processes message during User's night.

**Preconditions**:
*   Org has Quiet Hours enabled (e.g., 22:00 - 08:00).
*   User Timezone is `Asia/Tokyo`. Current User Time is 02:00.

**Execution Sequence**:
1.  **[Worker]** Calls `PolicyEngine.Evaluate(notification, orgPrefs)`.
2.  **[PolicyEngine]** Checks exceptions (Urgency=Critical?).
3.  **[PolicyEngine]** Determines `Result=Defer`, `ResumeAt=08:00 Today`.
4.  **[Worker]** Updates Database.
    *   **[DB]** `UPDATE notification_deliveries SET status='deferred', next_retry_at='2026-02-03T08:00:00+09:00'`.
5.  **[Worker]** ACKs SQS message (removing it from hot queue).
6.  **[Time Passes...]**
7.  **[Archiver]** `RequeueDeferredNotifications` job runs (Flow `SCHED-00X`).
8.  **[Archiver]** Queries: `SELECT * FROM notification_deliveries WHERE status='deferred' AND next_retry_at <= NOW()`.
9.  **[Archiver]** Re-Enqueues:
    *   **[SQS]** Sends original payload to `notification-queue`.
    *   **[DB]** Updates status `deferred` -> `pending`.

**Data State Changes**:
*   **DB**: Status transitions `pending` -> `deferred` -> `pending` -> `sent`.

**Gap Analysis**:
*   None. (Addressed by Refactoring Prompt: `deferred` status, `RequeueDeferredNotifications` job).

---

## NOTIF-003: Digest Generation & Delivery

**Primary Components**: `cmd/archiver` (Scheduler), `worker/email`, `internal/db`

**Trigger**: `EventBridge Rule: rate(1 minute)` with payload `{"task": "trigger_digests"}`

**Preconditions**:
*   Organizations exist with `next_digest_at <= NOW()`.
*   WatchPoints in those organizations have Monitor Mode data (`last_forecast_summary`) populated.

**Execution Sequence**:
1.  **[Archiver]** Receives payload. Calls `DigestScheduler.TriggerDigests(ctx, now)`.
2.  **[Scheduler]** Queries due organizations (Optimized Index Scan).
    *   **[DB]** `SELECT id, notification_preferences, timezone FROM organizations WHERE next_digest_at <= $now LIMIT 100`.
3.  **[Scheduler]** Iterates results. For each Org:
    *   **[SQS]** Enqueues `NotificationMessage` (Type: `monitor_digest`) to `notification-queue`.
    *   Calculates `next_run`: Uses `preferences.digest.schedule` + `timezone` logic to find next wall-clock occurrence.
    *   **[DB]** `UPDATE organizations SET next_digest_at = $next_run WHERE id = $id`.
4.  **[Email Worker]** Receives SQS message.
5.  **[Worker]** Calls `DigestGenerator.Generate`.
    *   Fetches all Monitor Mode WatchPoints for Org.
    *   Reads `last_digest_content` (Previous) and `last_forecast_summary` (Current) for each.
    *   **[Logic]** Performs comparison (NOTIF-003b).
    *   **[Logic]** Checks `SendEmpty` preference. If digest has 0 triggers/changes and `SendEmpty=false`: Abort (Log "Skipped").
    *   **[Logic]** If valid: Renders content using `TemplateSet` (default: "digest_default"). Truncates > 20 items.
6.  **[Worker]** Delivers via Channel (Email).
7.  **[Worker]** Commits State.
    *   **[DB]** Updates `watchpoint_evaluation_state.last_digest_content` = current summary (advances baseline).

**Data State Changes**:
*   **DB**: `organizations.next_digest_at` updated. `evaluation_state.last_digest_content` updated.
*   **SQS**: Digest message processed.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `next_digest_at` column and `SendEmpty` config).

---

## NOTIF-003b: Digest Comparison (Sub-Flow)

**Primary Components**: `internal/notifications/digest`

**Trigger**: Called by `DigestGenerator` during NOTIF-003.

**Preconditions**:
*   Evaluation State contains `last_digest_content` (Snapshot A) and `last_forecast_summary` (Snapshot B).

**Execution Sequence**:
1.  **[Generator]** Deserializes JSONB snapshots into `MonitorSummary` structs.
2.  **[Generator]** Compares "Max Values" (e.g., Rain Probability).
    *   *Logic*: `Delta = Current.MaxVal - Previous.MaxVal`.
    *   If `abs(Delta) > Threshold` (e.g., 20%): Add to `Changes` list.
3.  **[Generator]** Compares "Triggered Periods".
    *   *Logic*: If `Current` has periods and `Previous` did not -> "New Threat".
    *   If `Current` has no periods and `Previous` did -> "Cleared".
4.  **[Generator]** Returns `DigestDiff` struct containing only significant changes to be rendered in the email template.

**Data State Changes**:
*   None (Pure Logic).

**Gap Analysis**:
*   None.

---

## NOTIF-004: Webhook Delivery with Retry (Transient)

**Primary Components**: `worker/webhook`, `internal/db`

**Trigger**: SQS `notification-queue` message.

**Preconditions**:
*   Destination endpoint is temporarily unstable (HTTP 500/502/503).

**Execution Sequence**:
1.  **[Worker]** Calls `WebhookChannel.Deliver`.
2.  **[HTTP]** POST to destination. Returns `503 Service Unavailable`.
3.  **[Worker]** Identifies `Retryable: true`.
4.  **[Worker]** Reads `RetryCount` from message (e.g., 0).
5.  **[Logic]** Calculates Backoff: `1s * 5^0 = 1s`.
6.  **[Worker]** **Updates DB Persistence** (Observability Requirement).
    *   **[DB]** `UPDATE notification_deliveries SET status='retrying', attempt_count=1, next_retry_at=NOW()+1s WHERE id=$id`.
7.  **[Worker]** Re-Publishes to SQS.
    *   **[SQS]** `SendMessage(QueueUrl, Body, DelaySeconds=1)`. Payload includes `RetryCount=1`.
8.  **[Worker]** ACKs original SQS message.

**Data State Changes**:
*   **DB**: Delivery marked `retrying`.
*   **SQS**: New delayed message enqueued.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit DB update step).

---

## NOTIF-005: Webhook Rate Limit Handling (Long Delay)

**Primary Components**: `worker/webhook`, `cmd/archiver`

**Trigger**: SQS message. Destination returns `429 Too Many Requests`.

**Preconditions**:
*   Response header `Retry-After: 3600` (1 hour).

**Execution Sequence**:
1.  **[Worker]** `Deliver` receives 429. Parses `Retry-After`.
2.  **[Logic]** Checks duration (3600s) vs SQS Limit (900s).
3.  **[Branch]** Duration > 900s. **Park** the delivery.
    *   **[DB]** `UPDATE notification_deliveries SET status='deferred', next_retry_at=NOW()+3600s WHERE id=$id`.
    *   **[SQS]** ACK message. **DO NOT** re-publish.
4.  **[Time Passes...]** (1 hour later).
5.  **[Archiver]** `RequeueDeferredNotifications` job runs.
6.  **[Archiver]** Queries `deferred` items where `next_retry_at <= NOW()`.
7.  **[Archiver]** Re-enqueues to SQS and updates status to `pending`.

**Data State Changes**:
*   **DB**: Status `pending` -> `deferred` -> `pending`.

**Gap Analysis**:
*   None. (Addressed by Refactoring: "Parking" logic definition).

---

## NOTIF-006: Email Bounce Handling (Concurrent)

**Primary Components**: `api` (Webhook Handler), `worker/email` (Bounce Processor)

**Trigger**: SES sends bounce notification via SNS.

**Preconditions**:
*   A WatchPoint exists with the bouncing email address.
*   User might be editing the WatchPoint simultaneously.

**Execution Sequence**:
1.  **[API]** Receives Webhook. Verifies Signature. Pushes event to `BounceProcessor` (async).
2.  **[Processor]** Identifies `WatchPointID` and `EmailAddress` from metadata.
3.  **[Processor]** Fetches WatchPoint (reads `config_version` = 5).
4.  **[Logic]** Modifies `channels` JSON in memory: Finds email entry, sets `enabled=false`, adds `disabled_reason="hard_bounce"`.
5.  **[DB]** Attempts **Optimistic Update**:
    ```sql
    UPDATE watchpoints
    SET channels = $new_json, updated_at = NOW()
    WHERE id = $id AND config_version = 5;
    ```
6.  **[Branch A - Success]**: RowsAffected = 1. Done.
7.  **[Branch B - Conflict]**: RowsAffected = 0 (User bumped version to 6).
    *   **[Processor]** Refetches WatchPoint (Version 6).
    *   **[Processor]** Checks if email channel still exists.
    *   **[Processor]** Retries update logic with Version 6.

**Data State Changes**:
*   **DB**: WatchPoint channel disabled safely.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Optimistic locking strategy).

---

## NOTIF-007: Platform-Specific Formatting (Override)

**Primary Components**: `worker/webhook`

**Trigger**: Webhook delivery for a URL routed through a proxy (e.g., `https://my-proxy.com/slack`).

**Preconditions**:
*   Channel Config has `platform_override: "slack"`.

**Execution Sequence**:
1.  **[Worker]** Calls `PlatformRegistry.Detect(url, config)`.
2.  **[Registry]** Checks `config["platform_override"]`. Finds "slack".
3.  **[Registry]** Returns `SlackFormatter` (bypassing regex check on the proxy URL).
4.  **[Worker]** Calls `SlackFormatter.Format(payload)`.
5.  **[Formatter]** Iterates Triggered Periods.
    *   Count = 25. Limit = 5 (Top N strategy).
    *   Adds 5 blocks.
    *   Adds Footer: "...and 20 more events. <Link|View Dashboard>".
6.  **[Worker]** Sends valid Slack Block Kit JSON to the proxy URL.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `platform_override` field).

---

## NOTIF-008: Escalation Notification

**Primary Components**: `worker/eval` (Python), `internal/db`

**Trigger**: `EVAL-001` (Evaluation Pipeline) detects worsening conditions for an already-triggered WatchPoint.

**Preconditions**:
*   `watchpoint_evaluation_state.previous_trigger_state` is `TRUE`.
*   `watchpoint_evaluation_state.trigger_value` is set (e.g., 55.0).
*   Current forecast value is significantly higher (e.g., 85.0).

**Execution Sequence**:
1.  **[Eval Worker]** Evaluates condition: `RainProb > 50`. Current value: `85`.
2.  **[Eval Worker]** Calculates **Overage**: `(85 - 50) / 50 = 0.7` (70%).
3.  **[Eval Worker]** Determines **New Level**: 70% >= 50% (Level 2 "Severe").
4.  **[Eval Worker]** Checks State:
    *   Current DB Level: `0` (Initial).
    *   `last_escalated_at`: `NULL`.
5.  **[Logic]** `NewLevel (2) > OldLevel (0)` AND `(Now - NULL) > 2h`. **Escalation Valid**.
6.  **[Eval Worker]** Enqueues Notification:
    *   Event Type: `escalated`.
    *   Payload: Includes `escalation_level: 2`.
7.  **[Eval Worker]** Commits State Transaction:
    *   **[DB]** Insert into `notifications`.
    *   **[DB]** Update `watchpoint_evaluation_state`:
        *   `escalation_level` = 2
        *   `last_escalated_at` = NOW()
        *   `event_sequence` = `event_sequence` + 1
        *   `trigger_value` = 85.0 (Update trigger benchmark to new high)

**Data State Changes**:
*   **DB**: `watchpoint_evaluation_state` updated with new escalation trackers. `notifications` row created.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `last_escalated_at` and `escalation_level` columns).

---

## NOTIF-009: Cleared Notification

**Primary Components**: `worker/eval` (Python), `internal/db`

**Trigger**: `EVAL-001` detects conditions no longer met (Hysteresis applied).

**Preconditions**:
*   `previous_trigger_state` is `TRUE`.
*   `trigger_value` is `85.0`.
*   `notify_on_clear` preference is `TRUE`.

**Execution Sequence**:
1.  **[Eval Worker]** Evaluates condition: `RainProb > 50`. Current value: `40`.
2.  **[Eval Worker]** Applies Hysteresis: `40 < (50 - 5)`. **Cleared**.
3.  **[Eval Worker]** Constructs Payload:
    *   Event Type: `threshold_cleared`.
    *   Injects Context: `PreviousValue = state.trigger_value` (85.0).
4.  **[Eval Worker]** Enqueues Notification (SQS).
5.  **[Eval Worker]** Commits State Transaction:
    *   **[DB]** Insert into `notifications`.
    *   **[DB]** Update `watchpoint_evaluation_state`:
        *   `previous_trigger_state` = `FALSE`
        *   `trigger_value` = `NULL` (Reset)
        *   `escalation_level` = 0 (Reset)
        *   `last_escalated_at` = `NULL` (Reset)
        *   `event_sequence` = `event_sequence` + 1

**Data State Changes**:
*   **DB**: State effectively reset to clean slate. Notification history preserved.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `trigger_value` logic and state reset).

---

## NOTIF-010: All Channels Failed Recovery (Self-Healing)

**Primary Components**: `worker/email` (or `worker/webhook`), `internal/notification/core`

**Trigger**: `NOTIF-001` delivery attempt results in a terminal error (e.g., HTTP 401, Blocked Email).

**Preconditions**:
*   A notification has 2 channels (e.g., Email, Webhook).
*   Email already failed (`status='failed'`).
*   Webhook Worker is processing the second channel.

**Execution Sequence**:
1.  **[Worker]** Attempts Webhook delivery. Receives `410 Gone`.
2.  **[Worker]** Determines `Retryable = false`.
3.  **[Worker]** Calls `DeliveryManager.MarkFailure(ctx, deliveryID, "410 Gone")`.
    *   **[DB]** `UPDATE notification_deliveries SET status='failed' ...`
4.  **[Worker]** Calls `DeliveryManager.CheckAggregateFailure(ctx, notifID)`.
    *   **[DB]** `SELECT COUNT(*) FROM notification_deliveries WHERE notification_id=$1 AND status != 'failed'`.
    *   Result: `0` (All failed).
5.  **[Logic]** **Compensatory Action Required**. If we do nothing, the Eval Engine thinks the user was notified (`last_notified_at` was set during creation), preventing retries.
6.  **[Worker]** Calls `DeliveryManager.ResetNotificationState(ctx, wpID)`.
    *   **[DB]** `UPDATE watchpoint_evaluation_state SET last_notified_at = NULL, last_notified_state = NULL WHERE watchpoint_id=$1`.
7.  **[Worker]** Logs `NotificationFailed` metric.

**Data State Changes**:
*   **DB**: `notification_deliveries` marked failed. `watchpoint_evaluation_state` rolled back to allow retry.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `CheckAggregateFailure` and `ResetNotificationState`).

---

## WPLC-001: WatchPoint Creation (Event Mode)

**Primary Components**: `api` (`WatchPointHandler`), `internal/forecasts`

**Trigger**: `POST /v1/watchpoints`

**Preconditions**:
*   User has valid API Key/Session.
*   Organization is under plan limits.

**Execution Sequence**:
1.  **[Handler]** Decodes JSON.
2.  **[Handler]** Calls `Validator.ValidateStruct`.
    *   **[Validation]** Iterates Conditions. Validates `Threshold` against `types.StandardVariables` range (e.g., TempC -60 to 60).
3.  **[Handler]** Calls `UsageEnforcer.CheckLimit`.
4.  **[Handler]** Calls `ForecastProvider.GetSnapshot(lat, lon)`.
    *   **[Service]** Queries **Medium-Range (Atlas)** Zarr data from S3.
    *   *Scenario A (Success)*: Returns struct with Temp/Precip.
    *   *Scenario B (Failure)*: S3 timeout. Logs warning. Returns `nil`.
5.  **[Handler]** Sets defaults (`Status='active'`, `TemplateSet='default'`).
6.  **[Handler]** Calls `Repo.Create`.
    *   **[DB]** Inserts into `watchpoints` and `watchpoint_evaluation_state` (init).
7.  **[Handler]** Returns `201 Created`.
    *   Payload includes `current_forecast` (data or null).

**Data State Changes**:
*   **DB**: New WatchPoint record.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `StandardVariables` hoist, Soft Forecast Dependency).

---

## WPLC-002: WatchPoint Creation (Monitor Mode)

**Primary Components**: `api` (`WatchPointHandler`)

**Trigger**: `POST /v1/watchpoints` (with `monitor_config`)

**Preconditions**:
*   Request contains `monitor_config` instead of `time_window`.

**Execution Sequence**:
1.  **[Handler]** Validates `MonitorConfig` (WindowHours 6-168).
2.  **[Handler]** Validates `Conditions` against `types.StandardVariables`.
3.  **[Handler]** Calls `ForecastProvider.GetSnapshot` (Atlas model) for immediate UI feedback.
4.  **[Handler]** Calls `Repo.Create`.
    *   **[DB]** Insert WatchPoint.
    *   **[DB]** Init EvaluationState (`last_evaluated_at = NULL`).
5.  **[Logic]** **No Evaluation Trigger**.
    *   Unlike Resume, Create does *not* fire an async evaluation event.
    *   The WatchPoint waits for the next `DataPoller` -> `Batcher` cycle.
    *   *Reasoning*: `EVAL-005` dictates the first eval suppresses notifications anyway. Waiting for the batcher is efficient.
6.  **[Handler]** Returns `201 Created`.

**Data State Changes**:
*   **DB**: New WatchPoint record.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit "No Eval on Create" policy).


## WPLC-003: WatchPoint Update (Partial)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`, `worker/eval`

**Trigger**: `PATCH /v1/watchpoints/{id}`

**Preconditions**:
*   WatchPoint exists and belongs to organization.
*   User has `watchpoints:write` scope.

**Execution Sequence**:
1.  **[Handler]** Decodes JSON request (e.g., changing `threshold`).
2.  **[Handler]** Validates new conditions/fields.
3.  **[Handler]** Calls `Repo.Update(ctx, wp)`.
4.  **[DB]** Updates `watchpoints` table.
    *   *Trigger*: `increment_config_version` fires. `config_version` increments (e.g., 5 -> 6).
5.  **[Handler]** Calls `EvaluationTrigger.TriggerEvaluation(ctx, wp.ID)`.
6.  **[EvaluationTrigger]** Orchestrates message creation:
    *   Fetches latest valid `ForecastRun` timestamp from DB.
    *   Constructs `EvalMessage`: `{ForecastType: "medium_range", ..., SpecificWatchPointIDs: [wp.ID]}`.
    *   **[SQS]** Sends to `eval-queue-standard`.
7.  **[Handler]** Returns `200 OK` with updated WatchPoint.

**Data State Changes**:
*   **DB**: `watchpoints` updated (`conditions`, `config_version`, `updated_at`).
*   **SQS**: Targeted message enqueued.

**Gap Analysis**:
*   None. (Aligns with Refactoring: Targeted Evaluation).

---

## WPLC-004: Update While Triggered (State Reset)

**Primary Components**: `api`, `worker/eval`, `internal/db`

**Trigger**: `PATCH /v1/watchpoints/{id}` on a triggered WatchPoint.

**Preconditions**:
*   WatchPoint is in triggered state (`previous_trigger_state` = TRUE).
*   `watchpoint_evaluation_state.config_version` = 5.

**Execution Sequence**:
1.  **[API]** Updates WatchPoint. `config_version` becomes 6. Enqueues Targeted Eval (as per WPLC-003).
2.  **[Eval Worker]** Receives `EvalMessage` with `SpecificWatchPointIDs: ["wp_123"]`.
3.  **[Eval Worker]** Calls `Repo.FetchBatch` with ID filter.
4.  **[Eval Worker]** Fetches WatchPoint (v6) and State (v5).
5.  **[Eval Worker]** Detects Version Mismatch (`6 != 5`).
6.  **[Eval Worker]** Executes **Fast Fail & Reset** logic:
    *   Modifies State in memory:
        *   `config_version` = 6
        *   `previous_trigger_state` = FALSE
        *   `trigger_value` = NULL
        *   `escalation_level` = 0
    *   **[DB]** `UPDATE watchpoint_evaluation_state SET ...` (Persists reset).
7.  **[Eval Worker]** Proceeds to Evaluation (Clean Slate).
    *   Evaluates conditions against forecast.
    *   *Scenario*: Conditions no longer met under new config.
8.  **[Eval Worker]** Result is "Not Triggered". No notification generated (Silent resolution).

**Data State Changes**:
*   **DB**: `evaluation_state` reset and synced to v6.

**Gap Analysis**:
*   None. (Demonstrates robust handling of race conditions via worker authority).

---

## WPLC-005: WatchPoint Pause

**Primary Components**: `api` (`WatchPointHandler`)

**Trigger**: `POST /v1/watchpoints/{id}/pause`

**Preconditions**:
*   Status is 'active'.

**Execution Sequence**:
1.  **[Handler]** Calls `Repo.UpdateStatus(ctx, id, "paused")`.
2.  **[DB]** `UPDATE watchpoints SET status = 'paused' WHERE id = ...`.
3.  **[Logic]** API does **NOT** clear evaluation state. `trigger_value` and history remain preserved.
4.  **[Handler]** Returns `200 OK`.

**Data State Changes**:
*   **DB**: `watchpoints.status` updated.

**Gap Analysis**:
*   None.

---

## WPLC-006: WatchPoint Resume (Targeted)

**Primary Components**: `api`, `worker/eval`

**Trigger**: `POST /v1/watchpoints/{id}/resume`

**Preconditions**:
*   Status is 'paused'.

**Execution Sequence**:
1.  **[Handler]** Calls `DeliveryManager.CancelDeferred(ctx, id)`.
    *   **[DB]** `UPDATE notification_deliveries SET status='skipped', failure_reason='resume_flush' WHERE watchpoint_id=$1 AND status='deferred'`.
    *   *Rationale*: Ensures user isn't bombarded with old alerts from the pause period.
2.  **[Handler]** Calls `Repo.UpdateStatus(ctx, id, "active")`.
3.  **[Handler]** Calls `EvaluationTrigger.TriggerEvaluation(ctx, id)`.
    *   **[SQS]** Sends `EvalMessage` with `SpecificWatchPointIDs: [id]`.
4.  **[Eval Worker]** Processes message (Targeted Mode).
5.  **[Eval Worker]** Fetches State (preserved from before pause).
6.  **[Eval Worker]** Evaluates against *current* forecast.
7.  **[Logic]**
    *   *Scenario*: Conditions still met. -> State remains triggered. No new notification (unless escalation).
    *   *Scenario*: Conditions cleared during pause. -> Triggers `threshold_cleared` notification immediately.

**Data State Changes**:
*   **DB**: `notification_deliveries` (deferred cleared), `watchpoints` (active).
*   **SQS**: Targeted eval message.

**Gap Analysis**:
*   None. (Implements Resume cleanup policy).

---

## WPLC-007: Resume with Quiet Hours Backlog

**Primary Components**: `api`, `internal/notification/core`

**Trigger**: `POST .../resume` on a WP that has pending deferred notifications.

**Preconditions**:
*   WatchPoint was paused.
*   `notification_deliveries` has rows with `status='deferred'` (parked during Quiet Hours).

**Execution Sequence**:
1.  **[Handler]** Calls `DeliveryManager.CancelDeferred(ctx, id)`.
2.  **[DB]** Finds deferred rows. Updates them to `skipped`.
3.  **[Handler]** Updates status to 'active'.
4.  **[Handler]** Triggers Targeted Evaluation.
5.  **[Eval Worker]** Evaluates fresh data.
6.  **[Logic]** If conditions currently met, generates a **single, fresh** notification.
    *   *Result*: User receives one up-to-date alert instead of a flood of stale deferred alerts followed by a new one.

**Data State Changes**:
*   **DB**: Stale deliveries skipped. New fresh notification created (if applicable).

**Gap Analysis**:
*   None. (Consolidated logic confirmed).


## WPLC-008: WatchPoint Delete (Soft Delete)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: `DELETE /v1/watchpoints/{id}`

**Preconditions**:
*   User is authenticated and owns the WatchPoint (or is Owner/Admin).

**Execution Sequence**:
1.  **[Handler]** Validates authentication and permissions.
2.  **[Handler]** Calls `Repo.Delete(ctx, id, orgID)`.
3.  **[DB]** Executes:
    ```sql
    UPDATE watchpoints
    SET deleted_at = NOW(), status = 'archived' 
    WHERE id = $1 AND organization_id = $2;
    ```
4.  **[DB]** Returns row count. If 0, Handler returns 404.
5.  **[Logic]** Does **not** modify `watchpoint_evaluation_state`. Soft deletion is sufficient to stop evaluation due to the Batcher's index filter (`deleted_at IS NULL`).
6.  **[Handler]** Emits `watchpoint.deleted` to Audit Log.
7.  **[Handler]** Returns `204 No Content`.

**Data State Changes**:
*   **DB**: `watchpoints.deleted_at` set. `watchpoints.status` set to `archived` (defensive state).

**Gap Analysis**:
*   None. (Relies on `MAINT-005` for eventual hard cleanup).

---

## WPLC-009: WatchPoint Automatic Archival

**Primary Components**: `cmd/archiver` (Multiplexer), `internal/scheduler` (`ArchiverService`)

**Trigger**: `EventBridge Rule: rate(15 minutes)` with payload `{"task": "archive_watchpoints"}`

**Preconditions**:
*   Event Mode WatchPoints exist where `time_window_end < NOW() - 1h`.

**Execution Sequence**:
1.  **[Archiver]** Receives payload. Acquires distributed lock `archive_watchpoints:{hour}`.
2.  **[Service]** Calls `Repo.ArchiveExpired(ctx, now, buffer)`.
3.  **[DB]** Executes:
    ```sql
    UPDATE watchpoints
    SET status = 'archived', 
        archived_at = NOW(), 
        archived_reason = 'event_passed'
    WHERE status = 'active' 
      AND time_window_end < ($now - INTERVAL '1 hour')
    RETURNING id, organization_id, name;
    ```
4.  **[Service]** Iterates returned rows (Archived WatchPoints).
5.  **[Service]** For each WP, constructs a `NotificationMessage`:
    *   `EventType`: `event_summary`
    *   `Payload`: Context for summary generation.
6.  **[Service]** Enqueues message directly to `notification-queue` (Triggers `OBS-007` Post-Event Summary).
7.  **[Service]** Emits `watchpoint.archived` audit event.

**Data State Changes**:
*   **DB**: `watchpoints` status transitions `active` -> `archived`.
*   **SQS**: Summary generation messages enqueued.

**Gap Analysis**:
*   None. (Uses existing Notification Worker infrastructure for summaries).

---

## WPLC-010: Clone WatchPoint (Time-Shifted)

**Primary Components**: `api` (`WatchPointHandler`)

**Trigger**: `POST /v1/watchpoints/{id}/clone`

**Preconditions**:
*   Source WatchPoint exists (can be `archived`).
*   User provides `time_shift` (e.g., `{ "years": 1 }`).

**Execution Sequence**:
1.  **[Handler]** Calls `Repo.GetByID(ctx, sourceID)`.
2.  **[Handler]** Validates source ownership.
3.  **[Logic - ID Generation]** Generates new UUID `wp_new...`.
4.  **[Logic - Time Shift]**:
    *   If `Event Mode`:
        *   Load location `Timezone` (e.g., "America/New_York").
        *   Convert `StartUTC` -> `StartLocal`.
        *   Add `TimeShift` (1 Year).
        *   Convert `NewStartLocal` -> `NewStartUTC` (Preserves 3 PM regardless of DST).
        *   Repeat for `End`.
    *   If `Monitor Mode`:
        *   Ignore `TimeShift`. Use `monitor_config` as-is.
5.  **[Handler]** Constructs new `WatchPoint` struct:
    *   Copies conditions, channels, preferences.
    *   Sets `Status = 'active'`.
    *   Sets `ID = wp_new...`.
6.  **[Handler]** Calls `Repo.Create(ctx, newWP)`.
7.  **[Handler]** Emits `watchpoint.created` (Source: clone).
8.  **[Handler]** Returns `201 Created` with new object.

**Data State Changes**:
*   **DB**: New `watchpoints` record inserted. `watchpoint_evaluation_state` not created yet (Lazy Init by Worker).

**Gap Analysis**:
*   None. (Timezone logic strictly defined).

---

## WPLC-011: Bulk WatchPoint Import

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: `POST /v1/watchpoints/bulk`

**Preconditions**:
*   Request body contains array of WatchPoint definitions (Max 100).

**Execution Sequence**:
1.  **[Handler]** Validates batch size <= 100.
2.  **[Handler]** Checks Plan Limits (`Current + BatchSize <= Limit`).
3.  **[Logic - Phase 1: Filter]**:
    *   Iterate input items.
    *   Run `Validator.ValidateStruct` on each.
    *   If valid: Assign new UUID `wp_...`, add to `valid_batch` slice.
    *   If invalid: Add error detail to `failures` slice.
4.  **[Logic - Phase 2: Insert]**:
    *   If `len(valid_batch) > 0`:
        *   Call `Repo.CreateBatch(ctx, valid_batch)`.
        *   **[DB]** Executes `INSERT INTO watchpoints VALUES (...), (...), ...` (Single atomic statement).
5.  **[Logic - Quiet Create]**:
    *   Does **not** fetch forecast snapshots (returns `current_forecast: null`).
    *   Does **not** trigger immediate evaluation (Wait for Batcher).
6.  **[Handler]** Constructs response combining `successes` (with IDs) and `failures`.
7.  **[Handler]** Returns `200 OK` (Partial success allowed).

**Data State Changes**:
*   **DB**: Multiple `watchpoints` rows inserted.

**Gap Analysis**:
*   None. (Optimized for throughput via pre-generated IDs and quiet create).

---

## WPLC-012: Validation Rejection

**Primary Components**: `api` (`Validator`), `05a-api-core`

**Trigger**: `POST /v1/watchpoints` with invalid data (e.g., Lat=95)

**Execution Sequence**:
1.  **[Handler]** Decodes JSON body.
2.  **[Handler]** Calls `Validator.ValidateStruct(req)`.
3.  **[Validator]** Checks struct tags:
    *   `lat` (95) fails `max=90` check.
    *   Returns `ValidationErrors` array.
4.  **[Handler]** Maps errors to standard `APIErrorResponse`.
    *   Code: `validation_invalid_latitude`.
    *   Message: "Latitude must be between -90 and 90".
5.  **[Handler]** Returns `400 Bad Request`.
6.  **[Logic]** No Audit Log entry created (prevents noise/DOS).

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None.


# Flow Simulations: User & Organization Management (USER)

> **Range**: USER-001 to USER-005
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## USER-001: Organization Creation (Signup)

**Primary Components**: `api` (`OrganizationHandler`), `internal/db`, `external/stripe`, `internal/auth`

**Trigger**: `POST /v1/auth/signup` (or equivalent public registration endpoint)

**Preconditions**:
*   Email is unique in system.
*   `PlanRegistry` is configured.

**Execution Sequence**:
1.  **[Handler]** Decodes request (Name, Email, Password).
2.  **[Handler]** Calls `PlanRegistry.GetLimits("free")` to fetch default limits.
3.  **[Handler]** Starts Database Transaction.
4.  **[DB]** `INSERT INTO organizations (id, name, billing_email, plan, plan_limits...)`.
5.  **[DB]** `INSERT INTO users (id, email, password_hash, role='owner'...)`.
6.  **[DB]** Commits Transaction.
7.  **[Handler]** Calls `BillingService.EnsureCustomer(ctx, orgID, email)`.
    *   **[Stripe]** `POST /v1/customers`.
    *   **[Logic]** If success: `UPDATE organizations SET stripe_customer_id = $id`.
    *   **[Logic]** If failure: Log `BILLING_SETUP_FAIL` warning. **Do not fail request**. (Lazy repair via BILL-002).
8.  **[Handler]** Calls `EmailService.SendWelcome(ctx, email)` (Fire-and-forget).
9.  **[Handler]** Returns `201 Created` with User and Org details.

**Data State Changes**:
*   **DB**: New `organizations` row, new `users` row.
*   **External**: New Customer in Stripe.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit transaction boundaries and best-effort Stripe logic).

---

## USER-002: Organization Update

**Primary Components**: `api` (`OrganizationHandler`), `internal/db`, `external/stripe`

**Trigger**: `PATCH /v1/organization`

**Preconditions**:
*   User is authenticated as Owner or Admin.

**Execution Sequence**:
1.  **[Handler]** Validates permissions (RBAC).
2.  **[Handler]** Decodes request (e.g., changing `billing_email`).
3.  **[Handler]** Calls `Repo.Update(ctx, org)`.
4.  **[DB]** `UPDATE organizations SET billing_email = $2 ...`.
5.  **[Handler]** Checks if `billing_email` was modified.
6.  **[Logic]** If modified: Trigger Async Stripe Sync.
    *   *Action*: Spawns goroutine or calls non-blocking function.
    *   **[BillingService]** Calls Stripe API to update Customer email.
    *   **[Error Handling]** If Stripe fails, log warning. Database remains authoritative.
7.  **[Handler]** Emits `organization.updated` to Audit Log.
8.  **[Handler]** Returns `200 OK`.

**Data State Changes**:
*   **DB**: `organizations` updated.
*   **External**: Stripe Customer updated (eventually/best-effort).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Best-effort policy defined).

---

## USER-003: Organization Deletion (Soft Delete)

**Primary Components**: `api` (`OrganizationHandler`), `internal/db`, `external/stripe`

**Trigger**: `DELETE /v1/organization`

**Preconditions**:
*   User is authenticated as Owner.

**Execution Sequence**:
1.  **[Handler]** Validates permissions (RoleOwner).
2.  **[Handler]** Calls `BillingService.CancelSubscription(ctx, orgID)`.
    *   **[Stripe]** `DELETE /v1/subscriptions/...` (at period end false).
    *   **[Logic]** If Stripe returns error (e.g., API outage): **Return 500 Internal Server Error**. Abort. (Prevents Zombie Billing).
3.  **[Handler]** Calls `WatchPointRepo.PauseAllByOrgID(ctx, orgID)`.
    *   **[DB]** `UPDATE watchpoints SET status = 'paused' WHERE organization_id = $1`.
    *   *Rationale*: Stops evaluation immediately to save costs/resources.
4.  **[Handler]** Calls `OrgRepo.Delete(ctx, id)` (Soft Delete).
    *   **[DB]** `UPDATE organizations SET deleted_at = NOW() WHERE id = $1`.
5.  **[Handler]** Emits `organization.deleted` to Audit Log.
6.  **[Handler]** Returns `204 No Content`.

**Data State Changes**:
*   **DB**: `organizations.deleted_at` set. All `watchpoints.status` set to `paused`.
*   **External**: Stripe subscription canceled immediately.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Fail-fast billing check and cascading pause).

---

## USER-004: User Invite

**Primary Components**: `api` (`UserHandler`), `internal/db`, `external/ses`

**Trigger**: `POST /v1/users/invite`

**Preconditions**:
*   User is Admin/Owner.

**Execution Sequence**:
1.  **[Handler]** Validates input (Email format, Role).
2.  **[Handler]** Generates secure invite token string.
3.  **[Handler]** Hashes token (bcrypt).
4.  **[Handler]** Calls `Repo.CreateInvited(ctx, user, tokenHash)`.
    *   **[DB]** `INSERT INTO users (..., status='invited')`.
    *   **[Error Handling]** If DB returns Unique Violation on email: Return **409 Conflict** ("User already exists").
5.  **[Handler]** Calls `EmailService.SendInvite(ctx, email, token)`.
    *   **[SES]** Sends email via AWS SES.
    *   **[Error Handling]** If SES fails: Return **500**. (Admin sees error, user not confused by missing email).
6.  **[Handler]** Emits `user.invited` to Audit Log.
7.  **[Handler]** Returns `201 Created`.

**Data State Changes**:
*   **DB**: New `users` row with `status='invited'`.

**Gap Analysis**:
*   None. (Addressed by Refactoring: 409 Conflict handling and synchronous email).

---

## USER-005: User Accepts Invite

**Primary Components**: `api` (`AuthHandler`), `internal/db`

**Trigger**: `POST /auth/accept-invite`

**Preconditions**:
*   Valid invite token provided.

**Execution Sequence**:
1.  **[Handler]** Calls `AuthService.AcceptInvite`.
2.  **[Service]** Starts Database Transaction.
3.  **[Service]** Validates token hash against DB.
4.  **[Service]** Updates User:
    *   **[DB]** `UPDATE users SET password_hash=$1, status='active', name=$2, invite_token_hash=NULL WHERE id=$3`.
5.  **[Service]** Creates Session:
    *   **[DB]** `INSERT INTO sessions ...`.
6.  **[Service]** Commits Transaction.
    *   *Rationale*: If session creation fails, user remains 'invited' and can try again. No zombie state.
7.  **[Handler]** Sets Session Cookie.
8.  **[Handler]** Returns `200 OK` with User Profile.

**Data State Changes**:
*   **DB**: `users` row updated to active. New `sessions` row created.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Atomicity requirement).


# Flow Simulations: User Authentication & Management (USER)

> **Range**: USER-006 to USER-010
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## USER-006: User Login (Password)

**Primary Components**: `api` (`AuthHandler`), `internal/auth` (`AuthService`, `SessionService`), `internal/db`

**Trigger**: `POST /auth/login`

**Preconditions**:
*   User exists and is active.
*   Client does not have a valid session.

**Execution Sequence**:
1.  **[Handler]** Decodes request body (`email`, `password`).
2.  **[Handler]** Normalizes email (`strings.ToLower`).
3.  **[Handler]** Checks `BruteForceProtector.CheckLoginAllowed(ctx, email, ip)`.
    *   **[DB]** `SELECT count(*) FROM login_attempts WHERE ...`.
    *   **[Logic]** If locked out, return 429.
4.  **[Handler]** Calls `AuthService.Login(ctx, email, password)`.
5.  **[Service]** Fetches user by email.
    *   **[DB]** `SELECT * FROM users WHERE email = $1`.
    *   **[Logic]** Verify password hash (bcrypt). If invalid, record failure and return 401.
6.  **[Service]** Starts DB Transaction.
7.  **[Service]** Updates `last_login_at`.
    *   **[DB]** `UPDATE users SET last_login_at = NOW() WHERE id = $1`.
8.  **[Service]** Calls `SessionService.CreateSession`.
    *   Generates `SessionID` (32 bytes hex) and `CSRFToken`.
    *   **[DB]** `INSERT INTO sessions (id, user_id, organization_id, csrf_token, ...) VALUES (...)`.
9.  **[Service]** Commits Transaction.
10. **[Handler]** Sets **HTTP-Only Cookie** `session_id`.
    *   Attributes: `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`.
11. **[Handler]** Returns `200 OK` JSON.
    *   Payload: `{ "user": {...}, "csrf_token": "...", "expires_at": "..." }` (No Session ID in body).

**Data State Changes**:
*   **DB**: New `sessions` row. `users.last_login_at` updated. `login_attempts` row inserted (success).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Session ID exclusion from JSON, synchronous timestamp update).

---

## USER-007: User Login (OAuth)

**Primary Components**: `api` (`AuthHandler`), `internal/auth`, `external/oauth`

**Trigger**: `GET /auth/oauth/{provider}/callback`

**Preconditions**:
*   User initiated flow via `/login`, `oauth_state` cookie exists.

**Execution Sequence**:
1.  **[Handler]** Validates `state` param matches `oauth_state` cookie.
2.  **[Handler]** Calls `OAuthProvider.Exchange(code)`.
    *   **[External]** Calls Provider Token Endpoint -> UserInfo Endpoint.
    *   Returns `OAuthProfile` (Email, Verified, ProviderID).
3.  **[Handler]** Calls `AuthService.LoginWithOAuth(ctx, profile)`.
4.  **[Service]** Fetches user by email.
    *   **[DB]** `SELECT * FROM users WHERE email = $1`.
5.  **[Logic - Provider Mismatch]**:
    *   If user exists AND `auth_provider` (e.g., 'github') != `profile.Provider` (e.g., 'google'):
    *   Return `409 Conflict`: "Account linked to GitHub". Abort.
6.  **[Service]** Starts DB Transaction.
7.  **[Logic - Implicit Acceptance]**:
    *   If user exists AND `status='invited'`:
    *   **[DB]** `UPDATE users SET status='active', invite_token_hash=NULL, auth_provider=$1, auth_provider_id=$2 WHERE id=$3`.
8.  **[Logic - New User]**:
    *   If user missing: **[DB]** `INSERT INTO users ...` (with provider fields).
9.  **[Service]** Calls `SessionService.CreateSession`.
    *   **[DB]** `INSERT INTO sessions ...`.
10. **[Service]** Commits Transaction.
11. **[Handler]** Sets Session Cookie.
12. **[Handler]** Redirects to Dashboard URL.

**Data State Changes**:
*   **DB**: `users` created or updated (invite cleared). `sessions` created.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Strict provider rejection, implicit invite acceptance transaction).

---

## USER-008: Password Reset

**Primary Components**: `api` (`AuthHandler`), `internal/auth`, `external/ses`

**Trigger**: `POST /auth/forgot-password` AND `POST /auth/reset-password`

**Phase 1: Request**
1.  **[Handler]** Checks Rate Limit `email_reset_limit:{email}`.
    *   **[Logic]** If > 3/hour: Return 429.
2.  **[Handler]** Calls `AuthService.RequestPasswordReset(email)`.
3.  **[Service]** Generates `RawToken` (high entropy). Computes `TokenHash`.
4.  **[Service]** Persists Token.
    *   **[DB]** `INSERT INTO password_resets (user_id, token_hash, expires_at) ...`
5.  **[Service]** Sends Email.
    *   **[External]** SES: Sends link with `RawToken`.

**Phase 2: Complete**
1.  **[Handler]** Validates inputs (New Password complexity).
2.  **[Handler]** Calls `AuthService.CompletePasswordReset(token, newPassword)`.
3.  **[Service]** Starts DB Transaction.
4.  **[Service]** Fetches `password_resets` by hash of `token`. Checks expiry.
5.  **[Service]** Updates User.
    *   **[DB]** `UPDATE users SET password_hash = $newHash WHERE id = $uid`.
6.  **[Service]** **Hard Invalidation**.
    *   **[DB]** `DELETE FROM sessions WHERE user_id = $uid`.
7.  **[Service]** Marks token used.
    *   **[DB]** `UPDATE password_resets SET used = true ...`.
8.  **[Service]** Commits Transaction.
9.  **[Handler]** Returns 200 OK.

**Data State Changes**:
*   **DB**: `password_resets` inserted/updated. `users` updated. `sessions` deleted (rows removed).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Email rate limiting, hard session delete).

---

## USER-009: User Role Change

**Primary Components**: `api` (`UserHandler`), `internal/db`

**Trigger**: `PATCH /v1/users/{id}` (Body: `{"role": "admin"}`)

**Preconditions**:
*   Actor is Owner (updating anyone) or Admin (updating Member).

**Execution Sequence**:
1.  **[Middleware]** `AuthMiddleware` loads Actor.
    *   **[DB]** `SELECT u.role, u.organization_id FROM sessions s JOIN users u ON s.user_id = u.id WHERE s.id = $cookie`.
    *   Ensures live role check.
2.  **[Handler]** Validates RBAC (Admin cannot promote to Owner, etc.).
3.  **[Handler]** Calls `UserRepo.UpdateRole(ctx, targetID, newRole)`.
4.  **[DB]** Starts Transaction.
5.  **[DB]** Locks Owner Rows (Prevent Race).
    *   `SELECT 1 FROM users WHERE organization_id = $oid AND role = 'owner' FOR UPDATE`.
6.  **[DB]** Counts Owners.
7.  **[Logic]** If `TargetRole == 'owner'` AND `NewRole != 'owner'` AND `OwnerCount == 1`:
    *   Abort Transaction. Return `400 Bad Request` ("Cannot demote last owner").
8.  **[DB]** Updates Role.
    *   `UPDATE users SET role = $newRole WHERE id = $targetID`.
9.  **[DB]** Commits Transaction.
10. **[Handler]** Returns 200 OK.

**Data State Changes**:
*   **DB**: `users.role` updated.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `FOR UPDATE` locking strategy).

---

## USER-010: User Removal

**Primary Components**: `api` (`UserHandler`), `internal/db`

**Trigger**: `DELETE /v1/users/{id}`

**Preconditions**:
*   Actor has permission to delete target.

**Execution Sequence**:
1.  **[Handler]** Validates RBAC.
2.  **[Handler]** Calls `UserRepo.Delete(ctx, targetID)`.
3.  **[DB]** Starts Transaction.
4.  **[DB]** Locks/Counts Owners (Same logic as USER-009).
    *   If deleting last owner, Abort.
5.  **[DB]** Deletes User.
    *   `DELETE FROM users WHERE id = $targetID`.
    *   **[Cascade]** `sessions` rows deleted automatically.
    *   **[Set Null]** `api_keys.created_by_user_id` set to NULL automatically.
6.  **[DB]** Commits Transaction.
    *   *Note*: `audit_log` rows remain untouched (no FK).
7.  **[Handler]** Emits `user.removed` to Audit Log.
8.  **[Handler]** Returns 204 No Content.

**Data State Changes**:
*   **DB**: `users` row deleted. `sessions` rows deleted. `api_keys` rows updated (FK null). `audit_log` inserted.

**Gap Analysis**:
*   None. (Addressed by Refactoring: API Key persistence via SET NULL, audit log immutability).


# Flow Simulations: User Management & Billing (USER/BILL)

> **Range**: USER-011 to BILL-001
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## USER-011: API Key Creation

**Primary Components**: `api` (`APIKeyHandler`), `internal/db`

**Trigger**: `POST /v1/api-keys`

**Preconditions**:
*   User is authenticated as Admin or Owner.
*   Requested scopes are valid.

**Execution Sequence**:
1.  **[Handler]** Decodes request (`Name`, `Scopes`, `ExpiresInDays`).
2.  **[Handler]** Validates `Scopes` against `types.AllScopes` (e.g., `watchpoints:read`).
3.  **[Handler]** Generates Secure Secret:
    *   Prefix: `sk_live_` (or `sk_test_`).
    *   Random: 32 bytes cryptographically secure random string.
    *   Result: `plaintext_secret`.
4.  **[Handler]** Computes Hash: `bcrypt.GenerateFromPassword(plaintext_secret, 12)`.
5.  **[Handler]** Calls `Repo.Create(ctx, &types.APIKey{KeyHash: hash, ...})`.
    *   *Critical*: The plaintext secret is **NOT** passed to the repository.
6.  **[DB]** `INSERT INTO api_keys (id, organization_id, key_hash, key_prefix, scopes, created_at) VALUES (...)`.
7.  **[Handler]** Emits `apikey.created` to Audit Log.
8.  **[Handler]** Constructs Response.
    *   Injects `plaintext_secret` into `APIKeySecretResponse` struct.
9.  **[Handler]** Returns `201 Created` with the secret (displayed once).

**Data State Changes**:
*   **DB**: New `api_keys` row inserted with hashed secret.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit secret generation responsibility in Handler).

---

## USER-012: API Key Rotation

**Primary Components**: `api` (`APIKeyHandler`), `internal/db`

**Trigger**: `POST /v1/api-keys/{id}/rotate`

**Preconditions**:
*   Target Key ID exists and is active (`revoked_at` is NULL).

**Execution Sequence**:
1.  **[Handler]** Calls `Repo.GetByID` to verify ownership and state.
2.  **[Handler]** Generates NEW Secret and Hash (same logic as USER-011).
3.  **[Handler]** Calls `Repo.Rotate(ctx, id, newHash, graceEnd)`.
4.  **[DB]** Starts Transaction.
5.  **[DB]** Updates Old Key:
    *   `UPDATE api_keys SET expires_at = NOW() + INTERVAL '24 hours' WHERE id = $id`.
6.  **[DB]** Inserts New Key:
    *   `INSERT INTO api_keys ...` (New ID, New Hash, linked metadata).
7.  **[DB]** Commits Transaction.
8.  **[Handler]** Emits `apikey.rotated` to Audit Log.
9.  **[Handler]** Returns `200 OK` with `new_key` (plaintext) and `previous_key_valid_until`.

**Data State Changes**:
*   **DB**: Old key expiry updated. New key inserted.

**Gap Analysis**:
*   None.

---

## USER-013: API Key Revocation

**Primary Components**: `api` (`APIKeyHandler`), `internal/db`

**Trigger**: `DELETE /v1/api-keys/{id}`

**Preconditions**:
*   User is authenticated.

**Execution Sequence**:
1.  **[Handler]** Calls `Repo.Delete(ctx, id, orgID)`.
2.  **[DB]** `UPDATE api_keys SET revoked_at = NOW() WHERE id = $1 AND organization_id = $2`.
3.  **[Handler]** Emits `apikey.revoked` to Audit Log.
4.  **[Handler]** Returns `204 No Content`.

**Verification Logic (Middleware)**:
*   Subsequent requests with this key fail `Authenticator.ResolveToken` check: `WHERE key_hash = $1 AND revoked_at IS NULL`. Immediate effect.

**Data State Changes**:
*   **DB**: `api_keys.revoked_at` set.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Strict revocation precedence logic).

---

## USER-014: Notify Owner on Email Bounces

**Primary Components**: `worker/email` (`BounceProcessor`), `internal/db`, `external/ses`

**Trigger**: `NOTIF-006` (Bounce Handling) logic detects failure count >= 3.

**Preconditions**:
*   Channel disabled due to hard bounces.

**Execution Sequence**:
1.  **[Processor]** Determines channel is dead. Calls `ChannelHealthRepo.DisableChannel(...)`.
    *   **[DB]** Updates `watchpoints` JSONB channel config.
2.  **[Processor]** Needs to alert owner. Calls `UserRepo.GetOwnerEmail(ctx, orgID)`.
    *   **[DB]** `SELECT email FROM users WHERE organization_id = $1 AND role = 'owner' LIMIT 1`.
3.  **[Processor]** Calls `TemplateService.Resolve("system", EventSystemAlert)`.
4.  **[Processor]** Calls `EmailProvider.Send(ctx, input)`.
    *   `To`: Owner Email.
    *   `Template`: Bounce Warning.
    *   `Variables`: WatchPoint Name, Channel Address.
5.  **[SES]** Sends alert email.

**Data State Changes**:
*   **DB**: WatchPoint updated (channel disabled).
*   **External**: Alert email sent.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `GetOwnerEmail` repository method and `EventSystemAlert` type).

---

## BILL-001: Stripe Customer Creation

**Primary Components**: `api` (`OrganizationHandler`), `external/stripe` (`BillingService`), `internal/db`

**Trigger**: Organization Creation (USER-001) completes DB commit.

**Preconditions**:
*   Organization ID and Billing Email available.

**Execution Sequence**:
1.  **[Handler]** Calls `BillingService.EnsureCustomer(ctx, orgID, email)`.
2.  **[Service]** Queries Stripe Search API:
    *   `query: "metadata['org_id']:'" + orgID + "'"`.
3.  **[Logic]**
    *   *Case A (Found)*: Returns existing `cus_...` ID.
    *   *Case B (Not Found)*: Calls Stripe Create Customer API.
        *   `email`: billing_email
        *   `metadata`: `{org_id: ...}`
        *   Returns new `cus_...` ID.
4.  **[Service]** Updates Local DB.
    *   **[DB]** `UPDATE organizations SET stripe_customer_id = $1 WHERE id = $2`.
5.  **[Logic]** If Stripe API fails (network/500):
    *   Log `BILLING_SETUP_FAIL` warning.
    *   Return `nil` error to Handler (Best Effort).
    *   *Recovery*: `StripeSyncer` (SCHED-003) will repair via "Headless Repair" logic later.

**Data State Changes**:
*   **External**: Stripe Customer created.
*   **DB**: `organizations.stripe_customer_id` populated.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Search-before-create logic and scheduled repair).


# Flow Simulations: Billing & Subscription Lifecycle (BILL)

> **Range**: BILL-002 to BILL-006
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## BILL-002: Subscription Creation (Upgrade)

**Primary Components**: `api` (`BillingHandler`), `external/stripe`, `internal/db`

**Trigger**: `POST /v1/billing/checkout-session` (User Action)

**Preconditions**:
*   User is authenticated as Admin/Owner.
*   Organization currently on 'free' or lower tier than requested.

**Execution Sequence**:
1.  **[Handler]** Validates requested plan tier.
2.  **[Handler]** Calls `BillingService.EnsureCustomer(ctx, orgID, email)`.
    *   **[Service]** Queries Stripe Search API for `metadata['org_id']`.
    *   **[Logic]** If found, uses existing ID. If not, creates new Customer.
3.  **[Handler]** Calls `BillingService.CreateCheckoutSession`.
    *   **[Stripe]** Creates session with `client_reference_id=orgID` and `metadata`.
4.  **[Handler]** Returns Checkout URL to client.
5.  **[Client]** Redirects user to Stripe. Payment completes.
6.  **[Stripe]** Sends webhook `checkout.session.completed`.
7.  **[WebhookHandler]** Verifies signature.
8.  **[WebhookHandler]** Extracts `orgID` from client reference.
9.  **[WebhookHandler]** Calls `SubscriptionStateRepo.UpdateSubscriptionStatus`.
    *   **[DB]** `UPDATE organizations SET plan=$new_plan, subscription_status='active', last_subscription_event_at=$ts WHERE id=$id AND (last_subscription_event_at < $ts OR IS NULL)`.

**Data State Changes**:
*   **DB**: `organizations.plan` updated to new tier. `stripe_customer_id` confirmed.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Search-before-create logic).

---

## BILL-003: Plan Upgrade (Mid-Cycle)

**Primary Components**: `api` (`StripeWebhookHandler`), `internal/db`

**Trigger**: Stripe Webhook `customer.subscription.updated` (Plan changed via Portal or API)

**Preconditions**:
*   Organization exists.
*   Webhook signature is valid.

**Execution Sequence**:
1.  **[Handler]** Decodes webhook payload. Identifies `previous_attributes` contains plan change.
2.  **[Handler]** Maps new Stripe Price ID to `types.PlanTier` (e.g., "price_pro" -> `PlanPro`).
3.  **[Handler]** Calls `PlanRegistry.GetLimits(PlanPro)`.
4.  **[Handler]** Calls `SubscriptionStateRepo.UpdateSubscriptionStatus`.
    *   **[DB]** `UPDATE organizations SET plan='pro', plan_limits=$limits, updated_at=NOW() ...`.
    *   *Optimistic Lock*: Checks `last_subscription_event_at` to ensure out-of-order delivery doesn't revert plan.
5.  **[Logic]** New limits take effect immediately for subsequent API calls.

**Data State Changes**:
*   **DB**: `organizations.plan` and `plan_limits` updated.

**Gap Analysis**:
*   None.

---

## BILL-004: Plan Downgrade

**Primary Components**: `api` (`StripeWebhookHandler`), `internal/db`

**Trigger**: Stripe Webhook `customer.subscription.updated` (Downgrade)

**Preconditions**:
*   User lowered plan in Stripe Portal.

**Execution Sequence**:
1.  **[Handler]** Decodes payload. Maps to `PlanStarter`.
2.  **[Handler]** Calls `SubscriptionStateRepo.UpdateSubscriptionStatus`.
    *   **[DB]** Updates `plan` and `plan_limits` immediately.
3.  **[Logic]** Usage Check is **NOT** performed here. The organization may now be over limits.
4.  **[Future]** The `SCHED-002` (Usage Aggregation) job runs daily.
    *   It detects `active_watchpoints > new_limit`.
    *   It updates `overage_started_at = NOW()` if currently NULL.
    *   It triggers `SubscriptionEnforcer.EnforceOverage` if `overage_started_at` > 14 days old (see BILL-011 logic).

**Data State Changes**:
*   **DB**: `organizations.plan` updated. Organization potentially enters non-compliant state (soft limit enforcement).

**Gap Analysis**:
*   None. (Relies on Refactoring: `overage_started_at` column).

---

## BILL-005: Payment Failure

**Primary Components**: `api` (`StripeWebhookHandler`), `internal/db`

**Trigger**: Stripe Webhook `invoice.payment_failed`

**Preconditions**:
*   Recurring payment declined.

**Execution Sequence**:
1.  **[Handler]** Decodes payload. Extracts `orgID`.
2.  **[Handler]** Calls `SubscriptionStateRepo.UpdatePaymentFailure(ctx, orgID, eventTimestamp)`.
    *   **[DB]** `UPDATE organizations SET payment_failed_at = COALESCE(payment_failed_at, $ts) ...`.
    *   *Logic*: Only sets timestamp if currently NULL, preserving the start of the grace period.
3.  **[Handler]** Creates System Notification.
    *   **[DB]** `INSERT INTO notifications (event_type='billing_alert', urgency='critical', ...)`.
    *   **[SQS]** Enqueues message for `EmailWorker`.
4.  **[EmailWorker]** Picks up message.
    *   Sends "Payment Failed - Action Required" email to `billing_email`.
    *   Bypasses `notification_preferences`.

**Data State Changes**:
*   **DB**: `organizations.payment_failed_at` set. `notifications` row inserted.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit notification insert).

---

## BILL-006: Grace Period Expiration (Payment)

**Primary Components**: `cmd/archiver` (Multiplexer), `internal/billing` (`SubscriptionEnforcer`)

**Trigger**: `EventBridge Rule: cron(0 0 * * ? *)` (Daily)

**Preconditions**:
*   Organization has `payment_failed_at` older than 7 days.

**Execution Sequence**:
1.  **[Archiver]** Receives payload `{"task": "sync_stripe"}`. (This job orchestrates enforcement).
2.  **[Archiver]** Calls `SubscriptionEnforcer.EnforcePaymentFailure(ctx, now)`.
3.  **[Enforcer]** Queries delinquent organizations:
    *   **[DB]** `SELECT id FROM organizations WHERE payment_failed_at < $now - INTERVAL '7 days'`.
4.  **[Enforcer]** Iterates results. For each Org:
    *   **[Enforcer]** Calls `WatchPointRepo.PauseAllByOrgID(ctx, orgID, "billing_delinquency")`.
    *   **[DB]** `UPDATE watchpoints SET status='paused', paused_reason='billing_delinquency' WHERE organization_id=$1 AND status='active'`.
5.  **[Enforcer]** Logs metrics `BillingEnforcementPaused`.

**Data State Changes**:
*   **DB**: WatchPoints paused with specific reason code.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `paused_reason` and `SubscriptionEnforcer`).


# Flow Simulations: Billing Integration & Enforcement (BILL)

> **Range**: BILL-007 to BILL-011
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## BILL-007: Stripe Webhook Handler (Router & Auto-Resume)

**Primary Components**: `api` (`StripeWebhookHandler`), `internal/db`, `external/stripe`

**Trigger**: `POST /v1/webhooks/stripe` (Stripe Event)

**Preconditions**:
*   Payload signature matches `STRIPE_WEBHOOK_SECRET`.

**Execution Sequence**:
1.  **[Handler]** Reads body and `Stripe-Signature` header.
2.  **[Handler]** Calls `StripeVerifier.Verify`.
3.  **[Handler]** Parses event JSON to `stripe.Event`.
4.  **[Handler]** Extracts `client_reference_id` (OrgID) from event object.
5.  **[Logic]** Switch on `event.type`:
    *   `checkout.session.completed`: -> **BILL-002**.
    *   `customer.subscription.updated`: -> **BILL-003/004**.
    *   `invoice.payment_failed`: -> **BILL-005**.
    *   `invoice.paid`: **[Execution Path]**
        a.  **[Handler]** Calls `WatchPointRepo.ResumeAllByOrgID(ctx, orgID, "billing_delinquency")`.
        b.  **[DB]** Executes:
            ```sql
            UPDATE watchpoints
            SET status = 'active', paused_reason = NULL
            WHERE organization_id = $1
              AND status = 'paused'
              AND paused_reason = 'billing_delinquency';
            ```
        c.  **[Handler]** Logs `AutoResume` metric.

**Data State Changes**:
*   **DB**: WatchPoints paused for non-payment are reactivated. User-paused items remain paused.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Auto-resume logic).

---

## BILL-008: WatchPoint Limit Check (Strict Consistency)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: `POST /v1/watchpoints` (Embedded in WPLC-001)

**Preconditions**:
*   Organization Plan Limits loaded in context.

**Execution Sequence**:
1.  **[Handler]** Starts limit check.
2.  **[Handler]** Calls `UsageEnforcer.CheckLimit(orgID, ResourceWatchPoints, limit)`.
3.  **[Enforcer]** Executes **Direct Count Query**:
    *   **[DB]** `SELECT COUNT(*) FROM watchpoints WHERE organization_id = $1 AND status != 'archived'`.
    *   *Rationale*: Avoids drift associated with cached counters in `rate_limits` table.
4.  **[Enforcer]** Compares `count` vs `limit`.
5.  **[Logic]**
    *   If `count < limit`: Return `nil` (Allowed).
    *   If `count >= limit`: Return `ErrLimitWatchpoints`.
6.  **[Handler]** If error, returns `403 Forbidden` with upgrade link.

**Data State Changes**:
*   None (Read-only check).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Direct COUNT(*) strategy).

---

## BILL-009: API Rate Limit Check (Direct DB)

**Primary Components**: `api` (`RateLimitMiddleware`), `internal/db`

**Trigger**: Incoming HTTP Request to protected endpoint

**Preconditions**:
*   Request authenticated (OrgID available).

**Execution Sequence**:
1.  **[Middleware]** Calls `RateLimitStore.IncrementAndCheck(ctx, orgID)`.
2.  **[Store]** Executes Atomic Update:
    ```sql
    UPDATE rate_limits
    SET api_calls_count = api_calls_count + 1
    WHERE organization_id = $1
    RETURNING api_calls_count, period_end, warning_sent_at;
    ```
3.  **[Logic]** Check Reset:
    *   If `period_end < NOW()`: Trigger **SCHED-001** (Snapshot & Reset) inline or fail open (design choice: fail open, rely on daily job for clean reset, but update period_end here if possible. *Correction*: Design uses daily job for hard reset, but middleware can lazy-reset. For v1 simpler path: Middleware assumes valid period).
4.  **[Logic]** Check Limits:
    *   Limit = `Org.PlanLimits.APICalls`.
    *   **Threshold 80%**: If `count > (limit * 0.8)` AND `warning_sent_at IS NULL`: Trigger **BILL-010**.
    *   **Threshold 100%**: If `count > limit`: Return `RateLimitResult{Allowed: false}`.
5.  **[Middleware]**
    *   If `!Allowed`: Return `429 Too Many Requests`.
    *   If `Allowed`: Proceed to Handler.

**Data State Changes**:
*   **DB**: `rate_limits.api_calls_count` incremented.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `warning_sent_at` column).

---

## BILL-010: Overage Warning (Event Generation)

**Primary Components**: `api` (`RateLimitMiddleware`), `internal/db`, `worker/email`

**Trigger**: Usage > 80% detected in BILL-009

**Preconditions**:
*   `warning_sent_at` is NULL.

**Execution Sequence**:
1.  **[Middleware]** Detects condition.
2.  **[Middleware]** Updates warning state atomically:
    *   **[DB]** `UPDATE rate_limits SET warning_sent_at = NOW() WHERE organization_id = $1`.
3.  **[Middleware]** Inserts System Notification:
    *   **[DB]**
        ```sql
        INSERT INTO notifications (id, organization_id, event_type, urgency, payload)
        VALUES (..., 'billing_warning', 'warning', '{"limit_type": "api_calls", "usage_percent": 80}')
        ```
4.  **[Middleware]** Enqueues SQS message to `notification-queue`.
5.  **[EmailWorker]** Picks up message.
6.  **[EmailWorker]** Renders template "Usage Warning".
7.  **[EmailWorker]** Sends email to Org Owner.

**Data State Changes**:
*   **DB**: `rate_limits` flag set. `notifications` row created.

**Gap Analysis**:
*   None. (Addressed by Refactoring: EventType definition).

---

## BILL-011: Downgrade Enforcement (Grace Period Expiry)

**Primary Components**: `cmd/archiver`, `internal/billing` (`SubscriptionEnforcer`), `internal/db`

**Trigger**: `EventBridge Rule: cron(0 0 * * ? *)` (Daily) via `sync_stripe task

**Preconditions**:
*   Organization has `overage_started_at` older than 14 days.

**Execution Sequence**:
1.  **[Archiver]** Calls `SubscriptionEnforcer.EnforceOverage(ctx, now)`.
2.  **[Enforcer]** Queries target organizations:
    *   **[DB]** `SELECT id, plan_limits FROM organizations WHERE overage_started_at < $now - INTERVAL '14 days'`.
3.  **[Enforcer]** For each Org:
    *   Get current `active_count` (SELECT COUNT).
    *   Calculate `excess = active_count - limit`.
    *   If `excess > 0`:
        *   **[Enforcer]** Calls `WatchPointRepo.PauseExcessByOrgID(ctx, orgID, limit)`.
        *   **[DB]** Executes LIFO Pause:
            ```sql
            UPDATE watchpoints
            SET status = 'paused', paused_reason = 'billing_delinquency'
            WHERE id IN (
                SELECT id FROM watchpoints
                WHERE organization_id = $1 AND status = 'active'
                ORDER BY created_at DESC
                LIMIT $excess
            );
            ```
        *   **[Enforcer]** Triggers "Enforcement Active" notification (similar to BILL-010).

**Data State Changes**:
*   **DB**: Newest excess WatchPoints paused.

**Gap Analysis**:
*   None. (Addressed by Refactoring: LIFO pause strategy and Repo method).


# Flow Simulations: Billing & Observability (BILL/OBS)

> **Range**: BILL-012 to OBS-003
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## BILL-012: Usage Reporting & History

**Primary Components**: `api` (`UsageHandler`), `internal/billing` (`UsageReporter`), `internal/db`

**Trigger**: `GET /v1/usage` OR `GET /v1/usage/history`

**Preconditions**:
*   User is authenticated (OrgID available).

**Execution Sequence (Current Usage)**:
1.  **[Handler]** Calls `UsageReporter.GetCurrentUsage(ctx, orgID)`.
2.  **[Reporter]** Fetches Plan Limits.
    *   **[DB]** `SELECT plan, plan_limits FROM organizations WHERE id=$1`.
3.  **[Reporter]** Fetches WatchPoint Usage.
    *   **[DB]** `SELECT COUNT(*) FROM watchpoints WHERE organization_id=$1 AND status != 'archived'`.
4.  **[Reporter]** Fetches API Usage.
    *   **[DB]** `SELECT api_calls_count FROM rate_limits WHERE organization_id=$1`.
5.  **[Reporter]** Assembles `UsageSnapshot` struct with calculated percentages.
6.  **[Handler]** Returns JSON response.

**Execution Sequence (History)**:
1.  **[Handler]** Calls `UsageReporter.GetUsageHistory(ctx, orgID, start, end)`.
2.  **[Reporter]** Executes **Union Query** (Data Continuity Logic):
    ```sql
    -- Historical Data (Closed Days)
    SELECT date, api_calls, watchpoints, notifications 
    FROM usage_history 
    WHERE organization_id = $1 AND date BETWEEN $2 AND $3
    
    UNION ALL
    
    -- Current Day (Partial)
    SELECT CURRENT_DATE, r.api_calls_count, 
           (SELECT COUNT(*) FROM watchpoints WHERE organization_id = $1 AND status != 'archived'),
           (SELECT COUNT(*) FROM notifications WHERE organization_id = $1 AND created_at >= CURRENT_DATE)
    FROM rate_limits r
    WHERE r.organization_id = $1
    ```
3.  **[Reporter]** Sorts and formats time series.
4.  **[Handler]** Returns JSON response.

**Data State Changes**:
*   None (Read-only).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit Union logic for continuity).

---

## BILL-013: Invoice Receipt Delivery

**Primary Components**: `api` (`StripeWebhookHandler`), `worker/email`, `internal/db`

**Trigger**: Stripe Webhook `invoice.paid`

**Preconditions**:
*   Invoice successfully paid in Stripe.

**Execution Sequence**:
1.  **[Handler]** Verifies signature and parses payload (Invoice object).
2.  **[Handler]** Extracts `client_reference_id` (OrgID).
3.  **[Handler]** (Existing logic) Calls `ResumeAllByOrgID` (BILL-007).
4.  **[Handler]** **Receipt Logic**:
    *   Constructs payload: `{ "amount": 9900, "currency": "usd", "pdf_url": "...", "period": "..." }`.
    *   **[DB]** Inserts Notification:
        ```sql
        INSERT INTO notifications (
            id, organization_id, event_type, urgency, payload
        ) VALUES (
            'notif_...', $orgID, 'billing_receipt', 'routine', $json
        );
        ```
5.  **[Handler]** Returns 200 OK to Stripe.
6.  **[EmailWorker]** SQS triggers worker.
7.  **[Worker]** Resolves Template.
    *   `EventType` is `billing_receipt`. Maps to `receipt_default` template set.
8.  **[Worker]** Delivers email to `billing_email`.

**Data State Changes**:
*   **DB**: Notification inserted. Delivery tracked.
*   **External**: Email sent.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `EventBillingReceipt` enum).

---

## OBS-001: Dead Man's Switch (Medium-Range)

**Primary Components**: `CloudWatch`, `SNS`, `PagerDuty`

**Trigger**: CloudWatch Alarm State Change

**Preconditions**:
*   `ForecastReady` metric missing for > 8 hours.

**Execution Sequence**:
1.  **[CloudWatch]** Evaluates Alarm `MediumRangeForecastStale`.
    *   Metric: `WatchPoint/ForecastReady`.
    *   Dimension: `ForecastType=medium_range`.
    *   Logic: `Sum < 1` over `28800s` (8 hours).
    *   Missing Data Treatment: `Breaching` (Vital for dead man's switch).
2.  **[CloudWatch]** State transitions `OK` -> `ALARM`.
3.  **[CloudWatch]** Publishes message to `AlertSNSTopic`.
4.  **[SNS]** Delivers to subscribers (Email, PagerDuty integration).
5.  **[Ops]** PagerDuty triggers incident: "CRITICAL: No GFS Ingest in 8 hours".

**Data State Changes**:
*   **External**: PagerDuty incident created.

**Gap Analysis**:
*   None. (Validated against `04-sam-template.md`).

---

## OBS-002: Dead Man's Switch (Nowcast)

**Primary Components**: `CloudWatch`, `SNS`

**Trigger**: CloudWatch Alarm State Change

**Preconditions**:
*   `ForecastReady` metric missing for > 30 minutes.

**Execution Sequence**:
1.  **[CloudWatch]** Evaluates Alarm `NowcastForecastStale`.
    *   Dimension: `ForecastType=nowcast`.
    *   Period: `1800s` (30 mins).
2.  **[CloudWatch]** State transitions `OK` -> `ALARM`.
3.  **[CloudWatch]** Publishes to `AlertSNSTopic`.

**Data State Changes**:
*   **External**: Alert sent.

**Gap Analysis**:
*   None. (Validated against `04-sam-template.md`).

---

## OBS-003: Queue Depth Monitoring (All Critical Queues)

**Primary Components**: `CloudWatch`, `SNS`

**Trigger**: SQS Queue Depth > Threshold

**Preconditions**:
*   Workers failing or load spike.

**Execution Sequence**:
1.  **[CloudWatch]** Evaluates Alarms (Parallel):
    *   **Eval Standard**: `ApproximateNumberOfMessagesVisible > 1000` (5 min).
    *   **Eval Urgent**: `ApproximateNumberOfMessagesVisible > 100` (1 min). (Added by Refactoring)
    *   **Notifications**: `ApproximateNumberOfMessagesVisible > 500` (5 min). (Added by Refactoring)
2.  **[CloudWatch]** State transitions `ALARM`.
3.  **[CloudWatch]** Publishes to `AlertSNSTopic`.
4.  **[Ops]** Investigates Lambda concurrency/errors.

**Data State Changes**:
*   **External**: Alert sent.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Added alarms for Urgent and Notification queues).


# Flow Simulations: Observability & Maintenance (OBS)

> **Range**: OBS-004 to OBS-008
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## OBS-004: Dead Letter Queue Processing (Procedural)

**Primary Components**: `CloudWatch`, `PagerDuty`, `AWS Console`

**Trigger**: CloudWatch Alarm `DLQMessagesAlarm` (Queue Depth > 0)

**Preconditions**:
*   Messages exist in `eval-queue-dlq` or `notification-queue-dlq`.

**Execution Sequence**:
1.  **[CloudWatch]** Alarm state transitions to `ALARM`. Sends SNS.
2.  **[Ops]** Receives PagerDuty alert.
3.  **[Ops]** Logs into AWS Console -> SQS.
4.  **[Ops]** Inspects messages in DLQ.
    *   *Check*: Is it a poison pill (malformed JSON)?
    *   *Check*: Is it a timeout/OOM artifact?
5.  **[Ops]** Action Path:
    *   *Scenario A (Valid but Failed)*: Uses "Start DLQ Redrive" to move messages back to source queue.
    *   *Scenario B (Poison Pill)*: Purges queue or manually deletes specific messages.
6.  **[CloudWatch]** Alarm returns to `OK` once depth is 0.

**Data State Changes**:
*   **SQS**: Messages moved or deleted.

**Gap Analysis**:
*   None. (Procedural flow).

---

## OBS-005: Forecast Verification Pipeline

**Primary Components**: `cmd/archiver` (VerificationService), `worker/runpod` (Verification Task), `internal/db`

**Trigger**: `EventBridge Rule: cron(0 3 * * ? *)` (Daily 03:00 UTC) with payload `{"task": "verification"}`

**Preconditions**:
*   Forecast runs exist for 24-48h ago.
*   NOAA observational data is available.

**Execution Sequence**:
1.  **[Archiver]** Receives payload. Calls `VerificationService.TriggerVerification(ctx, window)`.
2.  **[Service]** Identifies target ForecastRuns.
    *   **[DB]** `SELECT * FROM forecast_runs WHERE run_timestamp BETWEEN $start AND $end AND status='complete'`.
3.  **[Service]** Constructs `InferencePayload`:
    *   `TaskType`: `verification`
    *   `InputConfig`: `{ "verification_window": { "start": ..., "end": ... }, "model_runs": [...] }`
4.  **[Service]** Calls `RunPodClient.TriggerInference`. Records `external_id`.
5.  **[RunPod Worker]** Starts. Detects `TaskType=verification`. Loads `verification.py`.
6.  **[RunPod Worker]** Checks Observation Data Cache (S3).
    *   If missing, downloads from NOAA (MRMS/ISD), normalizes, saves to `s3://observations/...`.
7.  **[RunPod Worker]** Iterates Forecast Runs.
    *   Reads Zarr forecast data.
    *   Reads Observation data.
    *   Computes metrics: RMSE, Bias, Brier Score.
8.  **[RunPod Worker]** Emits Metrics.
    *   **[CloudWatch]** `PutMetricData` (Namespace: WatchPoint, Metric: ForecastRMSE, Dim: Model).
9.  **[RunPod Worker]** Writes detailed results to S3 (e.g. `verification_results.json`).
10. **[RunPod Worker]** Exits Success.
11. **[Archiver]** (Later, via `ReconcileForecasts` or callback) Reads results from S3.
12. **[Archiver]** Bulk Inserts to DB.
    *   **[DB]** `INSERT INTO verification_results ...`

**Data State Changes**:
*   **S3**: Observation data cached. Results written.
*   **DB**: `verification_results` populated.
*   **CloudWatch**: Accuracy metrics published.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `TaskType` enum, `VerificationService` update).

---

## OBS-006: Calibration Coefficient Update (Bounded Automation)

**Primary Components**: `cmd/archiver` (CalibrationService), `worker/runpod` (Calibration Task), `internal/db`

**Trigger**: `EventBridge Rule: cron(0 5 * * ? *)` (Weekly)

**Preconditions**:
*   Verification results exist for the past week.

**Execution Sequence**:
1.  **[Archiver]** Calls `CalibrationService.UpdateCoefficients`.
2.  **[Service]** Triggers RunPod with `TaskType=calibration`.
3.  **[RunPod Worker]** Loads `calibration.py`.
4.  **[RunPod Worker]** Fetches last 7 days of `verification_results` (or raw data references).
5.  **[RunPod Worker]** Performs Regression Analysis (e.g., Logistic Regression for Prob(Reflectivity)).
6.  **[RunPod Worker]** outputs `proposed_coefficients.json` to S3.
7.  **[Archiver]** Reads `proposed_coefficients.json`.
8.  **[Service]** **Safety Check**:
    *   Fetches current active coefficients from DB.
    *   Compares Deltas. Limit: 15% change.
9.  **[Branch - Safe]**:
    *   **[DB]** `UPDATE calibration_coefficients SET ...`.
10. **[Branch - Unsafe/Drastic]**:
    *   **[DB]** `INSERT INTO calibration_candidates (location_id, proposed_coefficients, violation_reason, status) VALUES (..., 'delta > 15%', 'pending')`.
    *   **[Log]** Emits `CalibrationSafetyAlert` log/metric.

**Data State Changes**:
*   **DB**: `calibration_coefficients` updated OR `calibration_candidates` inserted.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `calibration_candidates` table).

---

## OBS-007: Post-Event Summary Generation

**Primary Components**: `cmd/archiver`, `worker/eval`, `internal/db`

**Trigger**: `WPLC-009` (Archiver) successfully archives a WatchPoint.

**Preconditions**:
*   WatchPoint transitioned to `archived` status.

**Execution Sequence**:
1.  **[Archiver]** Identifies archived WatchPoint ID.
2.  **[Archiver]** Enqueues Summary Job.
    *   **[SQS]** Sends to `eval-queue-standard`.
    *   Payload: `EvalMessage { TileID: "...", Action: "generate_summary", SpecificWatchPointIDs: [id] }`.
3.  **[Eval Worker]** Receives message. Detects `Action=generate_summary`.
4.  **[Eval Worker]** Routes to `SummaryGenerator`.
5.  **[Generator]** Fetches WatchPoint metadata (Time Window, Location).
6.  **[Generator]** **Data Fetch**:
    *   Reads Historical Forecast Zarr (Atlas) from S3.
    *   Reads Historical Observations (MRMS/ISD) from S3 (cached by OBS-005).
7.  **[Generator]** Computes Comparison.
    *   "Predicted Max Temp: 22C, Actual: 24C".
    *   "Predicted Precip Prob: 60%, Actual: 0mm".
8.  **[Generator]** Persists Summary.
    *   **[DB]** `UPDATE watchpoints SET summary = $jsonb WHERE id = $id`.
9.  **[Generator]** Enqueues Notification.
    *   **[SQS]** Sends `NotificationMessage` (Type: `event_summary`) to `notification-queue`.

**Data State Changes**:
*   **DB**: `watchpoints.summary` populated.
*   **SQS**: Notification enqueued.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `summary` column, `EvalAction` enum).

---

## OBS-008: Canary WatchPoint Evaluation

**Primary Components**: `cmd/archiver` (CanaryVerifier), `internal/db`, `CloudWatch`

**Trigger**: `EventBridge Rule: rate(30 minutes)`

**Preconditions**:
*   System WatchPoints exist with `metadata: {"system_role": "canary", "canary_expect_trigger": boolean}`.

**Execution Sequence**:
1.  **[Archiver]** Calls `CanaryVerifier.Verify`.
2.  **[Verifier]** Queries Canary WatchPoints.
    *   **[DB]** `SELECT id, metadata, id FROM watchpoints WHERE metadata->>'system_role' = 'canary'`.
3.  **[Verifier]** Iterates Canaries.
    *   Fetches `watchpoint_evaluation_state`.
    *   Reads `previous_trigger_state`.
    *   Reads expected state from `metadata`.
4.  **[Logic]** Compare Actual vs Expected.
    *   If `Actual != Expected`:
        *   Log Error: "Canary Mismatch: WP %s Expected %v Got %v".
        *   **[CloudWatch]** `PutMetricData` (Metric: `CanaryFailure`, Value: 1).
    *   If Match:
        *   **[CloudWatch]** `PutMetricData` (Metric: `CanarySuccess`, Value: 1).

**Data State Changes**:
*   **CloudWatch**: Metrics emitted.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Canary metadata standard).


# Flow Simulations: Observability & Maintenance (OBS/MAINT)

> **Range**: OBS-009 to MAINT-002
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## OBS-009: Anomaly Detection — Notification Spike (Procedural)

**Primary Components**: `CloudWatch`, `SNS`, `PagerDuty`, `Ops CLI`

**Trigger**: CloudWatch Alarm `NotificationSpikeAlarm` (defined in `04-sam-template` via Refactoring)

**Preconditions**:
*   Notification volume exceeds threshold (e.g., > 1000 per 5 mins).

**Execution Sequence**:
1.  **[CloudWatch]** Evaluates metric `WatchPoint/DeliveryAttempt`.
2.  **[CloudWatch]** Threshold breached. State transitions to `ALARM`.
3.  **[CloudWatch]** Publishes message to `AlertSNSTopic`.
4.  **[Ops]** Receives PagerDuty alert: "Critical: Notification Volume Spike".
5.  **[Ops]** Investigation Phase:
    *   Check `WatchPoint/DeliverySuccess` vs `DeliveryFailed`.
    *   Identify if specific Org or generic bug.
6.  **[Ops]** Action Phase (Kill Switch):
    *   **[CLI]** Updates SSM Parameter `/prod/watchpoint/features/enable_email` to `false`.
    *   **[CLI]** Forces Lambda config update (re-deploy) to flush cold start caches.
    *   *Result*: `EmailWorker` stops processing (drops/nacks messages or skips delivery).

**Data State Changes**:
*   **SSM**: Feature flag updated.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Alarm added to SAM, Kill Switch flow defined as procedural).

---

## OBS-010: Metrics Emission (Distributed)

**Primary Components**: `api` (`MetricsMiddleware`), `batcher`, `workers`

**Trigger**: Various runtime events

**Scenario A: API Request**
1.  **[Middleware]** Intercepts incoming HTTP request.
2.  **[Middleware]** Records `startTime`.
3.  **[Middleware]** Calls `next.ServeHTTP`.
4.  **[Middleware]** Intercepts response status code (e.g., 200).
5.  **[Middleware]** Calculates `duration`.
6.  **[Middleware]** Calls `MetricsCollector.RecordRequest`.
    *   **[CloudWatch]** `PutMetricData` (Async/Buffered).
    *   Metric: `APILatency`, Dims: `{Endpoint: "/v1/watchpoints", Method: "POST"}`.
    *   Metric: `APIRequestCount`, Dims: `{Status: "200"}`.

**Scenario B: Batcher Execution**
1.  **[Batcher]** Completes tile grouping.
2.  **[Batcher]** Calls `MetricsPublisher.PublishForecastReady`.
    *   **[CloudWatch]** Metric: `ForecastReady`, Value: 1.

**Scenario C: Notification Delivery**
1.  **[Worker]** Delivery attempt finishes.
2.  **[Worker]** Calls `NotificationMetrics.RecordDelivery`.
    *   **[CloudWatch]** Metric: `DeliveryAttempt`, Dims: `{Channel: "email", Result: "success"}`.

**Data State Changes**:
*   **CloudWatch**: Metrics ingested.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Middleware addition).

---

## OBS-011: Audit Log Query

**Primary Components**: `api` (`OrganizationHandler`), `internal/db` (`AuditRepository`)

**Trigger**: `GET /v1/organization/audit-logs?resource_type=watchpoint&limit=20`

**Preconditions**:
*   User has `RoleAdmin` or `RoleOwner`.

**Execution Sequence**:
1.  **[Handler]** Authenticates user, extracts `OrgID`.
2.  **[Handler]** Parses query parameters into `types.AuditQueryFilters`.
    *   `ResourceType`: "watchpoint"
    *   `Limit`: 20
3.  **[Handler]** Calls `Repo.List(ctx, filters)`.
4.  **[DB]** Executes Query:
    ```sql
    SELECT * FROM audit_log
    WHERE organization_id = $1
      AND resource_type = 'watchpoint'
    ORDER BY created_at DESC
    LIMIT 21; -- Fetch +1 for cursor
    ```
5.  **[Handler]** Constructs `AuditLogResponse`.
6.  **[Handler]** Returns JSON list.

**Data State Changes**:
*   None (Read-only).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Endpoint and Repo method added).

---

## MAINT-001: Forecast Data Tier Transition

**Primary Components**: `cmd/archiver`, `internal/scheduler`, `AWS S3`

**Trigger**: `EventBridge Rule: cron(0 4 * * ? *)` (Daily) AND S3 Lifecycle Rules

**Phase 1: Infrastructure (Physical Deletion)**
1.  **[AWS S3]** Lifecycle Rule evaluates bucket contents.
2.  **[AWS S3]** Objects in `nowcast/` prefix > 7 days old are expired (deleted).
3.  **[AWS S3]** Objects in `medium_range/` prefix > 90 days old are expired.

**Phase 2: Code (Logical Update)**
1.  **[Archiver]** Wakes up for `task: transition_forecasts`.
2.  **[Service]** Queries `forecast_runs` for logically expired items.
    *   **[DB]** `SELECT id FROM forecast_runs WHERE ...` (Logic defined in `09-scheduled-jobs`).
3.  **[Service]** Updates Status.
    *   **[DB]** `UPDATE forecast_runs SET status='deleted', storage_path='' WHERE id=$id`.
    *   *Note*: Service does not need to issue `DeleteObject` calls if S3 Lifecycle is relied upon, but explicitly issuing them ensures consistency.

**Data State Changes**:
*   **S3**: Objects deleted.
*   **DB**: Forecast runs marked deleted.

**Gap Analysis**:
*   None. (Addressed by Refactoring: S3 Lifecycle rules explicitly defined in SAM).

---

## MAINT-002: Archived WatchPoint Cleanup

**Primary Components**: `cmd/archiver`, `internal/db`

**Trigger**: `EventBridge Rule: cron(0 2 * * ? *)` (Daily)

**Preconditions**:
*   WatchPoints exist with `status='archived'` AND `archived_at < NOW() - 90 days`.

**Execution Sequence**:
1.  **[Archiver]** Wakes up for `task: cleanup_soft_deletes` (includes archived WPs).
2.  **[Service]** Calls `CleanupService.PurgeArchivedWatchPoints`.
3.  **[Service]** Selects Batch (Limit 1000).
    *   **[DB]** `SELECT id FROM watchpoints WHERE status='archived' AND ... LIMIT 1000`.
4.  **[Service]** Executes Deletion.
    *   **[DB]** `DELETE FROM watchpoints WHERE id = ANY($ids)`.
5.  **[DB]** **Cascade Action** (Crucial):
    *   Postgres deletes rows in `watchpoint_evaluation_state` via FK cascade.
    *   Postgres deletes rows in `notifications` via FK cascade (Refactored schema).
    *   Postgres deletes rows in `notification_deliveries` via FK cascade (Refactored schema).
6.  **[Service]** Logs count of deleted items.

**Data State Changes**:
*   **DB**: Hard delete of WatchPoints and all related child records.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Added `ON DELETE CASCADE` to schema).


# Flow Simulations: Maintenance & Data Lifecycle (MAINT)

> **Range**: MAINT-003 to MAINT-007
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## MAINT-003: Notification History Retention

**Primary Components**: `cmd/archiver`, `internal/db`

**Trigger**: `EventBridge Rule: cron(0 3 * * ? *)` (Daily) via `cleanup_notifications` task

**Preconditions**:
*   `notifications` table contains records older than retention policy (e.g., 90 days).

**Execution Sequence**:
1.  **[Archiver]** Receives payload. Calls `CleanupService.PurgeNotifications(ctx, 90*24*time.Hour)`.
2.  **[Service]** Calculates cutoff time: `now - 90 days`.
3.  **[Service]** Calls `NotificationRepo.DeleteBefore(ctx, cutoff)`.
4.  **[DB]** Executes:
    ```sql
    DELETE FROM notifications WHERE created_at < $1;
    ```
5.  **[DB]** **Cascade Action**: The deletion cascades to `notification_deliveries` via the foreign key constraint `ON DELETE CASCADE`, removing child records automatically.
6.  **[Service]** Logs count of deleted items.

**Data State Changes**:
*   **DB**: Old rows removed from `notifications` and `notification_deliveries`.

**Gap Analysis**:
*   None. (Relies on Refactoring: `DeleteBefore` method).

---

## MAINT-004: Audit Log Retention & Archival

**Primary Components**: `cmd/archiver`, `internal/db`, `AWS S3`

**Trigger**: `EventBridge Rule: cron(0 1 1 * ? *)` (Monthly) via `archive_audit_logs` task

**Preconditions**:
*   `audit_log` table contains records older than 2 years.
*   `ArchiveBucket` is configured and accessible.

**Execution Sequence**:
1.  **[Archiver]** Receives payload. Calls `CleanupService.ArchiveAuditLogs(ctx, 2*365*24*time.Hour, 1000)`.
2.  **[Service]** Calculates cutoff: `now - 2 years`.
3.  **[Service]** Loop (Batch Processing):
    *   **[Service]** Calls `AuditRepo.ListOlderThan(ctx, cutoff, limit=1000)`.
    *   **[DB]** `SELECT * FROM audit_log WHERE created_at < $1 LIMIT 1000`.
    *   If 0 results, break.
    *   **[Service]** Serializes batch to JSONL (Newline Delimited JSON).
    *   **[Service]** Generates key: `audit/YYYY/MM/batch_{uuid}.jsonl.gz`.
    *   **[Service]** Uploads to S3 `ArchiveBucket`.
    *   **[Service]** Calls `AuditRepo.DeleteIDs(ctx, batchIDs)`.
    *   **[DB]** `DELETE FROM audit_log WHERE id = ANY($1)`.

**Data State Changes**:
*   **S3**: New archive object created in Cold Storage bucket.
*   **DB**: Old rows removed from `audit_log`.

**Gap Analysis**:
*   None. (Relies on Refactoring: `ArchiveBucket` infra, Repo methods).

---

## MAINT-005: Soft-Deleted Organization Cleanup

**Primary Components**: `cmd/archiver`, `internal/db`

**Trigger**: `EventBridge Rule: cron(0 2 * * ? *)` (Daily) via `cleanup_soft_deletes` task

**Preconditions**:
*   Organizations exist with `deleted_at < NOW() - 30 days`.

**Execution Sequence**:
1.  **[Archiver]** Calls `CleanupService.PurgeSoftDeletedOrgs(ctx, 30*24*time.Hour, 50)`.
2.  **[Service]** Queries candidates:
    *   **[DB]** `SELECT id FROM organizations WHERE deleted_at < $cutoff LIMIT $limit`.
3.  **[Service]** Iterates IDs.
4.  **[Service]** Executes Hard Delete.
    *   **[DB]** `DELETE FROM organizations WHERE id = $id`.
5.  **[DB]** **Cascade Action**:
    *   `users` deleted (ON DELETE CASCADE).
    *   `watchpoints` deleted (ON DELETE CASCADE).
    *   `api_keys` deleted (ON DELETE CASCADE).
    *   `sessions` deleted (ON DELETE CASCADE).
    *   `rate_limits` deleted (ON DELETE CASCADE).
6.  **[DB]** **Set Null Action**:
    *   `audit_log.organization_id` set to NULL (ON DELETE SET NULL). This preserves compliance history.
7.  **[Service]** Logs count.

**Data State Changes**:
*   **DB**: Organization and all child resources permanently removed. Audit logs anonymized but retained.

**Gap Analysis**:
*   None. (Relies on Refactoring: Schema updates for CASCADE and SET NULL).

---

## MAINT-006: Expired Invite Cleanup

**Primary Components**: `cmd/archiver`, `internal/db`

**Trigger**: `EventBridge Rule: cron(0 4 * * ? *)` (Daily) via `cleanup_invites` task

**Preconditions**:
*   Users exist with `status='invited'` and `invite_expires_at < NOW()`.

**Execution Sequence**:
1.  **[Archiver]** Calls `CleanupService.PurgeExpiredInvites(ctx, now)`.
2.  **[Service]** Executes Query:
    *   **[DB]** `DELETE FROM users WHERE status = 'invited' AND invite_expires_at < $1`.
3.  **[Service]** Logs count of cleaned up tokens.

**Data State Changes**:
*   **DB**: Stale user records removed.

**Gap Analysis**:
*   None.

---

## MAINT-007: Stale Evaluation State Cleanup (Pruning)

**Primary Components**: `cmd/archiver`, `internal/db`

**Trigger**: `EventBridge Rule: cron(0 5 * * ? 0)` (Weekly) via `prune_state` task

**Preconditions**:
*   `watchpoint_evaluation_state` records contain `seen_threats` entries older than 7 days.

**Execution Sequence**:
1.  **[Archiver]** Calls `WatchPointRepo.PruneSeenThreats(ctx, 7*24*time.Hour)`.
2.  **[Repo]** Executes JSONB update (Postgres):
    ```sql
    UPDATE watchpoint_evaluation_state
    SET seen_threats = (
      SELECT jsonb_agg(threat)
      FROM jsonb_array_elements(seen_threats) as threat
      WHERE (threat->>'end_time')::timestamptz > NOW() - INTERVAL '7 days'
    )
    WHERE seen_threats IS NOT NULL 
      AND jsonb_array_length(seen_threats) > 0;
    ```
3.  **[Repo]** Returns count of updated rows.

**Data State Changes**:
*   **DB**: `seen_threats` JSONB arrays compacted, removing expired history to prevent unlimited growth.

**Gap Analysis**:
*   None. (Relies on Refactoring: `PruneSeenThreats` method definition).


# Flow Simulations: Scheduled Jobs (SCHED)

> **Range**: SCHED-001 to SCHED-005
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## SCHED-001 / SCHED-002: Rate Limit Reset & Usage Aggregation

**Primary Components**: `cmd/archiver` (Multiplexer), `internal/billing` (`UsageAggregator`), `internal/db`

**Trigger**: `EventBridge Rule: cron(0 1 * * ? *)` (Daily 01:00 UTC) with payload `{"task": "aggregate_usage"}`

**Preconditions**:
*   `rate_limits` table contains counts for the previous day (or older).
*   `organizations` table has valid `plan_limits`.

**Execution Sequence**:
1.  **[Archiver]** Receives payload. Acquires lock `aggregate_usage:{date}`. Calls `UsageAggregator.SnapshotDailyUsage(ctx, now)`.
2.  **[Service]** Identifies stale rate limits (Batched).
    *   **[DB]** `SELECT organization_id FROM rate_limits WHERE period_end < $now LIMIT 50`.
3.  **[Service]** Iterates Organization IDs. For each Org:
    *   **[Service]** Starts Transaction (Isolation: Read Committed).
    *   **[DB]** Locks row:
        ```sql
        SELECT api_calls_count, watchpoints_count, period_start, period_end
        FROM rate_limits WHERE organization_id = $1 FOR UPDATE;
        ```
    *   **[DB]** Inserts History:
        ```sql
        INSERT INTO usage_history (organization_id, date, api_calls, watchpoints)
        VALUES ($1, $period_start::date, $api_count, $wp_count)
        ON CONFLICT (organization_id, date) DO NOTHING;
        ```
    *   **[Service]** **Overage Check Logic**:
        *   Fetch `plan_limits` and `overage_started_at` from `organizations`.
        *   If `api_count > limit` OR `wp_count > limit`:
            *   If `overage_started_at` IS NULL: Set to `NOW()`.
        *   Else (Compliant):
            *   If `overage_started_at` IS NOT NULL: Set to `NULL` (Cure).
        *   **[DB]** `UPDATE organizations SET overage_started_at = $val ...`
    *   **[DB]** Resets Rate Limit:
        ```sql
        UPDATE rate_limits
        SET api_calls_count = 0,
            period_start = $now,
            period_end = $now + INTERVAL '24 hours',
            warning_sent_at = NULL,  -- Clear 80% warning flag
            last_reset_at = NOW()
        WHERE organization_id = $1;
        ```
    *   **[Service]** Commits Transaction.
4.  **[Service]** Logs metrics: `UsageSnapshotCreated`.

**Data State Changes**:
*   **DB**: `rate_limits` counters reset. `usage_history` populated. `organizations.overage_started_at` potentially updated.

**Gap Analysis**:
*   None. (Uses strict locking to prevent race conditions with live API traffic).

---

## SCHED-003: Stripe Subscription Sync (Daily Reconciliation)

**Primary Components**: `cmd/archiver`, `internal/billing` (`StripeSyncer`), `external/stripe`

**Trigger**: `EventBridge Rule: cron(0 0 * * ? *)` (Daily) with payload `{"task": "sync_stripe"}`

**Preconditions**:
*   `StripeService` configured.

**Execution Sequence**:
1.  **[Archiver]** Calls `StripeSyncer.SyncAtRisk(ctx, now, 24h, 50)`.
2.  **[Syncer]** Queries candidates:
    *   **[DB]** `SELECT id, stripe_customer_id, plan, subscription_status FROM organizations WHERE (last_billing_sync_at < NOW() - INTERVAL '24 hours' OR stripe_customer_id IS NULL) AND deleted_at IS NULL LIMIT 50`.
3.  **[Syncer]** Iterates Orgs.
4.  **[Branch - Headless Repair]** (`stripe_customer_id` is NULL):
    *   **[Syncer]** Calls `BillingService.EnsureCustomer`.
    *   **[Stripe]** Search/Create Customer.
    *   **[DB]** `UPDATE organizations SET stripe_customer_id = $id ...`.
5.  **[Branch - State Sync]** (ID exists):
    *   **[Syncer]** Calls `BillingService.GetSubscription(ctx, orgID)`.
    *   **[Stripe]** `GET /v1/subscriptions`.
    *   **[Logic]** Compare `remote.Plan` vs `local.Plan` AND `remote.Status` vs `local.SubscriptionStatus`.
    *   If Mismatch:
        *   **[Syncer]** Calls `SubscriptionStateRepo.UpdateSubscriptionStatus(...)`.
        *   **[DB]** Updates plan/status using optimistic lock (`last_subscription_event_at`).
        *   **[Log]** Emits metric `BillingStateDrift`.
6.  **[Syncer]** Updates timestamp:
    *   **[DB]** `UPDATE organizations SET last_billing_sync_at = NOW() WHERE id = $id`.

**Data State Changes**:
*   **DB**: `organizations` billing fields synchronized.

**Gap Analysis**:
*   None. (Uses existing repository logic for consistency).

---

## SCHED-004: Canary WatchPoint Verification

**Primary Components**: `cmd/archiver`, `internal/scheduler` (`CanaryVerifier`), `internal/db`

**Trigger**: `EventBridge Rule: rate(30 minutes)` (Implicit trigger logic in Archiver)

**Preconditions**:
*   WatchPoints exist with `metadata` containing `{"system_role": "canary", "canary_expect_trigger": boolean}`.

**Execution Sequence**:
1.  **[Archiver]** Detects scheduled time (or explicit payload). Calls `CanaryVerifier.Verify(ctx)`.
2.  **[Verifier]** Queries Canaries:
    *   **[DB]**
        ```sql
        SELECT wp.id, wp.metadata, wes.previous_trigger_state
        FROM watchpoints wp
        LEFT JOIN watchpoint_evaluation_state wes ON wp.id = wes.watchpoint_id
        WHERE wp.metadata->>'system_role' = 'canary' AND wp.status = 'active';
        ```
3.  **[Verifier]** Iterates results:
    *   `expected = row.metadata['canary_expect_trigger']`
    *   `actual = row.previous_trigger_state` (Defaults to `FALSE` if NULL/missing).
    *   If `expected != actual`:
        *   **[CloudWatch]** `PutMetricData(MetricName="CanaryFailure", Value=1, Dimensions={Region})`.
        *   **[Log]** Error: "Canary Mismatch on WP %s".
    *   Else (Match):
        *   **[CloudWatch]** `PutMetricData(MetricName="CanarySuccess", Value=1)`.

**Data State Changes**:
*   **CloudWatch**: Metrics emitted.

**Gap Analysis**:
*   None.

---

## SCHED-005: Dead Letter Queue Processing (Procedural)

**Primary Components**: `CloudWatch`, `AWS Console` (SQS)

**Trigger**: CloudWatch Alarm `DLQMessagesAlarm` (Queue Depth > 0)

**Preconditions**:
*   Messages exist in `eval-queue-dlq` or `notification-queue-dlq`.

**Execution Sequence**:
1.  **[CloudWatch]** Alarm state transitions to `ALARM`. Sends SNS notification to Ops.
2.  **[Ops]** Logs into AWS Console -> SQS.
3.  **[Ops]** Inspects messages in DLQ.
    *   *Check*: Is payload malformed (Poison Pill)?
    *   *Check*: Is it a timeout artifact?
4.  **[Ops]** Action Phase:
    *   *Scenario A (Transient)*: Select messages -> "Start DLQ Redrive" -> Move to Source Queue.
    *   *Scenario B (Poison Pill)*: Delete specific messages or Purge Queue.
5.  **[CloudWatch]** Alarm returns to `OK`.

**Data State Changes**:
*   **SQS**: Messages moved or deleted.

**Gap Analysis**:
*   None. (Procedural flow leveraging native AWS features).


# Flow Simulations: Scheduled Jobs (SCHED) - Continued

> **Range**: SCHED-006
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## SCHED-006: Webhook Health Pre-Check (Deferred)

**Primary Components**: N/A

**Trigger**: N/A

**Execution Sequence**:
*   **Decision**: This flow is explicitly **deferred** for v1.
*   **Rationale**: Proactive probing of customer webhooks introduces significant complexity regarding privacy, potential blocklisting, and load management. 
*   **Alternative**: The system relies on **Reactive Circuit Breaking** (see `NOTIF-006` and `NOTIF-010`) handled by the `WebhookWorker` to disable unhealthy channels based on actual delivery failures.

**Gap Analysis**:
*   None. (Decision logged as architectural choice).


# Flow Simulations: API Mechanics (API)

> **Range**: API-001 to API-004
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## API-001: Request Authentication & Authorization

**Primary Components**: `api` (`AuthMiddleware`), `internal/auth` (`Authenticator`), `internal/db`

**Trigger**: Incoming HTTP Request to protected endpoint

**Preconditions**:
*   Request contains `Authorization` header.

**Execution Sequence**:
1.  **[Router]** Middleware Stack executes. Order is critical:
    *   `Recoverer` -> `ContextTimeout` -> `RequestID` -> `Logger` -> `CORS` -> `Metrics`.
    *   **[AuthMiddleware]** (Executes BEFORE RateLimit/Idempotency).
2.  **[Middleware]** Extracts token. Parses prefix (`sk_live_`, `sk_test_`, `sess_`).
3.  **[Middleware]** Calls `Authenticator.ResolveToken(ctx, tokenString)`.
4.  **[Authenticator]** Hashes token.
5.  **[DB]** Executes Query:
    ```sql
    SELECT id, organization_id, key_hash, expires_at, scopes, test_mode
    FROM api_keys
    WHERE key_hash = $1 AND revoked_at IS NULL;
    ```
6.  **[Authenticator]** Application Logic:
    *   If no row: Return `ErrAuthTokenInvalid`.
    *   If `expires_at` is NOT NULL AND `expires_at < NOW()`: Return `ErrAuthTokenExpired`.
7.  **[Middleware]** Constructs `Actor` struct (ID, OrgID, Scopes, TestMode).
8.  **[Middleware]** Injects `Actor` into Context.
9.  **[Router]** Passes to `RateLimitMiddleware` (uses OrgID from context).
10. **[Router]** Passes to `IdempotencyMiddleware` (uses OrgID from context).
11. **[Handler]** `RequireScope("watchpoints:write")` middleware checks Actor scopes.
12. **[Handler]** Business logic executes.

**Data State Changes**:
*   None (Read-only).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Middleware reordering, Expiration logic).

---

## API-002: Webhook URL Validation (SSRF Protection)

**Primary Components**: `api` (`Validator`), `01-foundation-types` (`SSRFValidator`)

**Trigger**: `POST /v1/watchpoints` (Create/Update)

**Preconditions**:
*   Request body contains a webhook channel configuration.

**Execution Sequence**:
1.  **[Handler]** Decodes JSON request.
2.  **[Handler]** Calls `Validator.ValidateStruct(req)`.
3.  **[Validator]** Encounters field tagged `validate:"ssrf_url"`.
4.  **[Validator]** Invokes custom validation function.
5.  **[Validation Logic]**:
    *   Check URL Scheme (`https` required).
    *   Create context with **500ms timeout**.
    *   Call `net.LookupIP(host)`.
    *   **[Branch - Timeout]**: If timeout occurs, return `ErrValidationInvalidWebhook` (Fail Closed). API returns 400.
    *   **[Branch - Success]**: Iterate resolved IPs.
    *   Check against Blocked CIDRs (Localhost, Private, Link-Local, Cloud Metadata).
6.  **[Validator]** If blocked: Return error. If safe: Return nil.
7.  **[Handler]** Proceed to DB insert.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit timeout and wiring instructions).

---

## API-003: Health Check Endpoint

**Primary Components**: `api` (`HandleHealth`), `internal/db`, `AWS SDK`

**Trigger**: `GET /v1/health`

**Preconditions**:
*   None (Public endpoint).

**Execution Sequence**:
1.  **[Handler]** Creates context with **2 second timeout**.
2.  **[Handler]** Spawns Goroutines (Concurrent Execution):
    *   **Routine A (DB)**: Calls `pgxpool.Ping(ctx)`.
    *   **Routine B (S3)**: Calls `s3Client.HeadBucket(ctx, Bucket=ForecastBucket)`.
    *   **Routine C (SQS)**: Calls `sqsClient.GetQueueAttributes(ctx, QueueUrl=NotificationQueue)`.
3.  **[Handler]** Waits for `WaitGroup` or Context Timeout.
4.  **[Logic]**
    *   If Timeout: Return 503 Service Unavailable (`{"error": "timeout"}`).
    *   If Any Error: Return 503 Service Unavailable (`{"status": "unhealthy", "details": ...}`).
    *   If All Success: Return 200 OK (`{"status": "healthy"}`).

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Concurrent execution and global timeout specs).

---

## API-004: Idempotency Key Handling

**Primary Components**: `api` (`IdempotencyMiddleware`), `internal/db`

**Trigger**: `POST` request with `Idempotency-Key` header

**Preconditions**:
*   Authentication Middleware has successfully run (OrgID available).

**Execution Sequence**:
1.  **[Middleware]** Extracts Header `key` and Context `OrgID`.
2.  **[Middleware]** Checks DB.
    *   **[DB]** `SELECT response_code, response_body, status FROM idempotency_keys WHERE id=$1 AND organization_id=$2`.
3.  **[Logic]**
    *   *Case A (Found & Completed)*: Return stored `response_code` and `response_body` immediately. Stop chain.
    *   *Case B (Found & Processing)*: Return `409 Conflict` ("Request in progress"). Stop chain.
    *   *Case C (New)*: Proceed.
4.  **[Middleware]** Creates Record.
    *   **[DB]** `INSERT INTO idempotency_keys (id, organization_id, status, expires_at) VALUES ($1, $2, 'processing', NOW() + 24h)`.
5.  **[Middleware]** Wraps `http.ResponseWriter` with `ResponseCapturer` struct.
6.  **[Middleware]** Calls `next.ServeHTTP(capturer, req)`.
7.  **[Handler]** Processes request (e.g., creates WatchPoint), writes 201 JSON to capturer.
8.  **[Middleware]** Intercepts return.
9.  **[Middleware]** Updates Record.
    *   **[DB]** `UPDATE idempotency_keys SET status='completed', response_code=$code, response_body=$body WHERE id=$1`.
10. **[Middleware]** Writes captured data to actual client response.

**Data State Changes**:
*   **DB**: `idempotency_keys` inserted and updated.

**Gap Analysis**:
*   None. (Addressed by Refactoring: ResponseCapturer pattern and cleanup job).


# Flow Simulations: Forecast Queries (FQRY)

> **Range**: FQRY-001 to FQRY-005
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## FQRY-001: Point Forecast Retrieval

**Primary Components**: `api` (`ForecastHandler`), `internal/forecasts` (`ForecastService`, `ForecastReader`), `internal/db` (`ForecastRunRepository`), `AWS S3`

**Trigger**: `GET /v1/forecasts/point?lat=40.7&lon=-74.0&...`

**Preconditions**:
*   User authenticated.
*   S3 bucket contains valid Zarr stores.

**Execution Sequence**:
1.  **[Handler]** Parses query params: `lat`, `lon`, `start`, `end`, `variables`.
2.  **[Handler]** Calls `ForecastService.GetPointForecast(ctx, lat, lon, start, end)`.
3.  **[Service]** Identifies required models.
    *   If `start < NOW + 6h`: Needs Nowcast.
    *   Always needs Medium-Range (for fallback and >6h).
4.  **[Service]** Calls `RunRepo.GetLatestServing(ctx, model)` for both.
    *   **[DB]** `SELECT * FROM forecast_runs WHERE model=$1 AND status='complete' ORDER BY run_timestamp DESC LIMIT 1`.
5.  **[Service]** **Staleness/Fallback Check**:
    *   If Nowcast run is missing or >90m old: Flag `fallback_mode = true`.
6.  **[Service]** Calls `ForecastReader.ReadPoint` (Medium-Range).
    *   **[Reader]** Calculates TileID from Lat/Lon (e.g., "2.3").
    *   **[Reader]** Constructs S3 Key: `medium_range/{ts}/{var}/2.3`.
    *   **[Reader]** **[S3]** `GetObject` (Parallel fetch for each variable).
    *   **[Reader]** Decompresses Zstd payload.
    *   **[Reader]** **Bilinear Interpolation**:
        *   Maps `lat/lon` to grid indices `(row, col)`.
        *   Identifies 4 neighbors in flat float32 array.
        *   Calculates weighted average.
7.  **[Service]** Calls `ForecastReader.ReadPoint` (Nowcast) *unless fallback*.
    *   Same read logic, different resolution/S3 path.
8.  **[Service]** **Blending Logic**:
    *   For `t < 6h`: Use Nowcast value (or Medium-Range if `fallback_mode`).
    *   For `t > 6h`: Use Medium-Range value.
9.  **[Service]** Constructs `ForecastResponse`.
    *   If fallback used, adds `Metadata.Warnings: [{Code: "source_fallback_medium_range"}]`.
10. **[Handler]** Returns JSON response.

**Data State Changes**:
*   None (Read-only).

**Gap Analysis**:
*   None. (Relies on Hardcoded Schema constants to avoid extra S3 metadata lookups).

---

## FQRY-002: Batch Forecast Retrieval

**Primary Components**: `api`, `internal/forecasts`, `AWS S3`

**Trigger**: `POST /v1/forecasts/points`

**Preconditions**:
*   Request body contains up to 50 locations.

**Execution Sequence**:
1.  **[Handler]** Validates batch size <= 50.
2.  **[Handler]** Calls `ForecastService.GetBatchForecast(ctx, req)`.
3.  **[Service]** Fetches latest runs from DB.
4.  **[Service]** **Scatter Phase (Grouping)**:
    *   Iterates locations, calculates TileID for each.
    *   Groups: `map[TileID][]Location`.
5.  **[Service]** **Parallel Fetch**:
    *   Creates `errgroup` with **Semaphore** (Limit: 10 concurrent S3 operations).
    *   Loops over Tiles.
    *   Spawns Goroutine per Tile:
        *   **[Reader]** Fetches Zarr chunk for requested variables.
        *   **[Reader]** Decompresses into memory buffer.
        *   **[Reader]** Iterates locations within this tile.
        *   **[Reader]** Performs interpolation for each location using the *same* memory buffer.
6.  **[Service]** **Gather Phase**:
    *   Collects results into `BatchForecastResult` map.
    *   Handles partial tile failures (adds to `Errors` map).
7.  **[Handler]** Returns results.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Semaphore implementation prevents ephemeral port exhaustion).

---

## FQRY-003: Variable Availability Query

**Primary Components**: `api`, `internal/forecasts`

**Trigger**: `GET /v1/forecasts/variables`

**Execution Sequence**:
1.  **[Handler]** Accesses `types.StandardVariables` map (Source of Truth).
2.  **[Handler]** Iterates map to construct `VariableResponseMetadata` list.
3.  **[Handler]** Returns static JSON.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Static registry avoids S3 latency).

---

## FQRY-004: Forecast Status/Health Query

**Primary Components**: `api`, `internal/db`

**Trigger**: `GET /v1/forecasts/status`

**Execution Sequence**:
1.  **[Handler]** Calls `ForecastService.GetStatus(ctx)`.
2.  **[Service]** Queries DB:
    *   `SELECT model, run_timestamp FROM forecast_runs WHERE status='complete' ORDER BY run_timestamp DESC` (Distinct by model).
3.  **[Service]** Calculates Age: `Now - RunTimestamp`.
4.  **[Service]** **Health Logic**:
    *   **Medium-Range**: <8h (Healthy), 8-12h (Degraded), >12h (Stale).
    *   **Nowcast**: <45m (Healthy), 45-90m (Degraded), >90m (Stale).
5.  **[Handler]** Returns JSON with status enums.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None.

---

## FQRY-005: Forecast Data for WatchPoint (Context)

**Primary Components**: `api` (`WatchPointHandler`), `internal/forecasts`

**Trigger**: Embedded call within `WPLC-001` (Create WatchPoint)

**Preconditions**:
*   WatchPoint creation is in progress.

**Execution Sequence**:
1.  **[WP Handler]** Creates context with **500ms timeout** (`context.WithTimeout`).
2.  **[WP Handler]** Calls `ForecastService.GetSnapshot(shortCtx, lat, lon)`.
3.  **[Service]** Selects **Medium-Range (Atlas)** model only (for stability).
4.  **[Service]** Calls `Reader.ReadPoint`.
5.  **[Reader]** Initiates S3 GetObject.
6.  **[Branch - Success]**:
    *   S3 returns within 500ms.
    *   Reader interpolates and returns values.
    *   Handler adds to response.
7.  **[Branch - Timeout/Failure]**:
    *   Context deadline exceeded or S3 error.
    *   Service returns error.
    *   **[WP Handler]** Logs warning (`SnapshotFetchFailed`).
    *   **[WP Handler]** Sets `current_forecast: null`.
    *   **[WP Handler]** Proceeds with WatchPoint creation (Soft Fail).

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Explicit timeout prevents S3 latency from blocking API writes blocking).


# Flow Simulations: Informational & History (INFO)

> **Range**: INFO-001 to INFO-005
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## INFO-001: Notification History Retrieval (WatchPoint Scoped)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db` (`NotificationRepository`)

**Trigger**: `GET /v1/watchpoints/{id}/notifications`

**Preconditions**:
*   User authenticated, owns WatchPoint.

**Execution Sequence**:
1.  **[Handler]** Authenticates user, extracts `WatchPointID`.
2.  **[Handler]** Calls `NotificationRepo.List(ctx, filter)`.
    *   Filter: `{ WatchPointID: $id, OrganizationID: $orgID, Limit: 20 }`.
3.  **[Repo]** Executes Query (Join Pattern):
    ```sql
    SELECT n.id, n.event_type, n.created_at, n.payload,
           nd.channel_type, nd.status, nd.delivered_at
    FROM notifications n
    LEFT JOIN notification_deliveries nd ON n.id = nd.notification_id
    WHERE n.watchpoint_id = $1 AND n.organization_id = $2
    ORDER BY n.created_at DESC
    LIMIT 20;
    ```
4.  **[Repo]** **Aggregation Logic**:
    *   Iterates rows.
    *   Groups by `notification_id`.
    *   Constructs `NotificationHistoryItem` DTO.
    *   Populates `Channels: []DeliverySummary` slice (e.g., `[{Channel: "email", Status: "sent"}, {Channel: "webhook", Status: "failed"}]`).
5.  **[Handler]** Returns `ListResponse` JSON.

**Data State Changes**:
*   None (Read-only).

**Gap Analysis**:
*   None. (Uses consolidated Repository and DTOs).

---

## INFO-002: Organization Notification History

**Primary Components**: `api` (`OrganizationHandler`), `internal/db` (`NotificationRepository`)

**Trigger**: `GET /v1/organization/notifications?event_type=threshold_crossed&limit=50`

**Preconditions**:
*   User has `RoleMember` or higher.

**Execution Sequence**:
1.  **[Handler]** Extracts `OrgID` from context.
2.  **[Handler]** Parses query params into `types.NotificationFilter`.
    *   `EventTypes`: `["threshold_crossed"]`
    *   `Pagination`: `{ Limit: 50 }`
3.  **[Handler]** Calls `NotificationRepo.List(ctx, filter)`.
4.  **[Repo]** Executes Query:
    *   Uses Index: `idx_notif_org` (`organization_id`, `created_at DESC`).
    *   Applies dynamic WHERE clause for `event_type`.
    *   Joins `notification_deliveries` (same as INFO-001) to hydrate status.
5.  **[Repo]** Returns `[]NotificationHistoryItem`.
6.  **[Handler]** Returns JSON response.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Addressed by Refactoring: New Endpoint and Index).

---

## INFO-003: Current Usage Dashboard

**Primary Components**: `api` (`UsageHandler`), `internal/billing` (`UsageReporter`), `internal/db`

**Trigger**: `GET /v1/usage`

**Preconditions**:
*   User authenticated.

**Execution Sequence**:
1.  **[Handler]** Calls `UsageReporter.GetCurrentUsage(ctx, orgID)`.
2.  **[Reporter]** Fetches static plan limits:
    *   **[DB]** `SELECT plan_limits FROM organizations WHERE id = $1`.
3.  **[Reporter]** Fetches API Usage (Cached/Fast):
    *   **[DB]** `SELECT api_calls_count FROM rate_limits WHERE organization_id = $1`.
4.  **[Reporter]** Fetches Active WatchPoints (**Direct Count**):
    *   **[DB]** `SELECT COUNT(*) FROM watchpoints WHERE organization_id = $1 AND status != 'archived'`.
    *   *Rationale*: Avoids drift risks of cached counters; `idx_wp_org_status` ensures speed.
5.  **[Reporter]** Calculates percentages and constructing `UsageSnapshot`.
6.  **[Handler]** Returns JSON.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Strict Direct Count policy applied).

---

## INFO-004: Usage History Retrieval (Historical + Live)

**Primary Components**: `api` (`UsageHandler`), `internal/billing` (`UsageReporter`), `internal/db`

**Trigger**: `GET /v1/usage/history?granularity=monthly`

**Preconditions**:
*   User authenticated.

**Execution Sequence**:
1.  **[Handler]** Parses granularity (default `daily`).
2.  **[Handler]** Calls `UsageReporter.GetUsageHistory(ctx, orgID, start, end, granularity)`.
3.  **[Reporter]** Calls `UsageHistoryRepo.Query`.
4.  **[Repo]** Executes **Aggregation Query** (for Monthly):
    ```sql
    SELECT date_trunc('month', date) as period, 
           SUM(api_calls), MAX(watchpoints) 
    FROM usage_history 
    WHERE organization_id = $1 AND date BETWEEN $2 AND $3
    GROUP BY 1
    ```
5.  **[Reporter]** Executes **Live Data Query**:
    *   Fetches current partial day stats from `rate_limits` and active tables.
6.  **[Reporter]** Performs In-Memory Union:
    *   Appends current partial period to historical list.
7.  **[Handler]** Returns time-series JSON.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Refactoring ensures Granularity support in Repo).

---

## INFO-005: Invoice Retrieval (Stripe Proxy)

**Primary Components**: `api` (`BillingHandler`), `external/stripe` (`BillingService`)

**Trigger**: `GET /v1/billing/invoices?cursor=inv_123`

**Preconditions**:
*   User has `RoleAdmin` or `RoleOwner` (Financial Sensitivity).

**Execution Sequence**:
1.  **[Handler]** Validates RBAC (`RequireRole`).
2.  **[Handler]** Calls `BillingService.GetInvoices(ctx, orgID, params)`.
3.  **[Service]** Fetches Stripe Customer ID from DB.
4.  **[Service]** Calls Stripe API:
    *   Maps `params.Cursor` -> Stripe `starting_after`.
    *   `stripeClient.Invoices.List({Customer: cus_id, Limit: 20})`.
5.  **[Service]** Maps response:
    *   Converts Stripe Invoice objects -> `types.Invoice` domain structs.
    *   Extracts last ID for `PageInfo.NextCursor`.
6.  **[Handler]** Returns JSON list.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (RBAC and Pagination mapping defined).


# Flow Simulations: Informational & Dashboard (INFO/DASH)

> **Range**: INFO-006 to DASH-002
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## INFO-006: Subscription Status Retrieval (Opportunistic Sync)

**Primary Components**: `api` (`BillingHandler`), `external/stripe` (`BillingService`), `internal/db` (`SubscriptionStateRepository`)

**Trigger**: `GET /v1/billing/subscription`

**Preconditions**:
*   User is authenticated.

**Execution Sequence**:
1.  **[Handler]** Calls `BillingService.GetSubscription(ctx, orgID)`.
2.  **[Service]** Fetches local state:
    *   **[DB]** `SELECT plan, subscription_status, last_subscription_event_at FROM organizations WHERE id=$1`.
3.  **[Service]** Fetches remote state:
    *   **[Stripe]** Calls `stripe.Subscriptions.Get(...)` using stored customer ID.
4.  **[Service]** **Opportunistic Sync Logic**:
    *   Compare `remote.Status` vs `local.Status` and `remote.Plan` vs `local.Plan`.
    *   **[Branch - Mismatch]** (e.g., User just paid, webhook pending):
        *   Log info: "Billing state drift detected during read".
        *   **[DB]** `UPDATE organizations SET subscription_status=$remoteStatus, plan=$remotePlan, last_subscription_event_at=NOW() WHERE id=$orgID`.
        *   *Note*: This synchronous update ensures the UI reflects reality immediately.
5.  **[Service]** Returns `SubscriptionDetails` struct (populated from fresh remote data).
6.  **[Handler]** Returns JSON response.

**Data State Changes**:
*   **DB**: `organizations` table updated if drift detected.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Opportunistic Sync logic defined).

---

## INFO-007: Forecast Verification Results Display

**Primary Components**: `api` (`ForecastHandler`), `internal/forecasts` (`ForecastService`), `internal/db` (`VerificationRepository`)

**Trigger**: `GET /v1/forecasts/verification?model=medium_range&start=2026-01-01`

**Preconditions**:
*   Verification pipeline (`OBS-005`) has populated `verification_results`.

**Execution Sequence**:
1.  **[Handler]** Parses query params: `Model` (required), `Start/End` (optional, default 30 days).
2.  **[Handler]** Calls `ForecastService.GetVerificationMetrics(ctx, model, start, end)`.
3.  **[Service]** Calls `VerificationRepo.GetAggregatedMetrics(ctx, ...)`.
4.  **[Repo]** Executes **Aggregation Query**:
    ```sql
    SELECT metric_type, variable, AVG(value) as avg_val, DATE_TRUNC('day', computed_at) as day
    FROM verification_results
    WHERE model = $1 AND computed_at BETWEEN $2 AND $3
    GROUP BY 1, 2, 4
    ORDER BY day ASC;
    ```
5.  **[Service]** Maps rows to `VerificationReport` struct (organized by variable).
6.  **[Handler]** Returns JSON response.

**Data State Changes**:
*   None (Read-only).

**Gap Analysis**:
*   None. (Addressed by Refactoring: New Endpoint and Repository interface).

---

## INFO-008: WatchPoint Evaluation State Retrieval

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`, `internal/forecasts`

**Trigger**: `GET /v1/watchpoints/{id}`

**Preconditions**:
*   User owns WatchPoint.

**Execution Sequence**:
1.  **[Handler]** Calls `Repo.GetByID` (Fetch Config).
2.  **[Handler]** Calls `Repo.GetEvaluationState` (Fetch History).
    *   **[DB]** `SELECT * FROM watchpoint_evaluation_state WHERE watchpoint_id=$1`.
    *   Retrieves `last_evaluated_at`, `trigger_value`, `escalation_level`.
3.  **[Handler]** Calls `ForecastProvider.GetSnapshot` (Fetch Real-Time Context).
    *   **[Service]** Queries S3 Zarr for current conditions.
    *   *Resilience*: If S3 fails/times out, returns `nil`.
4.  **[Handler]** Combines into `WatchPointDetail` response.
    *   `CurrentForecast`: Live data (from step 3).
    *   `EvaluationState`: Persistent state (from step 2).
5.  **[Handler]** Returns JSON.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None.

---

## DASH-001: Session-Based Authentication (Login)

**Primary Components**: `api` (`AuthHandler`), `internal/auth` (`AuthService`), `internal/db` (`SessionRepository`)

**Trigger**: `POST /auth/login`

**Preconditions**:
*   Email/Password provided.

**Execution Sequence**:
1.  **[Handler]** Validates inputs. Checks Brute Force limits (`SEC-001`).
2.  **[Handler]** Calls `AuthService.Login(ctx, email, password)`.
3.  **[Service]** Verifies credentials against `users` table.
4.  **[Service]** **Lazy Session Cleanup** (Maintenance):
    *   **[DB]** `DELETE FROM sessions WHERE user_id = $uid AND expires_at < NOW()`.
    *   *Rationale*: Prevents table bloat during the write transaction.
5.  **[Service]** Creates new Session.
    *   Generates `SessionID` (random 32 bytes) and `CSRFToken` (random 32 bytes).
    *   **[DB]** `INSERT INTO sessions ...`.
6.  **[Handler]** Sets **HTTP-Only Cookie**: `session_id`.
    *   Secure, SameSite=Lax, Path=/.
7.  **[Handler]** Returns JSON payload:
    *   `user`: Profile data.
    *   `csrf_token`: The token string (client must send this in `X-CSRF-Token` header for mutations).

**Data State Changes**:
*   **DB**: New session row. Old expired sessions removed.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Cleanup logic and CSRF token return).

---

## DASH-002: Session-Based Authentication (OAuth JIT)

**Primary Components**: `api` (`AuthHandler`), `internal/auth` (`AuthService`), `internal/db`

**Trigger**: `GET /auth/oauth/google/callback`

**Preconditions**:
*   Valid `oauth_state` cookie present.

**Execution Sequence**:
1.  **[Handler]** Validates state parameter.
2.  **[Handler]** Calls `OAuthProvider.Exchange` to get Profile.
3.  **[Handler]** Calls `AuthService.LoginWithOAuth(ctx, profile)`.
4.  **[Service]** Checks if user exists by email.
5.  **[Branch - New User]** (JIT Provisioning):
    *   **[Service]** Starts Transaction.
    *   **[DB]** Creates Organization (Free Plan).
    *   **[DB]** Creates User (Role Owner, Linked Provider).
    *   **[DB]** Lazy Session Cleanup.
    *   **[DB]** Creates Session.
    *   **[Service]** Commits Transaction.
6.  **[Branch - Existing User]**:
    *   **[Service]** Validates provider link (rejects mismatches).
    *   **[Service]** Lazy Cleanup & Session Create.
7.  **[Handler]** Sets Session Cookie.
8.  **[Handler]** Redirects to Dashboard (with `csrf_token`? No, standard pattern is to redirect to UI, UI calls `/auth/me` to get CSRF token using the cookie. Assumption: Dashboard bootstraps via `/auth/me`).

**Data State Changes**:
*   **DB**: Org/User created (if new). Session created.

**Gap Analysis**:
*   None. (Addressed by Refactoring: JIT Provisioning logic).


# Flow Simulations: Dashboard & UI (DASH) - Continued

> **Range**: DASH-003 to DASH-007
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## DASH-003: Session Refresh (Sliding Window)

**Primary Components**: `api` (`AuthMiddleware`), `internal/db`

**Trigger**: API Request with valid session cookie

**Preconditions**:
*   Session is valid but approaching expiry (or `last_activity_at` is old).

**Execution Sequence**:
1.  **[Middleware]** `AuthMiddleware` validates session ID from cookie.
2.  **[Middleware]** Checks `session.LastActivityAt` against `NOW()`.
3.  **[Branch - Fresh]** (e.g., 5 mins ago):
    *   No DB write. Proceed to handler.
4.  **[Branch - Stale]** (e.g., > 1 hour ago):
    *   **[DB]** Executes Optimized Update:
        ```sql
        UPDATE sessions
        SET last_activity_at = NOW(), expires_at = NOW() + INTERVAL '7 days'
        WHERE id = $sessionID
          AND last_activity_at < NOW() - INTERVAL '1 hour';
        ```
    *   **[Logic]** If `RowsAffected > 0`, re-issue cookie.
    *   **[Middleware]** Sets `Set-Cookie` header with new 7-day expiry.

**Data State Changes**:
*   **DB**: `sessions` timestamps updated (conditionally).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Sliding window logic).

---

## DASH-004: Session Logout

**Primary Components**: `api` (`AuthHandler`), `internal/auth` (`SessionService`), `internal/db`

**Trigger**: `POST /auth/logout`

**Preconditions**:
*   User is logged in.

**Execution Sequence**:
1.  **[Handler]** Calls `SessionService.InvalidateSession(ctx, sessionID)`.
2.  **[Service]** Executes Hard Delete:
    *   **[DB]** `DELETE FROM sessions WHERE id = $sessionID`.
3.  **[Handler]** Clears Client Cookie (Security Requirement).
    *   Sets header: `Set-Cookie: session_id=; Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Path=/; HttpOnly; Secure`.
4.  **[Handler]** Returns `200 OK`.

**Data State Changes**:
*   **DB**: Session row deleted.
*   **Client**: Cookie removed.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit cookie clearing).

---

## DASH-005: Stripe Checkout Flow (Self-Healing)

**Primary Components**: `api` (`BillingHandler`), `external/stripe` (`BillingService`), `internal/db`

**Trigger**: `POST /v1/billing/checkout-session`

**Preconditions**:
*   User authenticated as Admin/Owner.

**Execution Sequence**:
1.  **[Handler]** Decodes request (`plan` tier).
2.  **[Handler]** Validates `plan != 'free'` (Downgrades handled via portal). Returns 400 if invalid.
3.  **[Handler]** Calls `BillingService.EnsureCustomer(ctx, orgID, email)` (Self-Healing).
    *   *Scenario*: User signup failed Stripe step previously. `stripe_customer_id` is NULL.
    *   **[Service]** Creates Customer in Stripe.
    *   **[DB]** Updates `organizations`.
4.  **[Handler]** Constructs **Hardcoded URLs** (Security):
    *   `SuccessURL = cfg.DashboardURL + "/billing?success=true"`.
    *   `CancelURL = cfg.DashboardURL + "/billing?canceled=true"`.
5.  **[Handler]** Calls `BillingService.CreateCheckoutSession`.
6.  **[Stripe]** API creates session.
7.  **[Handler]** Returns `checkout_url`.

**Data State Changes**:
*   **DB**: `organizations` potentially updated (self-healing).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Self-healing pattern, Open Redirect prevention).

---

## DASH-006: Stripe Customer Portal (Self-Healing)

**Primary Components**: `api` (`BillingHandler`), `external/stripe`

**Trigger**: `POST /v1/billing/portal-session`

**Preconditions**:
*   User authenticated.

**Execution Sequence**:
1.  **[Handler]** Calls `BillingService.EnsureCustomer` (Self-Healing logic same as DASH-005).
    *   *Rationale*: Even free users need a customer ID to view empty invoice history in portal.
2.  **[Handler]** Constructs **Hardcoded Return URL**:
    *   `ReturnURL = cfg.DashboardURL + "/billing"`.
3.  **[Handler]** Calls `BillingService.CreatePortalSession`.
4.  **[Stripe]** API creates session.
5.  **[Handler]** Returns `portal_url`.

**Data State Changes**:
*   **DB**: `organizations` potentially updated.

**Gap Analysis**:
*   None.

---

## DASH-007: Real-Time Notification Status (Deferred)

**Primary Components**: N/A

**Trigger**: N/A

**Execution Sequence**:
*   **Decision**: This flow is explicitly **deferred** for v1.
*   **Rationale**: Implementing WebSockets requires significant infrastructure complexity (Connection Management, $connect/$disconnect routes). 
*   **Alternative**: The Dashboard will rely on standard **Polling** of the `INFO-002` (Notification History) endpoint (e.g., every 30s) to update the notification feed.

**Gap Analysis**:
*   None. (Decision logged).


# Flow Simulations: Dashboard & UI (DASH) - Continued

> **Range**: DASH-008
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## DASH-008: Dashboard Stats & List Views (Optimized)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db` (`WatchPointRepository`)

**Trigger**: Dashboard Load (User logs in or refreshes)

**Preconditions**:
*   User is authenticated.
*   Organization has 500+ WatchPoints (Load test scenario).

**Phase 1: Aggregated Stats (Badges)**
1.  **[Frontend]** Calls `GET /v1/watchpoints/stats`.
2.  **[Handler]** Calls `Repo.GetStats(ctx, orgID)`.
3.  **[DB]** Executes Optimized Aggregation:
    ```sql
    SELECT 
        count(*) FILTER (WHERE status = 'active') as active,
        count(*) FILTER (WHERE status = 'paused') as paused,
        count(*) FILTER (WHERE status = 'error') as error,
        (SELECT count(*) FROM watchpoint_evaluation_state wes 
         JOIN watchpoints wp ON wes.watchpoint_id = wp.id 
         WHERE wp.organization_id = $1 AND wes.previous_trigger_state = true) as triggered
    FROM watchpoints
    WHERE organization_id = $1 AND deleted_at IS NULL;
    ```
4.  **[Handler]** Returns JSON: `{ "active": 450, "paused": 50, "error": 2, "triggered": 12 }`.

**Phase 2: Main List (Grid View)**
1.  **[Frontend]** Calls `GET /v1/watchpoints?limit=50&mode=summary`.
2.  **[Handler]** Calls `Repo.ListSummaries(ctx, orgID, filter)`.
3.  **[DB]** Executes Lightweight Join:
    ```sql
    SELECT wp.id, wp.name, wp.location_display_name, wp.status,
           wes.last_evaluated_at, wes.previous_trigger_state, wes.last_forecast_run
    FROM watchpoints wp
    LEFT JOIN watchpoint_evaluation_state wes ON wp.id = wes.watchpoint_id
    WHERE wp.organization_id = $1 AND wp.deleted_at IS NULL
    ORDER BY wp.updated_at DESC
    LIMIT 50;
    ```
    *Note*: Explicitly excludes `conditions`, `channels`, `monitor_config` (Heavy JSONB).
4.  **[Repo]** Maps rows to `types.WatchPointSummary` structs.
5.  **[Handler]** Returns `ListResponse[WatchPointSummary]`.

**Data State Changes**:
*   None (Read-only).

**Gap Analysis**:
*   None. (Addressed by Refactoring: `WatchPointSummary` DTO, `ListSummaries` method, `GetStats` method).


# Flow Simulations: Webhook Operations (HOOK)

> **Range**: HOOK-001 to HOOK-004
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## HOOK-001: Webhook Secret Rotation (Zero Downtime)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db` (`WatchPointRepository`)

**Trigger**: `POST /v1/watchpoints/{id}/channels/{channel_id}/rotate-secret`

**Preconditions**:
*   WatchPoint exists. Channel ID exists within `channels` array.
*   User has write permissions.

**Execution Sequence**:
1.  **[Handler]** Validates WatchPoint ownership.
2.  **[Handler]** Generates `NewSecret` (32-byte hex) and `GracePeriodEnd` (Now + 24h).
3.  **[Handler]** Calls `Repo.UpdateChannelConfig(ctx, wpID, channelID, mutationFn)`.
4.  **[DB]** Executes Atomic JSONB Update:
    ```sql
    UPDATE watchpoints
    SET channels = (
      SELECT jsonb_agg(
        CASE 
          WHEN elem->>'id' = $channelID THEN 
            jsonb_set(
              jsonb_set(elem, '{config,secret}', to_jsonb($newSecret::text)),
              '{config,previous_secret}', 
              elem->'config'->'secret'
            ) || jsonb_build_object('config', (elem->'config') || jsonb_build_object('previous_secret_expires_at', $graceEnd))
          ELSE elem 
        END
      )
      FROM jsonb_array_elements(channels) elem
    )
    WHERE id = $wpID AND organization_id = $orgID;
    ```
    *Note*: Does **NOT** increment `config_version` (separation of concerns).
5.  **[Handler]** Emits `watchpoint.secret_rotated` to Audit Log.
6.  **[Handler]** Returns `200 OK` with `{ "new_secret": "...", "valid_until": "..." }`.

**Data State Changes**:
*   **DB**: `watchpoints.channels` JSONB updated.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Channel ID, `UpdateChannelConfig`, Config Version Trigger logic).

---

## HOOK-002: Webhook URL Update (Safe Update)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: `PATCH /v1/watchpoints/{id}` with new URL in channel config.

**Preconditions**:
*   Request contains new URL for existing Channel ID.

**Execution Sequence**:
1.  **[Handler]** Decodes request.
2.  **[Handler]** Calls `Validator.ValidateStruct`.
    *   **[Validator]** Checks `ssrf_url` tag. Resolves DNS (500ms timeout). Checks blocklist.
3.  **[Handler]** Calls `PlatformRegistry.CheckDeprecation(newUrl)`.
    *   *Result*: `(warning="Teams Connector deprecated...", isDeprecated=true)`.
4.  **[Handler]** Proceed with Update.
    *   Calls `Repo.UpdateChannelConfig` (if partial) or standard `Update`.
5.  **[Handler]** Constructs Response.
    *   Injects warning into `APIResponse.Meta.Warnings` list.
6.  **[Handler]** Returns `200 OK`.

**Data State Changes**:
*   **DB**: Webhook URL updated.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Validation result handling).

---

## HOOK-003: Webhook Signature Verification (Customer-Side Documentation)

**Primary Components**: Documentation / Client Implementation

**Trigger**: Customer receiving a webhook POST from WatchPoint.

**Preconditions**:
*   Customer has the `secret` (and optionally `previous_secret` during rotation).

**Flow Logic**:
1.  **[Customer]** Receives HTTP POST.
2.  **[Customer]** reads `X-Watchpoint-Signature` header.
    *   Format: `t=1706745600,v1=abc...,v1_old=def...`
3.  **[Customer]** Extracts timestamp `t`. Verifies `abs(now - t) < 5 minutes`.
4.  **[Customer]** Constructs signed payload string: `{timestamp}.{http_body}`.
5.  **[Customer]** Computes HMAC-SHA256 of string using `secret`.
6.  **[Customer]** Compares computed hash vs `v1` value.
    *   If match: Accept.
    *   If no match AND `v1_old` exists: Compute using `previous_secret` and compare.
    *   If no match: Reject (401).

**Gap Analysis**:
*   None. (Matches `SignatureManager` implementation).

---

## HOOK-004: Platform-Specific Webhook Deprecation Warning

**Primary Components**: `api` (`WatchPointHandler`), `worker/webhook` (`PlatformRegistry`)

**Trigger**: `GET /v1/watchpoints/{id}` or `GET /v1/watchpoints`

**Preconditions**:
*   WatchPoint contains a webhook URL matching a deprecated pattern (e.g., `outlook.office.com/webhook/...`).

**Execution Sequence**:
1.  **[Handler]** Loads WatchPoint(s) from DB.
2.  **[Handler]** Iterates through `Channels` array.
3.  **[Handler]** For each webhook channel, calls `PlatformRegistry.CheckDeprecation(url)`.
    *   **[Registry]** Regex check: `if url contains 'outlook.office.com' return warning`.
4.  **[Handler]** Aggregates warnings.
    *   e.g., `"Channel 'Teams Alert' uses a deprecated Office 365 Connector. Migrate to Workflow by March 2026."`
5.  **[Handler]** Injects into `APIResponse.Meta.Warnings`.
6.  **[Frontend]** Displays warning banner on Dashboard configuration page.

**Data State Changes**:
*   None (Read-only).

**Gap Analysis**:
*   None. (Addressed by Refactoring: `Meta.Warnings` field)."


# Flow Simulations: Webhook Operations (HOOK) - Continued

> **Range**: HOOK-005
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## HOOK-005: Webhook Delivery Confirmation Tracking (Success Metric)

**Primary Components**: `worker/webhook`, `internal/db` (`ChannelHealthRepo`), `CloudWatch`

**Trigger**: Successful HTTP 200 OK response from Webhook destination.

**Preconditions**:
*   `Deliver` method executed successfully.
*   Channel config contains `id` (ChannelUUID).

**Execution Sequence**:
1.  **[Worker]** Receives HTTP 200.
2.  **[Worker]** Calls `NotificationMetrics.RecordDelivery(ctx, type="webhook", result="success")`.
    *   **[CloudWatch]** Emits `DeliverySuccess` metric with dimensions `{ChannelType: "webhook", Source: "default"}`.
3.  **[Worker]** **Lazy Reset Logic**:
    *   Checks in-memory `Channel` state passed from `NotificationMessage`.
    *   *Scenario A (FailureCount > 0)*: Channel was previously unhealthy.
        *   **[DB]** Calls `ChannelHealthRepo.ResetChannelFailureCount`.
        *   `UPDATE watchpoints SET channels = ... (reset failure_count=0) WHERE id=$wpID`.
    *   *Scenario B (FailureCount == 0)*: Channel already healthy.
        *   **[Logic]** Skip DB write. (Optimization).

**Data State Changes**:
*   **DB**: WatchPoint channel stats reset (conditionally).
*   **CloudWatch**: Metric emitted.

**Gap Analysis**:
*   None. (Lazy Reset reduces write amplification significantly).


# Flow Simulations: Vertical App Integration (VERT)

> **Range**: VERT-001 to VERT-004
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## VERT-001: Vertical App API Key Provisioning

**Primary Components**: `api` (`APIKeyHandler`), `internal/db`

**Trigger**: `POST /v1/api-keys`

**Preconditions**:
*   User is Admin/Owner.
*   Request body includes `source: "wedding_app"`.

**Execution Sequence**:
1.  **[Handler]** Decodes request. Validates `source` string format (alphanumeric).
2.  **[Handler]** Generates Secret and Hash.
3.  **[Handler]** Calls `Repo.Create`.
4.  **[DB]** Executes:
    ```sql
    INSERT INTO api_keys (
        id, organization_id, key_hash, key_prefix, scopes, source, created_at
    ) VALUES (
        'key_...', $orgID, $hash, 'sk_live_...', $scopes, 'wedding_app', NOW()
    );
    ```
5.  **[Handler]** Returns secret (one-time).

**Data State Changes**:
*   **DB**: New API Key row with `source` populated.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `source` column).

---

## VERT-002: WatchPoint Creation from Vertical App (Attribution)

**Primary Components**: `api` (`AuthMiddleware`, `WatchPointHandler`), `internal/db`

**Trigger**: `POST /v1/watchpoints` using the "wedding_app" API Key.

**Preconditions**:
*   API Key from VERT-001 exists.

**Execution Sequence**:
1.  **[AuthMiddleware]** Resolves Token.
    *   **[DB]** `SELECT ..., source FROM api_keys WHERE key_hash=$1 ...`.
    *   Result: `source='wedding_app'`.
2.  **[AuthMiddleware]** Populates `Actor`: `{ ID: ..., Source: "wedding_app" }`.
3.  **[Handler]** Processes Create Request.
4.  **[Logic]** Injects Source:
    *   `WatchPoint.Source = Actor.Source` ("wedding_app").
    *   *Note*: Ignores any source field in the JSON body (security).
5.  **[Handler]** Calls `Repo.Create`.
    *   **[DB]** `INSERT INTO watchpoints (..., source) VALUES (..., 'wedding_app')`.
6.  **[Handler]** Returns 201 Created.

**Data State Changes**:
*   **DB**: WatchPoint created with immutable attribution.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Immutable source logic).

---

## VERT-003: Template Set Selection and Rendering (Resilient)

**Primary Components**: `worker/email`, `internal/notification/core`

**Trigger**: `NOTIF-001` (Email Delivery)

**Preconditions**:
*   WatchPoint has `template_set="wedding"`.
*   `EmailConfig` has `Templates` JSON loaded.

**Execution Sequence**:
1.  **[Worker]** Extracts `template_set` from Notification.
2.  **[Worker]** Calls `TemplateEngine.Render(ctx, "wedding", EventType)`.
3.  **[Engine]** Lookups template ID in config.
    *   *Scenario A (Success)*: Found ID "d-wedding123". Renders.
    *   *Scenario B (Failure/Missing)*: Template set "wedding" not found in config.
4.  **[Engine - Soft Fail]**:
    *   Logs warning: "Template set 'wedding' not found, falling back to default".
    *   Lookups "default".
    *   Renders using default ID.
5.  **[Worker]** Proceeds with delivery using the rendered content.

**Data State Changes**:
*   None (Log only).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Soft-fail fallback logic).

---

## VERT-004: Vertical App Usage Attribution

**Primary Components**: `cmd/archiver`, `internal/billing` (`UsageAggregator`), `internal/db`

**Trigger**: `SCHED-002` (Daily Usage Aggregation)

**Preconditions**:
*   `rate_limits` table contains rows for multiple sources (e.g., `(org1, 'default')`, `(org1, 'wedding_app')`).

**Execution Sequence**:
1.  **[Archiver]** Calls `UsageAggregator.SnapshotDailyUsage`.
2.  **[Aggregator]** Queries stale rate limits.
    *   **[DB]** `SELECT organization_id, source, api_calls_count FROM rate_limits WHERE period_end < $now`.
3.  **[Aggregator]** Iterates results. For each `(Org, Source)` tuple:
    *   **[DB]** Inserts History:
        ```sql
        INSERT INTO usage_history (organization_id, date, source, api_calls)
        VALUES ($org, $date, $source, $count)
        ON CONFLICT (organization_id, date, source) DO NOTHING;
        ```
    *   **[DB]** Resets Rate Limit Row:
        ```sql
        UPDATE rate_limits
        SET api_calls_count = 0, period_end = ...
        WHERE organization_id = $org AND source = $source;
        ```
4.  **[Aggregator]** Logs metric `UsageSnapshotCreated`.

**Data State Changes**:
*   **DB**: Granular usage history created per source. Rate limits reset per source.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Composite primary keys on usage tables)."
}


# Flow Simulations: Validation & Security (VALID/SEC)

> **Range**: VALID-001 to SEC-004
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## VALID-001: Input Validation Pipeline

**Primary Components**: `api` (`WatchPointHandler`, `Validator`), `01-foundation-types`

**Trigger**: `POST /v1/watchpoints` with invalid payload (e.g., conflicting modes)

**Preconditions**:
*   Request body contains both `time_window` AND `monitor_config`.

**Execution Sequence**:
1.  **[Handler]** Decodes JSON into `CreateWatchPointRequest` DTO.
2.  **[Handler]** Calls `Validator.ValidateStruct(req)`.
3.  **[Validator]** Evaluates Struct Tags via `go-playground/validator`:
    *   Checks `TimeWindow`: tag `excluded_with=MonitorConfig`.
    *   Checks `MonitorConfig`: tag `excluded_with=TimeWindow`.
    *   *Result*: Verification fails due to mutual exclusivity violation.
4.  **[Handler]** Receives validation errors.
5.  **[Handler]** Constructs `APIErrorResponse`.
    *   Code: `validation_conflicting_modes`.
    *   Message: "Cannot specify both TimeWindow and MonitorConfig".
6.  **[Handler]** Returns `400 Bad Request`.

**Data State Changes**:
*   None. (Request rejected before DB access).

**Gap Analysis**:
*   None. (Addressed by Refactoring: DTO validation strategy confirmed).

---

## SEC-001: Brute Force Protection (Login)

**Primary Components**: `api` (`AuthHandler`), `internal/security` (`SecurityService`), `internal/db`

**Trigger**: `POST /auth/login` (Failed Attempt)

**Preconditions**:
*   Attacker is guessing passwords for `target@example.com`.

**Execution Sequence**:
1.  **[Handler]** Normalizes email.
2.  **[Handler]** Calls `SecurityService.IsIdentifierBlocked(ctx, email)`.
    *   **[DB]** `SELECT count(*) FROM security_events WHERE identifier=$1 AND event_type='login' AND success=false AND attempted_at > NOW() - INTERVAL '15m'`.
    *   *Result*: 0 (Not blocked yet).
3.  **[Handler]** Calls `AuthService.Login`.
4.  **[AuthService]** Verifies hash. Fails.
5.  **[AuthService]** Calls `SecurityService.RecordAttempt(ctx, 'login', email, ip, false)`.
    *   **[DB]** `INSERT INTO security_events (event_type, identifier, ip_address, success, failure_reason) VALUES ('login', $email, $ip, false, 'invalid_creds')`.
6.  **[Handler]** Returns `401 Unauthorized`.
7.  **[Loop]** Attacker repeats 5 times.
8.  **[Handler]** 6th Request: Calls `IsIdentifierBlocked`.
    *   **[DB]** Query returns 5.
    *   **[Service]** Returns `true`.
9.  **[Handler]** Returns `429 Too Many Requests` ("Account locked temporarily").

**Data State Changes**:
*   **DB**: Rows inserted into `security_events`.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Unified `security_events` table).

---

## SEC-002: Brute Force Protection (API Key Enumeration)

**Primary Components**: `api` (`IPSecurityMiddleware`), `internal/security` (`SecurityService`), `internal/auth` (`Authenticator`)

**Trigger**: API Request with invalid `Authorization: Bearer sk_live_invalid`

**Preconditions**:
*   Attacker is cycling random keys from one IP.

**Execution Sequence**:
1.  **[IPSecurityMiddleware]** Intercepts request.
2.  **[Middleware]** Calls `SecurityService.IsIPBlocked(ctx, ip)`.
    *   **[DB]** `SELECT count(*) FROM security_events WHERE ip_address=$1 AND event_type='api_auth' AND success=false ...`.
    *   *Result*: False.
3.  **[AuthMiddleware]** Calls `Authenticator.ResolveToken`.
4.  **[Authenticator]** Hashes token. Checks DB. Not found.
5.  **[AuthMiddleware]** Calls `SecurityService.RecordAttempt(ctx, 'api_auth', NULL, ip, false)`.
    *   **[DB]** `INSERT INTO security_events (event_type, ip_address, success, failure_reason) VALUES ('api_auth', $ip, false, 'token_invalid')`.
6.  **[Middleware]** Returns `401 Unauthorized`.
7.  **[Loop]** Attacker repeats 50 times.
8.  **[IPSecurityMiddleware]** 51st Request: Calls `IsIPBlocked`.
    *   **[DB]** Query returns > Threshold.
    *   **[Service]** Returns `true`.
9.  **[Middleware]** Returns `403 Forbidden` (Block at edge).

**Data State Changes**:
*   **DB**: Rows inserted into `security_events`.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `IPSecurityMiddleware` and `security_events` table).

---

## SEC-003: API Key Compromise Response

**Primary Components**: `api` (`APIKeyHandler`), `internal/db`

**Trigger**: Admin revoking compromised key `sk_live_abc123...`

**Preconditions**:
*   Admin knows key prefix or name.

**Execution Sequence**:
1.  **[Admin]** Calls `GET /v1/api-keys?prefix=sk_live_abc`.
2.  **[Handler]** Calls `Repo.List(ctx, {Prefix: "sk_live_abc"})`.
3.  **[DB]** `SELECT * FROM api_keys WHERE key_prefix LIKE 'sk_live_abc%'`.
4.  **[Handler]** Returns list containing Target Key ID (`key_target_1`).
5.  **[Admin]** Calls `DELETE /v1/api-keys/key_target_1`.
6.  **[Handler]** Calls `Repo.Delete`.
7.  **[DB]** `UPDATE api_keys SET revoked_at = NOW() WHERE id = 'key_target_1'`.
8.  **[Handler]** Returns `204 No Content`.

**Data State Changes**:
*   **DB**: `api_keys.revoked_at` set.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Prefix filtering support in List endpoint).

---

## SEC-004: Suspicious Activity Detection (Rapid Creation)

**Primary Components**: `api` (`APIKeyHandler`), `CloudWatch`

**Trigger**: Compromised User Account creates 10 keys in 1 minute

**Phase 1: Proactive Limit (Application Layer)**
1.  **[Handler]** Calls `Repo.CountRecent(ctx, userID, 1h)`.
    *   **[DB]** `SELECT count(*) FROM api_keys WHERE created_by_user_id=$1 AND created_at > NOW() - INTERVAL '1 hour'`.
2.  **[Logic]** Count = 5 (Limit).
3.  **[Handler]** Returns `429 Too Many Requests`.
    *   *Result*: Attack slowed/stopped.

**Phase 2: Reactive Alerting (Infrastructure Layer)**
1.  **[Handler]** For the first 5 successful creates: Emits `apikey.created` to Audit Log.
2.  **[CloudWatch Logs]** Ingests JSON log entry.
3.  **[Metric Filter]** `SecurityAbuseMetric` matches `{ $.action = "apikey.created" }`.
4.  **[CloudWatch Alarm]** `SuspiciousKeyCreationAlarm` fires (Threshold > 5 in 5 min).
5.  **[SNS]** Sends alert to Ops.

**Data State Changes**:
*   **CloudWatch**: Metric datapoints recorded.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Proactive 429 limit and Metric Filter).


# Flow Simulations: Security & Test Mode (SEC/TEST)

> **Range**: SEC-005 to TEST-003
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## SEC-005: Input Sanitization (Embedded)

**Primary Components**: `api` (`SecurityHeadersMiddleware`, `DecodeJSON`), `internal/db`

**Trigger**: Incoming API Request (e.g., `POST /v1/watchpoints`)

**Preconditions**:
*   Request contains potentially malicious payload (e.g., `<script>alert(1)</script>` in `name`).

**Execution Sequence**:
1.  **[Middleware]** `SecurityHeadersMiddleware` intercepts request.
2.  **[Middleware]** Sets response headers to prevent browser execution of reflected inputs:
    *   `X-Content-Type-Options: nosniff`
    *   `X-XSS-Protection: 1; mode=block`
    *   `Content-Security-Policy: default-src 'none'` (API only)
3.  **[Handler]** Calls `core.DecodeJSON(w, r, &struct)`.
    *   **[Logic]** Enforces strict types. Disallows unknown fields.
    *   *Note*: Does **not** HTML-escape the string content. The string is stored as-is.
4.  **[Handler]** Calls `Repo.Create`.
5.  **[DB]** Uses **Parameterized Query** (`pgx` driver):
    *   `INSERT INTO watchpoints (name...) VALUES ($1...)`
    *   *Result*: SQL Injection prevented by driver; string stored literally.
6.  **[Dashboard/Client]** (Out of scope of API, but architectural assumption): Responsible for Context-Aware Output Encoding when rendering `name`.

**Data State Changes**:
*   **DB**: Data stored verbatim without sanitization mutation.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Security Headers Middleware).

---

## SEC-006: Webhook Payload Signing (Outbound Security)

**Primary Components**: `worker/webhook`, `internal/notification/core` (`SignatureManager`)

**Trigger**: `WebhookChannel.Deliver` execution

**Preconditions**:
*   Channel config contains `secret`, `previous_secret`, and `previous_secret_expires_at`.

**Execution Sequence**:
1.  **[Worker]** Prepares JSON payload (bytes).
2.  **[Worker]** Calls `SignatureManager.SignPayload(payload, config, now)`.
3.  **[Manager]** Generates `timestamp` (Unix int).
4.  **[Manager]** Computes `v1` Signature:
    *   `HMAC-SHA256(secret, "$timestamp.$payload")`.
5.  **[Manager]** Checks Rotation Grace Period:
    *   Parse `previous_secret` and `previous_secret_expires_at`.
    *   **[Logic]** If `previous_secret` exists AND `now < previous_secret_expires_at`:
        *   Computes `v1_old` Signature.
        *   Formats header: `t=$ts,v1=$sig,v1_old=$old_sig`.
    *   **[Logic]** If expired or missing:
        *   Formats header: `t=$ts,v1=$sig`.
6.  **[Worker]** Sets `X-Watchpoint-Signature` header on HTTP request.
7.  **[Worker]** Executes POST.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Expiration logic in SignatureManager).

---

## TEST-001: Test Mode API Behavior (Propagation)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: `POST /v1/watchpoints` using `sk_test_...` key

**Preconditions**:
*   `AuthMiddleware` has set `Actor.IsTestMode = true`.

**Execution Sequence**:
1.  **[Handler]** Decodes request body.
2.  **[Handler]** Overrides `WatchPoint.TestMode`:
    *   Logic: `wp.TestMode = actor.IsTestMode`.
    *   *Result*: Ignores any `test_mode` field sent in the JSON body by the client.
3.  **[Handler]** Calls `UsageEnforcer`.
    *   **[Logic]** Test resources **DO** count against plan limits (to allow users to test limit logic).
4.  **[Handler]** Calls `Repo.Create`.
5.  **[DB]** `INSERT INTO watchpoints (..., test_mode) VALUES (..., true)`.
6.  **[Handler]** Returns `201 Created`.

**Data State Changes**:
*   **DB**: WatchPoint created with `test_mode=true`.

**Gap Analysis**:
*   None.

---

## TEST-002: Test Mode Notification Handling

**Primary Components**: `worker/email` (or `worker/webhook`), `internal/db`

**Trigger**: `Deliver` method invoked for a Test Mode notification

**Preconditions**:
*   `Notification.TestMode` is `true`.

**Execution Sequence**:
1.  **[Worker]** Starts `Deliver`.
2.  **[Worker]** Checks `n.TestMode`.
3.  **[Branch - Test Mode]**:
    *   **[Worker]** Logs: "Test mode: suppressing email delivery".
    *   **[Worker]** Returns `DeliveryResult`:
        *   `Status`: `skipped`
        *   `ProviderMessageID`: `test-simulated-{uuid}`
4.  **[Worker]** **Skips**: Template Rendering (prevents errors if test templates are missing) and Provider Call.
5.  **[Worker]** Updates Database.
    *   **[DB]** `UPDATE notification_deliveries SET status='skipped', provider_message_id='test-simulated-...'`.

**Data State Changes**:
*   **DB**: Delivery marked `skipped`.

**Gap Analysis**:
*   None.

---

## TEST-003: Test Mode Data Isolation

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: `GET /v1/watchpoints`

**Preconditions**:
*   Repository supports `TestMode` filter in query.

**Execution Sequence**:
1.  **[Handler]** Authenticates Actor.
2.  **[Handler]** Constructs `ListWatchPointsParams`.
3.  **[Logic - Actor Branch]**:
    *   **Case A (API Key)**: `Actor.Type == ActorTypeAPIKey`.
        *   Set `params.TestMode = Actor.IsTestMode`.
        *   *Result*: Live keys see ONLY live data; Test keys see ONLY test data.
    *   **Case B (Dashboard User)**: `Actor.Type == ActorTypeUser`.
        *   Parse query param `?test_mode=true|false` (Default `false`).
        *   Set `params.TestMode = queryVal`.
        *   *Result*: Dashboard can toggle views.
4.  **[Handler]** Calls `Repo.List(ctx, params)`.
5.  **[DB]** `SELECT * FROM watchpoints WHERE organization_id=$1 AND test_mode=$2 ...`.
6.  **[Handler]** Returns filtered list.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit branching logic for Actor types).


# Flow Simulations: Testing & Bulk Operations (TEST/BULK)

> **Range**: TEST-004 to BULK-004
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## TEST-004: Test/Live Data Isolation (Dashboard Toggle)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: Dashboard User toggles "View Test Data" (GET request)

**Preconditions**:
*   User authenticated via Session (`ActorTypeUser`).
*   Database contains mix of Test and Live WatchPoints.

**Execution Sequence**:
1.  **[Frontend]** Request `GET /v1/watchpoints?test_mode=true`.
2.  **[Handler]** `AuthMiddleware` resolves Session Actor.
3.  **[Handler]** Checks Actor Type.
    *   *Case API Key*: Ignores query param, forces `filter.TestMode = key.TestMode`.
    *   *Case User*: Reads `test_mode=true` from query, sets `filter.TestMode = true`.
4.  **[Handler]** Calls `Repo.List(ctx, orgID, filter)`.
5.  **[DB]** Executes Query:
    ```sql
    SELECT * FROM watchpoints 
    WHERE organization_id = $1 
      AND test_mode = $2 -- Strict filter
      AND deleted_at IS NULL;
    ```
6.  **[Handler]** Returns only Test Mode resources.

**Data State Changes**:
*   None (Read-only).

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit Actor branching logic).

---

## BULK-001: Bulk WatchPoint Import

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: `POST /v1/watchpoints/bulk`

**Preconditions**:
*   Request contains array of 100 WatchPoints (some with Webhooks).

**Execution Sequence**:
1.  **[Handler]** Validates Batch Size (<= 100).
2.  **[Handler]** Checks Plan Limits (`Current + 100 <= Limit`).
3.  **[Handler]** **Parallel Validation** (using `errgroup`):
    *   Spawns goroutines to run `Validator.ValidateStruct` for each item.
    *   *Critical*: Performs DNS resolution for Webhook URLs in parallel to avoid timeout.
    *   Collects valid items into `valid_batch`.
4.  **[Handler]** Injects `Actor.Source` and `Actor.IsTestMode` into valid items.
5.  **[Handler]** Calls `Repo.CreateBatch(ctx, valid_batch)`.
6.  **[DB]** Executes Bulk Insert:
    ```sql
    INSERT INTO watchpoints (id, organization_id, ..., test_mode) 
    VALUES ($1, $2, ..., $tm), ...;
    ```
7.  **[Logic]** Quiet Create (No immediate eval, `current_forecast: null`).
8.  **[Handler]** Emits `watchpoint.bulk_imported` to Audit Log (Count: 100).
9.  **[Handler]** Returns 200 OK with success/failure breakdown.

**Data State Changes**:
*   **DB**: 100 new rows in `watchpoints`.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Parallel validation).

---

## BULK-002: Bulk Pause

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: `POST /v1/watchpoints/bulk/pause`

**Preconditions**:
*   Filter provided (e.g., `tags: ["project-x"]`).

**Execution Sequence**:
1.  **[Handler]** Calls `Repo.UpdateStatusBatch(ctx, filter, "paused", actor.IsTestMode)`.
2.  **[DB]** Executes:
    ```sql
    UPDATE watchpoints 
    SET status = 'paused' 
    WHERE organization_id = $1 
      AND tags @> $tags
      AND test_mode = $3 -- Strict Isolation
    RETURNING id;
    ```
3.  **[Repo]** Returns list of affected IDs.
4.  **[Handler]** Calls `Repo.CancelDeferredDeliveriesBatch(ctx, ids)`.
    *   **[DB]** `UPDATE notification_deliveries SET status='skipped' WHERE watchpoint_id = ANY($ids) AND status='deferred'`.
5.  **[Handler]** Emits `watchpoint.bulk_paused` audit event (Metadata: `count`, `filter`).
6.  **[Handler]** Returns count.

**Data State Changes**:
*   **DB**: WatchPoints paused. Deferred deliveries skipped.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `test_mode` param in Repo, Deferred cleanup).

---

## BULK-003: Bulk Resume

**Primary Components**: `api`, `internal/db`, `worker/eval`

**Trigger**: `POST /v1/watchpoints/bulk/resume`

**Preconditions**:
*   WatchPoints are paused.

**Execution Sequence**:
1.  **[Handler]** Calls `Repo.UpdateStatusBatch(..., "active", actor.IsTestMode)`.
2.  **[DB]** Updates status, returns IDs.
3.  **[Handler]** Calls `Repo.CancelDeferredDeliveriesBatch(ctx, ids)` (Safety hygiene).
4.  **[Handler]** **Trigger Evaluation Loop**:
    *   Iterates IDs (e.g., 50 items).
    *   Calls `EvaluationTrigger.TriggerEvaluation(ctx, id)`.
    *   **[SQS]** Sends 50 individual `EvalMessage` payloads (Targeted).
    *   *Rationale*: Batching SQS is not possible due to per-tile geometry requirements of Eval Worker.
5.  **[Handler]** Emits `watchpoint.bulk_resumed` audit event.
6.  **[Handler]** Returns count.

**Data State Changes**:
*   **DB**: WatchPoints active.
*   **SQS**: 50 messages enqueued.

**Gap Analysis**:
*   None. (Loop inefficiencies accepted for v1).

---

## BULK-004: Bulk Delete

**Primary Components**: `api`, `internal/db`

**Trigger**: `POST /v1/watchpoints/bulk/delete`

**Preconditions**:
*   `confirm: true` in payload.

**Execution Sequence**:
1.  **[Handler]** Calls `Repo.DeleteBatch(ctx, filter, actor.IsTestMode)`.
2.  **[DB]** Executes Soft Delete:
    ```sql
    UPDATE watchpoints 
    SET status = 'archived', deleted_at = NOW() 
    WHERE organization_id = $1 
      AND test_mode = $2 
      AND ... (filter match)
    RETURNING id;
    ```
3.  **[Logic]** Does not explicitly delete child records (relies on `MAINT-005` or manual cascade if needed, but soft delete effectively removes them from system view).
4.  **[Handler]** Emits `watchpoint.bulk_deleted` audit event.
5.  **[Handler]** Returns count.

**Data State Changes**:
*   **DB**: WatchPoints marked deleted/archived.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit `status='archived'` update)."


# Flow Simulations: Bulk Operations (BULK) - Continued

> **Range**: BULK-005 to BULK-006
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## BULK-005: Bulk Tag Update (Atomic)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: `PATCH /v1/watchpoints/bulk/tags`

**Preconditions**:
*   Request contains `filter` (e.g., `tags: ["project-a"]`), `add_tags: ["archived"]`, `remove_tags: ["active"]`.

**Execution Sequence**:
1.  **[Handler]** Decodes request into `BulkTagUpdateRequest`.
2.  **[Handler]** Calls `Repo.UpdateTagsBatch(ctx, orgID, filter, addTags, removeTags)`.
3.  **[DB]** Executes Atomic Array Update:
    ```sql
    UPDATE watchpoints
    SET tags = (tags || $add_tags) - $remove_tags,
        updated_at = NOW()
    WHERE organization_id = $orgID
      AND tags @> $filter_tags
      AND status = ANY($filter_status)
      AND test_mode = $testMode
    RETURNING id;
    ```
    *Rationale*: Uses Postgres array operators (`||` for concat, `-` for remove) to update in-place without race conditions.
4.  **[DB]** Returns list of updated IDs.
5.  **[Handler]** Emits `watchpoint.bulk_tags_updated` audit event (Metadata: `count`, `ids`).
6.  **[Handler]** Returns `200 OK` with count.

**Data State Changes**:
*   **DB**: `watchpoints.tags` arrays modified in bulk.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `UpdateTagsBatch` method and `BulkFilter` hoist).

---

## BULK-006: Bulk Clone (Time-Shifted)

**Primary Components**: `api` (`WatchPointHandler`), `internal/db`

**Trigger**: `POST /v1/watchpoints/bulk/clone`

**Preconditions**:
*   Payload: `source_ids`, `time_shift` (e.g., +1 year).

**Execution Sequence**:
1.  **[Handler]** Calls `Repo.GetBatch(ctx, sourceIDs, orgID)`.
    *   **[DB]** `SELECT * FROM watchpoints WHERE id = ANY($ids) AND organization_id = $orgID ...`
2.  **[Handler]** Iterates results. For each Source WP:
    *   Generates new UUID.
    *   **[Logic - Monitor Mode]**: If `monitor_config` is present:
        *   Ignores `time_shift`.
        *   Copies configuration as-is.
    *   **[Logic - Event Mode]**: If `time_window` is present:
        *   Applies Timezone-aware shift logic (Start/End + 1 Year).
        *   Validates new window is in future.
        *   If invalid (e.g., past), skip item and add to `failures` list.
    *   Sets `Status='active'`.
3.  **[Handler]** Calls `Repo.CreateBatch(ctx, validItems)`.
    *   **[DB]** Bulk `INSERT`.
4.  **[Handler]** Emits `watchpoint.bulk_cloned` audit event.
5.  **[Handler]** Returns response with `successes` (mapped IDs) and `failures`.

**Data State Changes**:
*   **DB**: New WatchPoint records inserted.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `GetBatch` vectorization and Monitor Mode logic).


# Flow Simulations: Failure & Recovery (FAIL)

> **Range**: FAIL-001 to FAIL-003
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## FAIL-001: Database Primary Unavailable — Automatic Failover

**Primary Components**: `Infrastructure` (AWS RDS/Supabase), `internal/db` (`pgxpool`)

**Trigger**: Hardware failure or maintenance on Primary DB Node.

**Sequence**:
1.  **[Infrastructure]** AWS detects failure. Promotes Read Replica to Primary. Updates DNS.
2.  **[Application]** `pgxpool` detects broken connections on next use.
3.  **[Application]** `HealthCheckPeriod` (configured to 1m) proactively pings idle connections.
    *   *Result*: Stale connections closed/discarded.
4.  **[Application]** New queries trigger new connection attempts.
5.  **[Application]** DNS resolution picks up new Primary IP.
6.  **[Application]** Connection established. System recovers.
7.  **[Impact]** ~60s of HTTP 500/503 errors during DNS propagation.

**Data State Changes**:
*   None (Service interruption).

**Gap Analysis**:
*   None. (Addressed by Refactoring: `HealthCheckPeriod` config).

---

## FAIL-002: Database Unavailable — Queue Writes for Replay

**Primary Components**: `worker/*` (Async Consumers), `api` (Synchronous)

**Trigger**: Total Database Outage (e.g., Region Issue)

**Scenario A: Workers (Async)**
1.  **[Worker]** Eval/Notification Worker attempts DB transaction.
2.  **[Worker]** Fails (Connection Refused/Timeout).
3.  **[Worker]** Returns error to Lambda runtime.
4.  **[AWS Lambda]** Does **not** delete message from SQS.
5.  **[SQS]** Message becomes visible again after Visibility Timeout.
6.  **[Loop]** Retries continue until DB recovers or DLQ limit reached.
    *   *Result*: Writes are implicitly queued/buffered in SQS.

**Scenario B: API (Synchronous)**
1.  **[Handler]** Receives `POST /watchpoints`.
2.  **[Handler]** Calls `Repo.Create`.
3.  **[DB]** `pgx` attempts connection.
4.  **[DB]** Hits `AcquireTimeout` (2s).
5.  **[Handler]** Returns `503 Service Unavailable`.
    *   *Result*: Client must retry. No internal queueing for sync API.

**Data State Changes**:
*   **SQS**: Messages retained (Flight Mode).

**Gap Analysis**:
*   None. (Architecture relies on SQS persistence for workers, Client retry for API).

---

## FAIL-003: Database Connection Exhaustion

**Primary Components**: `internal/db` (`pgxpool`)

**Trigger**: Traffic spike exceeds Supabase Transaction Pool limit (e.g., 500 conns).

**Sequence**:
1.  **[Application]** High concurrency Lambda invocations start.
2.  **[DB Pool]** `pgx` requests connection from local pool.
3.  **[DB Pool]** Local pool empty. Tries to open new connection to Supabase.
4.  **[Supabase]** PgBouncer rejects connection (Max Clients reached).
5.  **[Application]** `pgx` waits for a slot or timeout.
6.  **[Application]** `AcquireTimeout` (2s) expires.
7.  **[Handler]** Returns `503 Service Unavailable` (`ErrInternalDB`).
8.  **[Metric]** `APILatency` shows fast failure (2s) rather than 30s timeout.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Addressed by Refactoring: `AcquireTimeout` prevents Lambda thread exhaustion cascades).


# Flow Simulations: Failure & Recovery (FAIL) - Continued

> **Range**: FAIL-004 to FAIL-008
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## FAIL-004: GFS Data Unavailable — Mirror Failover

**Primary Components**: `cmd/data-poller`, `internal/db`, `internal/forecasts` (`UpstreamSource`)

**Trigger**: `SCHED-00X` (Data Poller Execution)

**Preconditions**:
*   Primary NOAA GFS bucket is inaccessible or empty.
*   `ForecastConfig` contains `UpstreamMirrors` list (e.g., `["noaa-gfs-bdp-pds", "aws-noaa-gfs"]`).

**Execution Sequence**:
1.  **[DataPoller]** Calls `RunRepository.GetLatest` to find last successful run.
2.  **[DataPoller]** Calls `UpstreamSource.CheckAvailability(ctx, lastRun)`.
3.  **[Source Implementation]** Iterates `UpstreamMirrors`:
    *   *Attempt 1*: List Objects on `noaa-gfs-bdp-pds`. Returns Error (404/Connection).
    *   *Log*: Warning "Primary mirror unavailable".
    *   *Attempt 2*: List Objects on `aws-noaa-gfs`.
    *   *Result*: Success. Found new runs.
4.  **[DataPoller]** Proceeds to trigger inference with valid source paths from the secondary mirror.
5.  **[DataPoller]** Logic ensures the `InferencePayload` contains the correct S3 URI for the successful mirror.

**Data State Changes**:
*   None (Read-only failover).

**Gap Analysis**:
*   None. (Addressed by Refactoring: `UpstreamMirrors` config and iteration logic).

---

## FAIL-005: GOES/Radar Data Unavailable — Nowcast Sync Check

**Primary Components**: `cmd/data-poller`

**Trigger**: `SCHED-00X` (Data Poller Execution)

**Preconditions**:
*   GOES data available, but MRMS Radar data is missing/delayed.

**Execution Sequence**:
1.  **[DataPoller]** Detects `nowcast` check required.
2.  **[DataPoller]** Instantiates two `UpstreamSource` clients: `GoesSource` and `MrmsSource`.
3.  **[DataPoller]** Fetches available timestamps from both:
    *   `GoesTS`: `[T1, T2, T3]`
    *   `MrmsTS`: `[T1]` (Lagging)
4.  **[DataPoller]** Executes **Intersection Logic**:
    *   Finds matching pairs within 5-minute tolerance.
    *   Match found: `T1`.
    *   No match for `T2, T3`.
5.  **[DataPoller]** Triggers inference ONLY for `T1`.
6.  **[DataPoller]** Logs/Metrics:
    *   If no intersection found for > 30 mins: `NowcastDataLag` metric emitted.
    *   *Result*: Prevents triggering RunPod with partial data (which would fail inference).

**Data State Changes**:
*   **DB**: `forecast_runs` created only for valid intersection.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit synchronization logic in Poller).

---

## FAIL-006: RunPod Inference Timeout (Hanging Job)

**Primary Components**: `cmd/archiver` (Reconciler), `internal/scheduler`

**Trigger**: `EventBridge Rule: rate(1 hour)` via `reconcile_forecasts` task

**Preconditions**:
*   A Nowcast job stuck in `running` state for 40 minutes.
*   `TimeoutConfig.Nowcast` = 20 minutes.

**Execution Sequence**:
1.  **[Archiver]** Calls `ForecastReconciler.ReconcileStaleRuns`.
2.  **[Reconciler]** Queries `forecast_runs` where `status='running'`.
3.  **[Reconciler]** Iterates runs. Finds Job A (Nowcast).
4.  **[Reconciler]** Checks Duration:
    *   `Now - CreatedAt` = 40m.
    *   Limit = 20m.
    *   *Result*: Timeout Exceeded.
5.  **[Reconciler]** Calls `RunPodClient.CancelJob(ctx, externalID)`.
    *   **[RunPod]** Terminates container (stops billing).
6.  **[Reconciler]** Updates DB:
    *   `UPDATE forecast_runs SET status='failed', failure_reason='Timeout exceeded (20m)' WHERE id=...`
7.  **[Reconciler]** Emits `ForecastTimeout` metric.

**Data State Changes**:
*   **DB**: Run marked failed.
*   **External**: Job cancelled.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Model-specific timeouts and CancelJob).

---

## FAIL-007: RunPod API Unavailable

**Primary Components**: `cmd/data-poller` (or Reconciler)

**Trigger**: Attempt to trigger inference or check status

**Preconditions**:
*   RunPod API returning 503 or Connection Refused.

**Execution Sequence**:
1.  **[Client]** Calls `TriggerInference`.
2.  **[Client]** HTTP Request fails.
3.  **[Client]** Retry Policy (Exponential Backoff):
    *   Attempt 1: Fail.
    *   Wait 1s.
    *   Attempt 2: Fail.
    *   Wait 2s.
    *   Attempt 3: Fail.
4.  **[Client]** Returns error `ErrUpstreamUnavailable`.
5.  **[DataPoller]** Logs error. Aborts current cycle.
    *   *Result*: No DB write. System waits for next 15-minute schedule.
    *   *Note*: If persistent, `ForecastReady` metric stops, triggering Dead Man's Switch (OBS-001/002).

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (Standard client resilience).

---

## FAIL-008: Zarr File Corruption

**Primary Components**: `worker/eval`

**Trigger**: `EVAL-001` processing a tile.

**Preconditions**:
*   S3 object exists but has invalid checksum or truncated Zarr header.

**Execution Sequence**:
1.  **[Worker]** Calls `ForecastReader.LoadTile`.
2.  **[Reader]** Downloads chunk.
3.  **[Reader]** Validates Zarr integrity/Checksum.
4.  **[Reader]** Detects Corruption.
    *   Raises `ZarrCorruptionError` (Terminal).
5.  **[Worker]** Catch Block:
    *   Logs CRITICAL error: "Zarr corruption detected for run X tile Y".
    *   **[CloudWatch]** Emits `CorruptForecast` metric.
    *   **[SQS]** ACKs message (removes from queue to stop poison loop).
6.  **[CloudWatch]** `CorruptForecastAlarm` fires -> PagerDuty.
7.  **[Ops]** Manual intervention required (Delete S3 prefix to force rebuild via Reconciler or manual backfill).

**Data State Changes**:
*   **SQS**: Message removed.
*   **CloudWatch**: Critical alarm fired.

**Gap Analysis**:
*   None. (Addressed by Refactoring: Explicit terminal error handling and metric).


# Flow Simulations: Failure & Recovery (FAIL) - Continued

> **Range**: FAIL-009 to FAIL-013
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## FAIL-009: Bug Causing False Alerts (Kill Switch Activation)

**Primary Components**: `AWS SSM`, `AWS Lambda`, `worker/email`

**Trigger**: Ops decision triggered by `OBS-009` (Notification Spike Alarm)

**Preconditions**:
*   System is flooding users with erroneous alerts (e.g., bug in evaluation logic).
*   `FEATURE_ENABLE_EMAIL` SSM parameter exists.

**Execution Sequence**:
1.  **[Ops]** Executes CLI command to update SSM Parameter:
    `aws ssm put-parameter --name /prod/watchpoint/features/enable_email --value false --overwrite`
2.  **[Ops]** Forces Lambda Configuration Update to flush cold starts:
    `aws lambda update-function-configuration --function-name email-worker-prod --environment Variables={FORCE_UPDATE=timestamp}`
3.  **[AWS Lambda]** Service recycles execution environments. New instances load config from SSM during initialization (`03-config.md`).
4.  **[EmailWorker]** New instances start with `cfg.Feature.EnableEmail = false`.
5.  **[EmailWorker]** Processes next SQS message.
6.  **[Logic]** Checks `if !cfg.Feature.EnableEmail`.
7.  **[Branch - Kill Switch Active]**:
    *   **[Worker]** Logs: "Kill switch active: dropping notification".
    *   **[Worker]** ACKs SQS message (removing it from queue to stop the flood).
    *   **[DB]** Updates `notification_deliveries.status = 'skipped'` with reason `'kill_switch'`.
8.  **[Result]** Outbound traffic stops within ~60 seconds of Ops action.

**Data State Changes**:
*   **DB**: Deliveries marked skipped.
*   **SSM**: Parameter updated.

**Gap Analysis**:
*   None. (Procedural latency accepted as per architectural decision).

---

## FAIL-010: Bug Causing Missed Alerts (Recovery)

**Primary Components**: `api`, `worker/eval`, `DevOps`

**Trigger**: `OBS-008` (Canary Failure) or Customer Support ticket

**Preconditions**:
*   Bug identified and patched in code. Fix deployed to Production.
*   Affected WatchPoints known (e.g., all Monitor Mode WPs).

**Execution Sequence**:
1.  **[Ops]** Identifies affected IDs or criteria (e.g., "All active Monitor Mode").
2.  **[Ops]** Constructs Filter: `{"status": ["active"], "monitor_mode": true}`.
3.  **[Ops]** Calls `POST /v1/watchpoints/bulk/resume` (using the "Bulk Resume as Force Eval" pattern).
4.  **[API Handler]** Calls `Repo.UpdateStatusBatch` (No-op if already active).
5.  **[API Handler]** **Trigger Loop**:
    *   Iterates IDs.
    *   Calls `EvaluationTrigger.TriggerEvaluation(ctx, id)`.
    *   **[SQS]** Enqueues `EvalMessage` with `SpecificWatchPointIDs: [id]`.
6.  **[Eval Worker]** Receives message.
7.  **[Eval Worker]** Evaluates using **Fixed Code logic** against current forecast.
8.  **[Eval Worker]** Generates missed notification.
9.  **[Eval Worker]** Updates `last_evaluated_at` and `event_sequence`.

**Data State Changes**:
*   **SQS**: Targeted eval messages enqueued.
*   **DB**: Notifications created (late).

**Gap Analysis**:
*   None. (Uses existing Resume endpoint for forced re-evaluation).

---

## FAIL-011: Lambda Function Error (Generic Crash)

**Primary Components**: `AWS Lambda`, `SQS`, `CloudWatch`

**Trigger**: Unhandled Panic or OOM in Worker

**Preconditions**:
*   Message being processed causes runtime crash.

**Execution Sequence**:
1.  **[Worker]** Starts processing message.
2.  **[Worker]** Crashes (e.g., `panic: out of memory`).
3.  **[AWS Lambda]** Internal runtime detects failure. Does **not** delete SQS message.
4.  **[DB]** `notification_deliveries` state remains `pending` (worker died before DB update).
5.  **[SQS]** Visibility Timeout expires. Message becomes visible.
6.  **[AWS Lambda]** Retries (Attempt 2). Crashes again.
7.  **[AWS Lambda]** Retries (Attempt 3). Crashes again.
8.  **[SQS]** MaxReceiveCount (3) exceeded. Moves message to `notification-queue-dlq`.
9.  **[CloudWatch]** `DLQMessagesAlarm` fires.
10. **[Ops]** Manually investigates DLQ (SCHED-005).
11. **[Ops]** Determines poison pill. Purges message.
12. **[Ops]** Manually updates DB state to `failed` (SQL) if strict accounting required.

**Data State Changes**:
*   **SQS**: Message moved to DLQ.
*   **DB**: State remains stale (`pending`) until manual intervention.

**Gap Analysis**:
*   None. (Gap acknowledged and accepted; handled via DLQ procedure).

---

## FAIL-012: Single AZ Failure (Connection Recovery)

**Primary Components**: `api`, `internal/db` (`pgxpool`)

**Trigger**: AWS Availability Zone outage affecting RDS Primary

**Preconditions**:
*   Database configured Multi-AZ.

**Execution Sequence**:
1.  **[Infrastructure]** RDS Primary stops responding (IP unreachable).
2.  **[API]** Incoming Request `GET /v1/watchpoints`.
3.  **[API]** `pgxpool` attempts to check out connection.
4.  **[pgx]** Existing connections hang on TCP read.
5.  **[API]** `AcquireTimeout` (2s) fires.
6.  **[Handler]** Returns `500 Internal Server Error`.
7.  **[Infrastructure]** RDS promotes Standby (new IP/DNS).
8.  **[API]** `HealthCheckPeriod` (1m) background ticker runs.
9.  **[pgx]** Pings connections. Fails. Discards bad connections.
10. **[API]** Next request triggers new connection dial.
11. **[pgx]** Resolves DNS to new Primary IP. Connects successfully.
12. **[API]** Service restored.

**Data State Changes**:
*   None.

**Gap Analysis**:
*   None. (AcquireTimeout ensures fail-fast behavior during the gray failure period).

---

## FAIL-013: Region Failure — DR Failover

**Primary Components**: `Ops`, `cmd/data-poller`, `internal/db`

**Trigger**: Region `us-east-1` total outage. Management invokes DR Plan.

**Preconditions**:
*   DB Snapshot available in `us-west-2` (DR Region).
*   Infrastructure deployed in `us-west-2` (Pilot Light).

**Execution Sequence**:
1.  **[Ops]** Restores DB Snapshot to new RDS instance in `us-west-2`.
2.  **[Ops]** Updates DNS `api.watchpoint.io` -> `us-west-2` API Gateway.
3.  **[Ops]** **State Reset**: Executes SQL to invalidate S3 paths pointing to the dead region:
    ```sql
    UPDATE forecast_runs 
    SET status='failed', storage_path='' 
    WHERE run_timestamp > NOW() - INTERVAL '48 hours';
    ```
4.  **[Ops]** **Data Hydration**: Triggers `DataPoller` manually to re-download forecast data into the empty `us-west-2` buckets.
    *   Payload: `{"backfill_hours": 48, "limit": 10, "force_retry": true}`.
    *   **[Poller]** Finds failed runs. Re-triggers RunPod. RunPod writes to `us-west-2` S3 bucket.
5.  **[Ops]** Repeats Step 4 loop until caught up.
6.  **[System]** `ForecastReady` event fires in new region. Batcher processes. Recovery complete.

**Data State Changes**:
*   **DB**: `forecast_runs` reset and then re-completed.
*   **S3**: New region buckets populated.

**Gap Analysis**:
*   None. (Addressed by Refactoring: DataPoller `limit` input and SQL reset step).  step).


# Flow Simulations: Failure & Concurrency (FAIL/CONC)

> **Range**: FAIL-014 to CONC-003
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## FAIL-014: S3 Unavailable

**Primary Components**: `api` (`ForecastHandler`), `internal/forecasts` (`ForecastReader`), `worker/runpod` (`ZarrWriter`)

**Trigger**: AWS S3 Service Outage (5xx Errors)

**Scenario A: Forecast Retrieval (Read)**
1.  **[Handler]** Receives `GET /v1/forecasts/point`.
2.  **[Service]** Calls `ForecastReader.ReadPoint`.
3.  **[Reader]** Calls `s3Client.GetObject`.
4.  **[AWS SDK]** Internal Retry Policy executes (Default: 3 attempts, exponential backoff).
5.  **[Reader]** Retries exhausted. Returns error.
6.  **[Service]** Maps error to `types.ErrUpstreamForecastUnavailable`.
7.  **[Handler]** Returns `503 Service Unavailable` with `{"error": {"code": "upstream_forecast_unavailable"}}`.

**Scenario B: Forecast Generation (Write)**
1.  **[RunPod Worker]** `inference.py` attempts to write Zarr chunk via `s3fs`.
2.  **[RunPod Worker]** `s3fs` retries on 503.
3.  **[RunPod Worker]** Retries exhausted. Raises `IOError`.
4.  **[RunPod Worker]** Script exits with non-zero code.
5.  **[DataPoller]** (Client) Detects RunPod failure/timeout.
6.  **[DataPoller]** Increments `retry_count` in `forecast_runs`. (Handled by `FCST-004`).

**Data State Changes**:
*   None (Operations fail safely).

**Gap Analysis**:
*   None.

---

## FAIL-015: Stripe Unavailable

**Primary Components**: `api` (`BillingHandler`, `StripeWebhookHandler`), `external/stripe` (`StripeClient`)

**Trigger**: Stripe API Outage

**Scenario A: User Initiated Checkout (Outbound)**
1.  **[Handler]** Receives `POST /v1/billing/checkout-session`.
2.  **[Handler]** Calls `BillingService.CreateCheckoutSession`.
3.  **[Client]** `BaseClient` executes request wrapped in `gobreaker.CircuitBreaker`.
4.  **[Client]** Stripe returns 500/Timeout.
5.  **[Client]** Breaker records failure. Returns error.
6.  **[Handler]** Returns `503 Service Unavailable`.
    *   *Result*: User must retry later. No internal state corruption.

**Scenario B: Webhook Processing (Inbound)**
1.  **[Handler]** Receives `POST /v1/webhooks/stripe` (e.g., `invoice.paid`).
2.  **[Handler]** Calls `StripeVerifier.Verify`.
    *   *Logic*: Computes HMAC-SHA256 locally using `STRIPE_WEBHOOK_SECRET` env var.
    *   *Note*: **No outbound call** to Stripe is made.
3.  **[Handler]** Signature matches. Processes event payload.
4.  **[Handler]** Calls `Repo.ResumeAllByOrgID`.
    *   *Result*: Webhook processed successfully despite Stripe API outage. Critical billing state (subscription status) is updated locally.

**Data State Changes**:
*   **DB**: Outbound fails (no change). Inbound succeeds (state updated).

**Gap Analysis**:
*   None.

---

## CONC-001: Simultaneous PATCH to Same WatchPoint

**Primary Components**: `api` (`WatchPointHandler`), `internal/db` (`WatchPointRepository`)

**Trigger**: Two overlapping `PATCH /v1/watchpoints/{id}` requests

**Preconditions**:
*   `config_version` = 5.

**Execution Sequence**:
1.  **[Request A]** Starts. Validates input (e.g., change `threshold` to 10).
2.  **[Request B]** Starts. Validates input (e.g., change `threshold` to 20).
3.  **[Request A]** Calls `Repo.Update(ctx, wpA)`.
4.  **[DB - A]** Executes `UPDATE watchpoints ...`.
    *   Trigger `increment_config_version` fires.
    *   `config_version` becomes 6.
    *   Transaction A commits.
5.  **[Request B]** Calls `Repo.Update(ctx, wpB)`.
6.  **[DB - B]** Executes `UPDATE watchpoints ...`.
    *   Trigger `increment_config_version` fires.
    *   `config_version` becomes 7.
    *   Transaction B commits (Last Write Wins).
7.  **[Handler]** Both return 200 OK.
8.  **[Audit]** Both updates logged sequentially.

**Data State Changes**:
*   **DB**: `watchpoints` updated twice. Final state corresponds to Request B.

**Gap Analysis**:
*   None. (Last-Write-Wins behavior confirmed as per Design Addendum).

---

## CONC-002: Delete During Active Evaluation

**Primary Components**: `worker/eval`, `cmd/batcher`, `api` (`WatchPointHandler`)

**Trigger**: `DELETE /v1/watchpoints/{id}` occurs after Batcher runs but before Worker processes.

**Execution Sequence**:
1.  **[Batcher]** Queries tile X. Counts 10 active WatchPoints (including Target). Enqueues SQS message.
2.  **[API]** `DELETE` handler runs.
    *   **[DB]** `UPDATE watchpoints SET status='archived', deleted_at=NOW() WHERE id='target'`.
3.  **[Eval Worker]** Receives SQS message for tile X.
4.  **[Eval Worker]** Calls `Repo.FetchWatchpoints(tileID)`.
5.  **[DB]** Executes:
    ```sql
    SELECT * FROM watchpoints 
    WHERE tile_id = $1 AND status = 'active' -- Filters out archived/deleted
    ```
6.  **[Repo]** Returns 9 WatchPoints. Target is missing.
7.  **[Eval Worker]** Iterates list. Target is skipped.
8.  **[Eval Worker]** Commits state for 9 items.

**Data State Changes**:
*   **DB**: Target WatchPoint effectively ignored by evaluation loop.

**Gap Analysis**:
*   None. (Database query filter handles concurrency gracefully).

---

## CONC-003: Pause During Active Evaluation

**Primary Components**: `worker/eval`, `api` (`WatchPointHandler`)

**Trigger**: `POST /pause` occurs during evaluation window.

**Execution Sequence**:
1.  **[Batcher]** Enqueues message for tile containing Target.
2.  **[API]** `Pause` handler runs.
    *   **[DB]** `UPDATE watchpoints SET status='paused' WHERE id='target'`.
3.  **[Eval Worker]** Receives SQS message.
4.  **[Eval Worker]** Calls `Repo.FetchWatchpoints`.
5.  **[DB]** Query `WHERE status='active'` runs.
6.  **[Repo]** Target is excluded from result set.
7.  **[Eval Worker]** Skips Target.

**Data State Changes**:
*   **DB**: No evaluation state update for Target.

**Gap Analysis**:
*   None.


# Flow Simulations: Concurrency & Implementation (CONC/IMPL)

> **Range**: CONC-004 to IMPL-002
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## CONC-004: Organization Downgrade During WatchPoint Creation (Limit Race)

**Primary Components**: `api` (`WatchPointHandler`, `UsageEnforcer`), `internal/db`

**Trigger**: Two concurrent `POST /v1/watchpoints` requests (or 1 Create + 1 Downgrade) when near limit.

**Preconditions**:
*   Org Limit: 100.
*   Current Count: 99.

**Execution Sequence**:
1.  **[Request A]** Starts. Calls `UsageEnforcer`.
    *   **[DB]** `SELECT COUNT(*) ...`. Result: 99. Limit: 100. Status: Allowed.
2.  **[Request B]** Starts. Calls `UsageEnforcer`.
    *   **[DB]** `SELECT COUNT(*) ...`. Result: 99. Limit: 100. Status: Allowed.
3.  **[Request A]** Proceed to Insert.
    *   **[DB]** `INSERT INTO watchpoints ...` Commit.
4.  **[Request B]** Proceed to Insert.
    *   **[DB]** `INSERT INTO watchpoints ...` Commit.
5.  **[Result]** Current Count: 101. (Over Limit).
6.  **[Resolution]** The system accepts this temporary inconsistency ("Soft Consistency").
7.  **[Recovery]** Daily Job `BILL-011` runs.
    *   Detects 101 > 100.
    *   Pauses the most recent WatchPoint (LIFO strategy).

**Data State Changes**:
*   **DB**: `watchpoints` count exceeds plan limit temporarily.

**Gap Analysis**:
*   None. (Architectural decision documented in `05b-api-watchpoints.md`).

---

## CONC-005: Config Update During Notification Delivery (Snapshot Integrity)

**Primary Components**: `worker/eval`, `worker/email`, `internal/db`

**Trigger**: `EVAL-001` triggers an alert while user simultaneously deletes the channel via API.

**Execution Sequence**:
1.  **[Eval Worker]** Evaluates conditions. Result: Triggered.
2.  **[Eval Worker]** Prepares Notification.
3.  **[Eval Worker]** **Snapshots** Channel Config.
    *   Reads `wp.Channels` from memory (loaded at start of batch).
    *   Includes `email: user@example.com`.
4.  **[API]** `PATCH /watchpoints/{id}`. Deletes email channel. Commits to DB.
5.  **[Eval Worker]** Commits State Transaction.
    *   **[DB]** `INSERT INTO notification_deliveries (..., channel_config) VALUES (..., '{"address": "user@example.com"}')`.
6.  **[Email Worker]** Picks up SQS message.
7.  **[Email Worker]** Reads `channel_config` from delivery record.
    *   *Note*: Does NOT check live `watchpoints` table.
8.  **[Email Worker]** Delivers email to `user@example.com`.

**Data State Changes**:
*   **External**: Email sent to deleted channel (Correct "Deliver Original" behavior).

**Gap Analysis**:
*   None. (Snapshot pattern ensures delivery consistency).

---

## CONC-006: Forecast Ready During Previous Evaluation Batch (Stale Write Protection)

**Primary Components**: `worker/eval`, `internal/db`

**Trigger**: Batcher sends Message B (Ts=12:00) while Worker is stuck on Message A (Ts=11:00).

**Preconditions**:
*   Worker B finishes first (fast path). Worker A finishes later (slow path).

**Execution Sequence**:
1.  **[Worker B]** Processes Ts=12:00. Commits state.
    *   **[DB]** `last_forecast_run` updated to 12:00.
2.  **[Worker A]** Finishes processing Ts=11:00. Attempts commit.
3.  **[Worker A]** Executes Conditional Update:
    ```sql
    UPDATE watchpoint_evaluation_state
    SET last_forecast_run = '11:00', ...
    WHERE watchpoint_id = $id
      AND (last_forecast_run IS NULL OR last_forecast_run <= '11:00')
    RETURNING watchpoint_id;
    ```
4.  **[DB]** Condition `12:00 <= 11:00` fails. Returns 0 rows.
5.  **[Worker A]** Detects 0 updates.
6.  **[Worker A]** Skips `INSERT INTO notifications` for this WatchPoint.
7.  **[Worker A]** Completes batch (ACKs SQS).

**Data State Changes**:
*   **DB**: State remains at 12:00. Stale 11:00 write discarded.

**Gap Analysis**:
*   None. (Refactoring prompt mandated conditional update).

---

## IMPL-001: Config Version Race Condition Resolution (Fast Fail & Reset)

**Primary Components**: `worker/eval`, `internal/db`

**Trigger**: Eval Worker detects `wp.ConfigVersion != state.ConfigVersion`.

**Preconditions**:
*   User moved WatchPoint (Lat/Lon change) to a different tile.

**Execution Sequence**:
1.  **[Eval Worker]** Fetches WatchPoint (Version 2).
2.  **[Eval Worker]** Calculates `expected_tile` from `wp.Location`.
3.  **[Eval Worker]** Compares with `loaded_tile` (from SQS message).
4.  **[Logic]** Mismatch (`Tile A != Tile B`). Unsafe to evaluate.
5.  **[Eval Worker]** Executes **Reset Transaction**:
    ```sql
    UPDATE watchpoint_evaluation_state
    SET config_version = 2,
        previous_trigger_state = FALSE,
        trigger_value = NULL,
        escalation_level = 0
        -- Note: seen_threats is PRESERVED
    WHERE watchpoint_id = $id;
    ```
6.  **[Eval Worker]** Skips condition evaluation.
7.  **[Eval Worker]** ACKs message.
8.  **[Next Cycle]** Batcher puts WP in correct Tile B queue. Evaluation resumes.

**Data State Changes**:
*   **DB**: State reset, deduplication history preserved.

**Gap Analysis**:
*   None. (Refactoring prompt clarified `seen_threats` preservation).

---

## IMPL-002: Hot Tile Sub-Batch Failure Handling (Partial Retry)

**Primary Components**: `worker/eval`, `internal/db`

**Trigger**: Lambda timeout halfway through processing a page of 500 WatchPoints.

**Preconditions**:
*   Items 1-250 completed and committed. Items 251-500 pending.

**Execution Sequence**:
1.  **[Worker]** Timeout detected or Crash.
2.  **[SQS]** Message visibility expires. Message re-delivered.
3.  **[Worker]** Starts Retry (Items 1-500).
4.  **[Worker]** Loop Item 1:
    *   **[DB]** Checks `last_forecast_run` vs `msg.Timestamp`.
    *   Result: Match (Completed in previous run). **Skip**.
5.  **[Worker]** Loop Item 251:
    *   **[DB]** Checks `last_forecast_run`.
    *   Result: Mismatch (Old/Null). **Process**.
6.  **[Worker]** Evaluates Item 251.
7.  **[Worker]** Commits batch (only new/processed items updates applied).

**Data State Changes**:
*   **DB**: Evaluation completes for remaining items. No duplicate notifications generated.

**Gap Analysis**:
*   None. (Refactoring prompt mandated individual idempotency checks).


# Flow Simulations: Implementation Edge Cases (IMPL) - Continued

> **Range**: IMPL-003
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## IMPL-003: Forecast Run Idempotency Check (Phantom Protection)

**Primary Components**: `cmd/batcher`, `internal/db`

**Trigger**: `S3 Event: ObjectCreated` (Key: `.../_SUCCESS`)

**Preconditions**:
*   S3 event received.

**Execution Sequence**:
1.  **[Batcher]** `Handler` invoked. Parses `ForecastType`, `RunTimestamp` from key.
2.  **[Batcher]** Queries DB for Run Status.
    *   **[DB]** `SELECT id, status FROM forecast_runs WHERE model=$1 AND run_timestamp=$2`.
3.  **[Logic - Branching]**:
    *   **Case A (Normal)**: `status='running'`. Valid completion. Proceed to Step 4.
    *   **Case B (Duplicate)**: `status='complete'`. Log info "Duplicate S3 Event". **Exit Success**.
    *   **Case C (Phantom)**: Record not found.
        *   *Scenario*: Manual upload to S3 or zombie process.
        *   **[Batcher]** Log CRITICAL Error: "Phantom Forecast detected in S3".
        *   **[Batcher]** Emit `PhantomForecast` metric.
        *   **[Batcher]** **Exit Success** (discard event, do not process). *Refactoring implemented strict rejection.*.
4.  **[Batcher]** (If Case A) Updates DB.
    *   **[DB]** `UPDATE forecast_runs SET status='complete', storage_path=$path ...`
5.  **[Batcher]** Proceeds to Tile Grouping and Enqueue.

**Data State Changes**:
*   **DB**: `forecast_runs` updated to complete (only if valid).

**Gap Analysis**:
*   None. (Strict data authority enforced).


# Flow Simulations: Integration & End-to-End (INT)

> **Range**: INT-001 to INT-004
> **Status**: Simulated against Refactored Architecture (v3.3 + Refactoring Prompt)

---

## INT-001: New User to First Alert (The "Happy Path")

**Journey**: Complete onboarding lifecycle.

**Primary Components**: All

**Execution Sequence**:
1.  **[Signup]** User calls `POST /v1/users/signup` (USER-001).
    *   **[Result]** Org, User, and Plan Limits created atomically in DB.
2.  **[Setup]** User calls `POST /v1/watchpoints` (WPLC-001/002).
    *   **[API]** Validates against Plan Limits (Success).
    *   **[API]** *Branch: Event Mode* -> Returns `current_forecast` snapshot immediately (Instant Value).
    *   **[API]** *Branch: Monitor Mode* -> Returns `201` but no snapshot. **Suppresses immediate eval** (Quiet Create).
3.  **[Forecast]** Time passes. `DataPoller` triggers new forecast (FCST-003).
4.  **[Processing]** `Batcher` sees new forecast. Enqueues Eval (EVAL-001).
5.  **[Evaluation]** `EvalWorker` processes the new WatchPoint.
    *   *Event Mode*: Evaluates condition. Triggered.
    *   *Monitor Mode*: Establishes baseline (EVAL-005). No Alert.
6.  **[Notification]** (Event Mode) `EvalWorker` queues notification.
7.  **[Delivery]** `EmailWorker` delivers alert (NOTIF-001).

**Outcome**:
*   User receives email.
*   Dashboard reflects status "Triggered".

**Gap Analysis**:
*   None. (Explicit distinction between Event Mode instant gratification and Monitor Mode baseline latency confirmed).

---

## INT-002: Forecast Generation to Customer Notification (Latency Trace)

**Journey**: Tracing a single forecast run through the distributed system.

**Primary Components**: `RunPod`, `Batcher`, `EvalWorker`, `NotificationWorker`

**Execution Sequence**:
1.  **[RunPod]** Writes S3 Zarr. Latency Timer Start.
2.  **[Batcher]** S3 Event triggers. Generates `TraceID: uuid-123`.
3.  **[Batcher]** Enqueues `EvalMessage` to SQS. Payload includes `TraceID: uuid-123`.
4.  **[Eval Worker]** Dequeues message.
    *   Logs: `"Processing Tile", trace_id=uuid-123`.
5.  **[Eval Worker]** Detects Trigger. Generates `NotificationEvent`.
    *   **[Logic]** Copies `TraceID` from Input -> Output (*Refactored Logic*).
6.  **[Eval Worker]** Enqueues `NotificationMessage` to SQS. Payload includes `TraceID: uuid-123`.
7.  **[Notification Worker]** Dequeues message.
    *   Logs: `"Attempting Delivery", trace_id=uuid-123`.
8.  **[Notification Worker]** Emits `DeliveryLatency` metric.

**Outcome**:
*   X-Ray / Logs show unified trace across 3 async boundaries.

**Gap Analysis**:
*   None. (Refactoring fixed the trace propagation gap in Python worker).

---

## INT-003: WatchPoint Complete Lifecycle (Active -> Paused -> Archived)

**Journey**: Long-term state management.

**Primary Components**: `API`, `EvalWorker`, `Archiver`

**Execution Sequence**:
1.  **[Active]** WatchPoint created. Evaluated multiple times. `watchpoint_evaluation_state` stores history.
2.  **[Pause]** User calls `POST /pause` (WPLC-005).
    *   `watchpoints.status` -> `paused`.
    *   `evaluation_state` -> **Preserved** (Trigger history retained).
3.  **[Resume]** User calls `POST /resume` (WPLC-006).
    *   Pending `deferred` notifications cancelled.
    *   Immediate Eval triggered.
    *   State continues from preserved history.
4.  **[Expire]** Event end time passes.
5.  **[Archive]** `Archiver` job runs (WPLC-009).
    *   `watchpoints.status` -> `archived`.
    *   Trigger `generate_summary` task.
6.  **[Summary]** `EvalWorker` runs `SummaryGenerator` (OBS-007).
    *   Reads `archived` WatchPoint.
    *   Reads preserved `evaluation_state` history.
    *   Writes `watchpoints.summary` JSONB.
7.  **[Clone]** User clones for next year (WPLC-010).
    *   New WP created. Old WP remains as historical record.

**Outcome**:
*   Data integrity maintained across all state transitions.

**Gap Analysis**:
*   None.

---

## INT-004: Monitor Mode Daily Cycle

**Journey**: Recurring surveillance pattern.

**Primary Components**: `EvalWorker`, `DigestScheduler`, `EmailWorker`

**Execution Sequence**:
1.  **[Cycle 1-N]** Every 15m/6h, `EvalWorker` runs.
    *   Updates `watchpoint_evaluation_state.seen_threats` (Deduplication).
    *   Updates `watchpoint_evaluation_state.last_forecast_summary` (Snapshot).
2.  **[Digest Trigger]** `DigestScheduler` runs (NOTIF-003).
    *   Selects Orgs due for digest.
    *   Enqueues `monitor_digest` task.
3.  **[Generation]** `EmailWorker` (Digest Generator) runs.
    *   Reads `last_forecast_summary` (Current).
    *   Reads `last_digest_content` (Previous).
    *   **[Logic]** Generates diff (Improving/Worsening).
    *   **[Logic]** Renders template.
4.  **[Update]** `EmailWorker` updates `last_digest_content` = Current.
5.  **[Delivery]** Email sent.

**Outcome**:
*   User receives consolidated daily update without alert spam.

**Gap Analysis**:
*   None. (MVCC ensures consistent reads of summary data even if Eval Worker is writing).