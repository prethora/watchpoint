# WatchPoint Platform — Flows Specification v1

> Comprehensive mapping of all data flows, system behaviors, and process sequences in the WatchPoint platform.

**Companion to**:
- `watchpoint-platform-design-final.md`
- `watchpoint-tech-stack-v3.3.md`
- `watchpoint-design-addendum-v3.md`

**Generated from**: 3-Question Expansion Analysis (2026-02-01)

**Flow Count**: ~160 distinct flows across 23 domains

---

## Table of Contents

1. [Document Overview](#1-document-overview)
2. [System Context](#2-system-context)
3. [Core Runtime Flows](#3-core-runtime-flows)
4. [Resource Lifecycle Flows](#4-resource-lifecycle-flows)
5. [Operational Flows](#5-operational-flows)
6. [API & Integration Flows](#6-api--integration-flows)
7. [Supporting Flows](#7-supporting-flows)
8. [Failure & Recovery Flows](#8-failure--recovery-flows)
9. [End-to-End Integration Flows](#9-end-to-end-integration-flows)
10. [Appendices](#10-appendices)

---

## 1. Document Overview

### 1.1 Purpose

This document provides an exhaustive mapping of every flow in the WatchPoint platform. A **flow** describes what happens at a high level when a trigger occurs—detailing the sequence of operations, data mutations, downstream effects, and completion criteria.

This flows specification serves as:
- **Verification checklist** for the architecture document
- **Test case foundation** for integration testing
- **Onboarding reference** for developers
- **Audit trail** for system behavior

### 1.2 How to Read This Document

Each flow follows a consistent anatomy:

| Element | Description |
|---------|-------------|
| **Flow ID** | Unique identifier (e.g., `EVAL-001`) |
| **Flow Name** | Descriptive name |
| **Actor** | System (Scheduled), System (Event-Driven), or API |
| **Trigger** | What initiates this flow |
| **Preconditions** | What must be true for this flow to proceed |
| **Steps** | Numbered sequence of operations |
| **Data Touchpoints** | Tables, buckets, queues read/written |
| **Downstream** | Flows triggered by this one |
| **Completion** | How we know the flow succeeded |
| **Errors** | What can go wrong |
| **Idempotent** | Can this flow be safely retried? |

**Detail Tiers:**
- **Tier 1 (Full)**: Core flows with pseudocode and diagrams
- **Tier 2 (Standard)**: Most flows with complete steps
- **Tier 3 (Light)**: Supporting flows with brief description

### 1.3 Actor Definitions

| Actor | Description | Examples |
|-------|-------------|----------|
| **System (Scheduled)** | Time-based triggers, no external input | EventBridge cron, scheduled jobs |
| **System (Event-Driven)** | Internal events, reactive to state changes | S3 events, SQS messages, DB triggers |
| **API** | External HTTP requests | Client API calls, Stripe webhooks |

### 1.4 Flow ID Conventions

```
{DOMAIN}-{NUMBER}{VARIANT}

Where:
  DOMAIN  = 2-5 uppercase letters
  NUMBER  = 3-digit zero-padded number
  VARIANT = Optional lowercase letter for sub-flows
```

**Domain Prefixes:**

| Prefix | Domain | Description |
|--------|--------|-------------|
| `FCST` | Forecast Generation | Data fetching, inference, storage |
| `EVAL` | Evaluation Engine | Condition matching, state transitions |
| `NOTIF` | Notification Delivery | Channel formatting, delivery, retry |
| `WPLC` | WatchPoint Lifecycle | CRUD, pause, resume, archive |
| `USER` | User & Organization | Signup, invite, roles, API keys |
| `BILL` | Billing & Plans | Stripe, limits, usage |
| `OBS` | Observability | Monitoring, verification, alerts |
| `MAINT` | Maintenance | Cleanup, archival, retention |
| `SCHED` | Scheduled Jobs | Background job coordination |
| `API` | API Mechanics | Auth, validation, health |
| `FQRY` | Forecast Queries | Direct forecast access |
| `INFO` | Informational | Read-only data retrieval |
| `DASH` | Dashboard/UI | Session auth, Stripe checkout |
| `HOOK` | Webhook Operations | Secret rotation, delivery tracking |
| `VERT` | Vertical Apps | Integration patterns |
| `VALID` | Validation | Input validation pipelines |
| `SEC` | Security | Protection mechanisms |
| `TEST` | Testing/Sandbox | Test mode behavior |
| `BULK` | Bulk Operations | Multi-resource actions |
| `FAIL` | Failure & Recovery | Error handling, fallbacks |
| `CONC` | Concurrency | Race condition handling |
| `IMPL` | Implementation | Edge case handling |
| `INT` | Integration | End-to-end journeys |

---

## 2. System Context

### 2.1 Actor Taxonomy

```
ACTORS IN THE WATCHPOINT SYSTEM
================================

┌─────────────────────────────────────────────────────────────────┐
│                    SYSTEM (SCHEDULED)                           │
│  ───────────────────────────────────────────────────────────── │
│  EventBridge triggers at defined intervals                      │
│                                                                 │
│  • Data Poller (every 15 min)                                  │
│  • Archiver (every 15 min)                                     │
│  • Digest Generator (per-org schedule)                         │
│  • Verification Pipeline (daily)                               │
│  • Cleanup Jobs (daily/weekly/monthly)                         │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                  SYSTEM (EVENT-DRIVEN)                          │
│  ───────────────────────────────────────────────────────────── │
│  Reactive to internal state changes                            │
│                                                                 │
│  • S3 Event (_SUCCESS marker) → Batcher                        │
│  • SQS Message → Eval Workers, Notification Workers            │
│  • Database Trigger → config_version increment                 │
│  • Email Bounce Event → Bounce Handler                         │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                        API (EXTERNAL)                           │
│  ───────────────────────────────────────────────────────────── │
│  HTTP requests from outside the system                         │
│                                                                 │
│  • Client API calls (WatchPoint CRUD, queries)                 │
│  • Dashboard requests (session-based auth)                     │
│  • Stripe webhooks (payment events)                            │
│  • Health checks (monitoring probes)                           │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 Flow Dependency Graph

```
SCHEDULED TRIGGERS
==================

EventBridge (every 15 min)
    └── FCST-003: Data Poller
            ├── [GFS detected] ──► RunPod ──► FCST-001: Medium-Range Generation
            └── [GOES detected] ──► RunPod ──► FCST-002: Nowcast Generation

EventBridge (every 15 min)
    └── WPLC-009: Archiver
            └── OBS-007: Post-Event Summary Generation

EventBridge (per-org schedule)
    └── NOTIF-003: Digest Generation
            └── NOTIF-001: Notification Delivery (digest)

EventBridge (daily)
    ├── SCHED-001: Rate Limit Counter Reset
    ├── SCHED-002: Usage Aggregation
    ├── SCHED-003: Stripe Sync
    ├── MAINT-001: Forecast Tier Transition
    ├── MAINT-002: Archived WatchPoint Cleanup
    ├── MAINT-005: Soft-Deleted Org Cleanup
    ├── MAINT-006: Expired Invite Cleanup
    └── OBS-005: Verification Pipeline
            └── OBS-006: Calibration Update (weekly)


EVENT-DRIVEN TRIGGERS
=====================

S3 Event (_SUCCESS marker)
    └── EVAL-001: Batcher
            ├── [nowcast] ──► SQS urgent ──► EVAL-001: Eval Worker (urgent)
            └── [medium-range] ──► SQS standard ──► EVAL-001: Eval Worker (standard)
                    │
                    ├── [hot tile] ──► EVAL-002: Hot Tile Pagination
                    ├── [monitor mode] ──► EVAL-003: Monitor Mode Evaluation
                    │       └── EVAL-003c: Threat Hash Deduplication
                    ├── [config mismatch] ──► IMPL-001: Config Version Resolution
                    ├── [first eval] ──► EVAL-005: First Evaluation
                    │
                    └── [state change] ──► notification-queue
                            │
                            ├── NOTIF-001: Immediate Delivery
                            │       ├── [quiet hours] ──► NOTIF-002: Suppression/Deferral
                            │       ├── [webhook fail] ──► NOTIF-004: Retry
                            │       ├── [429 received] ──► NOTIF-005: Rate Limit Handling
                            │       └── [success] ──► HOOK-005: Delivery Tracking
                            │
                            ├── [escalation] ──► NOTIF-008: Escalation Notification
                            └── [cleared] ──► NOTIF-009: Cleared Notification

Email Bounce Event
    └── NOTIF-006: Bounce Handling
            └── [3 bounces] ──► USER-014: Notify Owner


API TRIGGERS
============

POST /v1/watchpoints
    └── WPLC-001 or WPLC-002: Create WatchPoint
            ├── VALID-001: Input Validation
            │       └── API-002: SSRF Check (if webhook)
            ├── BILL-008: Limit Check
            └── [next forecast_ready] ──► EVAL-001 (includes new WP)

POST /v1/watchpoints/{id}/resume
    └── WPLC-006: Resume
            └── [immediate evaluation triggered]

POST /v1/webhooks/stripe
    └── BILL-007: Webhook Handler
            ├── [checkout.session.completed] ──► BILL-002: Subscription Created
            ├── [invoice.payment_failed] ──► BILL-005: Payment Failure
            │       └── [7 days elapsed] ──► BILL-006: Grace Period Expiration
            └── [customer.subscription.updated] ──► BILL-003 or BILL-004: Plan Change


FAILURE CASCADES
================

FCST-001/002 Failure
    └── FCST-004: Retry (up to 3x)
            └── [all retries failed] ──► FCST-005: Serve Stale
                    └── OBS-001/002: Dead Man's Switch Alarm

Database Unavailable
    └── FAIL-001: Automatic Failover
            └── [failover fails] ──► FAIL-002: Queue Writes
```

### 2.3 Data Store Reference

| Store | Type | Purpose |
|-------|------|---------|
| PostgreSQL (Supabase) | Database | All transactional data |
| S3 (forecasts bucket) | Object Storage | Zarr forecast files |
| S3 (archives bucket) | Object Storage | Cold storage, backups |
| SQS (eval-queue-urgent) | Queue | Nowcast evaluation messages |
| SQS (eval-queue-standard) | Queue | Medium-range evaluation messages |
| SQS (notification-queue) | Queue | Notification delivery messages |
| SQS (DLQ) | Queue | Failed messages for review |
| CloudWatch | Metrics | Observability data |
| Stripe | External | Billing, subscriptions |
| AWS SES | External | Email delivery |

---

## 3. Core Runtime Flows

### 3.1 Forecast Generation Domain (FCST)

---

#### FCST-001: Medium-Range Forecast Generation

**Actor**: System (Event-Driven)
**Trigger**: Data Poller detects new GFS run available (~4x/day)
**Tier**: 1 (Full Detail)

**Preconditions**:
- New GFS model run available on upstream source
- RunPod GPU capacity available
- Previous inference not currently running

**Steps**:
1. Data Poller detects new GFS run timestamp
2. Trigger RunPod serverless endpoint with run metadata
3. RunPod fetches GFS data from AWS/NOAA mirror
4. Atlas model performs inference (global, 0.25° resolution)
5. Variable translation maps model output to standard variables
6. Precipitation probability derived from ensemble spread
7. Output written to Zarr format with chunking by tile
8. Zarr uploaded to S3: `s3://forecasts/medium_range/{run_timestamp}/`
9. Write `_SUCCESS` marker file to signal completion
10. Emit ForecastReady metric to CloudWatch

**Data Touchpoints**:
```
DB Reads:   forecast_runs (check for duplicate)
DB Writes:  forecast_runs (new record)
S3 Reads:   Upstream GFS data (AWS/NOAA)
S3 Writes:  s3://forecasts/medium_range/{timestamp}/forecast.zarr
            s3://forecasts/medium_range/{timestamp}/_SUCCESS
External:   RunPod API (trigger inference)
Metrics:    ForecastReady, InferenceDuration
```

**Downstream**: S3 event triggers EVAL-001 (Batcher)

**Completion**: `_SUCCESS` marker exists, forecast_runs record status = 'complete'

**Errors**:
- Upstream data unavailable → FCST-004 (Retry) → FCST-005 (Serve Stale)
- RunPod timeout → FCST-004 (Retry)
- S3 write failure → Retry with backoff

**Idempotent**: Yes (same run_timestamp produces same output)

**Sequence Diagram**:
```
┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐
│DataPoller│  │  RunPod  │  │   GFS    │  │    S3    │  │    DB    │
└────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘
     │             │             │             │             │
     │ Detect new run            │             │             │
     │─────────────────────────────────────────────────────►│
     │             │             │             │  Check dup  │
     │             │             │             │◄────────────│
     │             │             │             │             │
     │ Trigger     │             │             │             │
     │────────────►│             │             │             │
     │             │ Fetch GFS   │             │             │
     │             │────────────►│             │             │
     │             │◄────────────│             │             │
     │             │             │             │             │
     │             │ Inference   │             │             │
     │             │─────────────│             │             │
     │             │             │             │             │
     │             │ Write Zarr  │             │             │
     │             │────────────────────────►│             │
     │             │             │             │             │
     │             │ Write _SUCCESS           │             │
     │             │────────────────────────►│             │
     │             │             │             │─────────────│
     │             │             │             │  S3 Event   │
     │             │             │             │             │
```

---

#### FCST-002: Nowcast Forecast Generation

**Actor**: System (Event-Driven)
**Trigger**: Data Poller detects new GOES + radar data (~every 15-30 min)

**Preconditions**:
- New GOES-16/18 satellite data available
- New MRMS radar data available
- Both data sources within acceptable time window

**Steps**:
1. Data Poller detects new satellite and radar data
2. Validate temporal alignment (within 5 minutes of each other)
3. Trigger RunPod with GOES + MRMS references
4. StormScope model performs inference (CONUS only, 2km resolution)
5. Derive precipitation probability from deterministic output using calibration
6. Output written to Zarr with hourly chunks for 0-6 hour horizon
7. Zarr uploaded to S3: `s3://forecasts/nowcast/{run_timestamp}/`
8. Write `_SUCCESS` marker
9. Emit ForecastReady metric (nowcast variant)

**Data Touchpoints**:
```
DB Reads:   forecast_runs, calibration_coefficients
DB Writes:  forecast_runs
S3 Reads:   GOES bucket, MRMS bucket
S3 Writes:  s3://forecasts/nowcast/{timestamp}/forecast.zarr
            s3://forecasts/nowcast/{timestamp}/_SUCCESS
```

**Downstream**: S3 event triggers EVAL-001 (Batcher) with urgent queue routing

**Completion**: `_SUCCESS` marker exists

**Errors**: Same as FCST-001

**Idempotent**: Yes

---

#### FCST-003: Data Poller Execution

**Actor**: System (Scheduled)
**Trigger**: EventBridge schedule (every 15 minutes)

**Steps**:
1. Lambda invoked by EventBridge
2. Check GFS bucket for runs newer than last processed
3. Check GOES bucket for images newer than last processed
4. Check MRMS bucket for radar newer than last processed
5. For each new data set found:
   - Record in forecast_runs with status = 'detected'
   - Trigger appropriate inference job (FCST-001 or FCST-002)
6. Update last_checked timestamps
7. Emit DataPollerRun metric

**Data Touchpoints**:
```
DB Reads:   forecast_runs (last processed timestamps)
DB Writes:  forecast_runs (new detected records)
S3 Reads:   GFS bucket (list), GOES bucket (list), MRMS bucket (list)
External:   RunPod API (trigger on detection)
```

**Downstream**: FCST-001 or FCST-002

**Completion**: All detected data queued for processing

**Idempotent**: Yes (detection is idempotent)

---

#### FCST-004: Forecast Generation Failure — Retry

**Actor**: System (Event-Driven)
**Trigger**: Inference failure or timeout on RunPod

**Steps**:
1. Detect failure (timeout, error response, or missing output)
2. Increment retry_count in forecast_runs
3. If retry_count < 3:
   - Wait with exponential backoff (30s, 60s, 120s)
   - Re-trigger RunPod with same parameters
4. If retry_count >= 3:
   - Mark forecast_run as 'failed'
   - Trigger FCST-005 (Serve Stale)
   - Emit ForecastGenerationFailed alarm

**Data Touchpoints**:
```
DB Writes:  forecast_runs (retry_count, status)
Metrics:    ForecastRetry, ForecastFailed
```

**Downstream**: Re-attempt FCST-001/002 or FCST-005

---

#### FCST-005: Forecast Generation Failure — Fallback to Stale

**Actor**: System (Event-Driven)
**Trigger**: All retries exhausted OR upstream data unavailable

**Steps**:
1. Mark current run as 'failed_stale_fallback'
2. Identify most recent successful forecast
3. Update forecast_status table to indicate stale serving
4. Continue serving previous forecast with staleness metadata
5. Alert ops team via SNS/PagerDuty
6. Include staleness warning in API responses

**Data Touchpoints**:
```
DB Reads:   forecast_runs (find latest successful)
DB Writes:  forecast_status (stale indicator)
External:   SNS/PagerDuty (ops alert)
```

**Downstream**: OBS-001/002 (Dead Man's Switch) may fire

**Completion**: System operating in degraded mode with alerts sent

---

#### FCST-006: Forecast Data Retention/Cleanup

**Actor**: System (Scheduled)
**Trigger**: Scheduled job (daily)

**Steps**:
1. Query forecast_runs for transition candidates
2. Apply retention rules:
   - Nowcast: Delete after 7 days
   - Medium-range hot → warm: After 24 hours
   - Medium-range warm → cold: After 7 days
   - Medium-range cold → delete: After 90 days
3. Move/delete S3 objects accordingly
4. Update forecast_runs status
5. Emit retention metrics

**Data Touchpoints**:
```
DB Reads:   forecast_runs
DB Writes:  forecast_runs (status updates)
S3 Writes:  Delete or move objects between tiers
```

---

### 3.2 Evaluation Domain (EVAL)

---

#### EVAL-001: Forecast Evaluation Pipeline (Core)

**Actor**: System (Event-Driven)
**Trigger**: S3 event (_SUCCESS marker written)
**Tier**: 1 (Full Detail)

**Preconditions**:
- Forecast generation completed successfully
- _SUCCESS marker exists in S3

**Steps**:

**Batcher Phase:**
1. S3 event triggers Batcher Lambda
2. Parse event to extract forecast type and run_timestamp
3. Check forecast_runs for duplicate (idempotency)
4. Query active WatchPoints grouped by tile_id:
   ```sql
   SELECT tile_id, COUNT(*) as wp_count
   FROM watchpoints
   WHERE status = 'active'
   GROUP BY tile_id
   ```
5. For each tile with WatchPoints:
   - If nowcast AND within CONUS: route to urgent queue
   - If medium-range: route to standard queue
   - If wp_count > 500: apply EVAL-002 (pagination)
6. Create SQS message with: tile_id, run_timestamp, forecast_type, page info
7. Send to appropriate queue

**Eval Worker Phase:**
8. Worker receives SQS message
9. Load Zarr chunk for tile from S3
10. Fetch WatchPoints for tile (with pagination if applicable):
    ```sql
    SELECT * FROM watchpoints
    WHERE tile_id = ? AND status = 'active'
    ORDER BY id
    LIMIT ? OFFSET ?
    ```
11. For each WatchPoint:
    - Fetch evaluation_state
    - Check config_version match (→ IMPL-001 if mismatch)
    - If Monitor Mode: apply EVAL-003 logic
    - If first evaluation: apply EVAL-005 logic
    - Extract forecast values at location (bilinear interpolation)
    - Evaluate conditions against thresholds
    - Determine trigger state and escalation level
    - Compare to previous state
12. If state changed (triggered or cleared):
    - Create notification record
    - Queue to notification-queue
13. Update evaluation_state:
    - last_evaluated_at
    - previous_trigger_state
    - evaluation_result (forecast values)
    - escalation_level
14. Delete SQS message (acknowledge)

**Data Touchpoints**:
```
DB Reads:   watchpoints, watchpoint_evaluation_state, organizations
DB Writes:  watchpoint_evaluation_state, notifications, notification_deliveries
S3 Reads:   s3://forecasts/{type}/{timestamp}/forecast.zarr/{tile_chunk}
SQS Send:   eval-queue-urgent OR eval-queue-standard (Batcher)
            notification-queue (Eval Worker)
SQS Recv:   eval-queue-urgent OR eval-queue-standard (Eval Worker)
```

**Downstream**: NOTIF-001 (if state changed)

**Completion**: All tiles processed, evaluation_state updated

**Errors**:
- Zarr read failure → FAIL-008
- Database unavailable → FAIL-002
- Worker timeout → SQS retry

**Idempotent**: Yes (re-evaluation produces same state if conditions unchanged)

**Pseudocode (Condition Evaluation)**:
```python
def evaluate_conditions(watchpoint, forecast_values):
    results = []
    for condition in watchpoint.conditions:
        value = forecast_values[condition.variable]

        if condition.operator == 'gt':
            met = value > condition.threshold
        elif condition.operator == 'lt':
            met = value < condition.threshold
        elif condition.operator == 'between':
            met = condition.threshold[0] <= value <= condition.threshold[1]
        elif condition.operator == 'eq':
            met = abs(value - condition.threshold) < 0.01

        results.append(met)

    if watchpoint.condition_logic == 'all':
        return all(results)
    else:  # 'any'
        return any(results)
```

---

#### EVAL-002: Hot Tile Pagination

**Actor**: System (Event-Driven)
**Trigger**: Batcher detects tile with >500 WatchPoints

**Steps**:
1. Calculate page count: ceil(wp_count / 500)
2. For each page:
   - Create SQS message with page_number and page_size
   - Include total_pages for tracking
3. Eval Worker processes subset with LIMIT/OFFSET
4. Each page processes independently
5. Partial success acceptable (failed page retries independently)

**Data Touchpoints**:
```
SQS Send:   Multiple messages for same tile with page metadata
DB Reads:   watchpoints with LIMIT/OFFSET
```

---

#### EVAL-003: Monitor Mode Evaluation

**Actor**: System (Event-Driven)
**Trigger**: Same as EVAL-001, but WatchPoint has monitor_config

**Steps**:
1. Calculate rolling window: now() to now() + window_hours
2. Apply active_hours filter (EVAL-003b)
3. Apply active_days filter
4. For each valid time step in window:
   - Evaluate conditions
   - Generate threat hash if triggered (EVAL-003c)
5. Check threat hash against seen_threat_hashes
6. If new threat: queue notification
7. If known threat: skip (deduplicated)
8. Update seen_threat_hashes array
9. Store detailed evaluation result for digest comparison

**Sub-flows**:

##### EVAL-003a: Rolling Window Calculation
```python
def calculate_window(monitor_config, timezone):
    now = current_time_in_tz(timezone)
    window_end = now + timedelta(hours=monitor_config.window_hours)
    return (now, window_end)
```

##### EVAL-003b: Active Hours Filtering
```python
def filter_active_hours(time_steps, active_hours, timezone):
    result = []
    for ts in time_steps:
        local_hour = ts.astimezone(timezone).hour
        for start, end in active_hours:
            if start <= local_hour < end:
                result.append(ts)
                break
    return result
```

##### EVAL-003c: Threat Hash Deduplication
```python
def generate_threat_hash(threat_start, threat_type, severity):
    data = f"{threat_start.isoformat()}_{threat_type}_{severity}"
    return hashlib.sha256(data.encode()).hexdigest()[:16]

def is_new_threat(hash, seen_hashes, max_age_hours=48):
    # Clean old hashes
    cutoff = now() - timedelta(hours=max_age_hours)
    seen_hashes = [h for h in seen_hashes if h.timestamp > cutoff]

    if hash in [h.hash for h in seen_hashes]:
        return False
    return True
```

---

#### EVAL-004: Config Version Mismatch Handling

**Actor**: System (Event-Driven)
**Trigger**: Eval Worker detects config_version changed

**Steps**:
1. Detect: evaluation_state.config_version != watchpoint.config_version
2. Reset trigger state (re-evaluate from scratch)
3. Clear escalation_level
4. Evaluate with new conditions
5. Update evaluation_state with new config_version

**Note**: Prevents stale condition logic from affecting notifications

---

#### EVAL-005: First Evaluation (New WatchPoint)

**Actor**: System (Event-Driven)
**Trigger**: WatchPoint created, next forecast_ready event occurs

**Steps**:
1. Detect: No evaluation_state record exists
2. Create evaluation_state record with defaults
3. Perform full evaluation
4. If triggered:
   - For Event Mode: Send notification (first alert)
   - For Monitor Mode: No notification on first eval (need baseline)
5. Store baseline evaluation_result

---

#### EVAL-006: Evaluation Queue Backlog Recovery

**Actor**: System (Event-Driven)
**Trigger**: Queue depth alarm fires (>100 messages for >10 min)

**Steps**:
1. Ops receives alarm notification
2. Assess: transient spike or persistent issue
3. If persistent, options:
   - Scale up worker concurrency
   - Skip old messages (process newest)
   - Prioritize urgent queue
4. Implement strategy via configuration change
5. Monitor queue depth reduction

**Note**: Manual/semi-automated intervention flow

---

### 3.3 Notification Domain (NOTIF)

---

#### NOTIF-001: Immediate Notification Delivery

**Actor**: System (Event-Driven)
**Trigger**: Notification queued by Eval Worker
**Tier**: 1 (Full Detail)

**Preconditions**:
- Notification record exists in database
- At least one active channel configured

**Steps**:
1. Worker pulls message from notification-queue
2. Fetch notification record and WatchPoint
3. Check if WatchPoint still exists and is active
4. For each channel:
   - Check quiet hours (→ NOTIF-002 if applicable)
   - Format payload for channel type
   - If webhook: detect platform and format (→ NOTIF-007)
   - Deliver:
     - Email: Send via AWS SES
     - Webhook: POST with signature
5. Record delivery attempt in notification_deliveries
6. If all channels succeed: mark notification as 'delivered'
7. If any channel fails: apply retry logic (→ NOTIF-004/005)
8. Delete SQS message

**Data Touchpoints**:
```
DB Reads:   notifications, watchpoints, organizations
DB Writes:  notifications (status), notification_deliveries
SQS Recv:   notification-queue
External:   AWS SES (email), Customer webhooks
```

**Downstream**: HOOK-005 (delivery tracking on success)

**Completion**: All channels attempted, status updated

---

#### NOTIF-002: Quiet Hours Suppression & Deferral

**Actor**: System (Event-Driven)
**Trigger**: Notification arrives during quiet hours

**Steps**:
1. Check organization.notification_preferences.quiet_hours
2. Determine if current time is within quiet period (in user's timezone)
3. Check exception criteria:
   - Urgency level >= threshold?
   - Nowcast alert (imminent weather)?
4. If exception met: proceed with delivery
5. If no exception:
   - Option A: Defer to end of quiet period
   - Option B: Include in morning digest
6. Create deferred_notification record
7. Schedule for later delivery

---

#### NOTIF-003: Digest Generation & Delivery

**Actor**: System (Scheduled)
**Trigger**: Scheduled job based on user's digest_preferences

**Steps**:
1. EventBridge triggers digest Lambda at scheduled time
2. Query Monitor Mode WatchPoints for organization
3. For each WatchPoint:
   - Fetch last_evaluation_result
   - Fetch previous_digest_content (→ NOTIF-003b)
   - Calculate deltas and trends
4. Aggregate into digest content
5. Compare to previous digest (NOTIF-003b)
6. Format digest notification
7. Queue to notification-queue
8. Store current digest as previous for next comparison

**Sub-flow NOTIF-003b: Comparison to Previous Digest**
```python
def compare_to_previous(current, previous):
    if not previous:
        return {'status': 'first_digest'}

    changes = []
    for var in current.variables:
        curr_val = current.values[var]
        prev_val = previous.values.get(var)
        if prev_val:
            delta = curr_val - prev_val
            if abs(delta) > significance_threshold(var):
                direction = 'increased' if delta > 0 else 'decreased'
                changes.append({
                    'variable': var,
                    'from': prev_val,
                    'to': curr_val,
                    'direction': direction
                })
    return {'status': 'changed' if changes else 'stable', 'changes': changes}
```

---

#### NOTIF-004: Webhook Delivery with Retry

**Actor**: System (Event-Driven)
**Trigger**: Webhook POST fails (timeout, 5xx, connection error)

**Steps**:
1. Detect failure (non-2xx, timeout, connection refused)
2. Increment retry_count
3. If retry_count < 3:
   - Calculate backoff: 1s, 5s, 30s
   - Re-queue with delay (SQS delay seconds)
4. If retry_count >= 3:
   - Mark delivery as 'failed'
   - Log error details
   - Consider NOTIF-010 (all channels failed)

---

#### NOTIF-005: Webhook Rate Limit Handling (429)

**Actor**: System (Event-Driven)
**Trigger**: Webhook returns 429 with Retry-After

**Steps**:
1. Parse Retry-After header (seconds or HTTP-date)
2. Calculate delay
3. Re-queue with appropriate delay
4. Does not count against retry limit (different from errors)

---

#### NOTIF-006: Email Bounce Handling

**Actor**: System (Event-Driven)
**Trigger**: Email provider reports bounce

**Steps**:
1. Receive bounce notification from SES via SNS
2. Identify affected email channel
3. Increment bounce_count on channel
4. If bounce_count >= 3:
   - Mark channel as 'failed'
   - Trigger USER-014 (notify owner)
5. Log bounce details for support

---

#### NOTIF-007: Platform-Specific Formatting

**Actor**: Embedded in NOTIF-001

**Platform Detection Logic**:
```python
def detect_platform(url):
    if 'hooks.slack.com' in url:
        return 'slack'
    elif 'discord.com/api/webhooks' in url:
        return 'discord'
    elif 'webhook.office.com' in url or 'outlook.office.com' in url:
        return 'teams'
    elif 'chat.googleapis.com' in url:
        return 'google_chat'
    else:
        return 'generic'
```

**Formatting by Platform**:
- Slack: Block Kit JSON
- Discord: Embeds with color coding
- Teams: Adaptive Cards
- Google Chat: Cards v2
- Generic: Standard JSON payload

---

#### NOTIF-008: Escalation Notification

**Actor**: System (Event-Driven)
**Trigger**: Conditions worsened while still triggered

**Steps**:
1. Detect escalation: new severity > previous severity
2. Check escalation cooldown (2 hours since last escalation)
3. Check daily limit (max 3 escalations per 24h)
4. If allowed:
   - Increment escalation_level
   - Create escalation notification
   - Queue for delivery
5. If cooldown or limit: skip, log suppression

---

#### NOTIF-009: Cleared Notification

**Actor**: System (Event-Driven)
**Trigger**: Conditions no longer met (triggered → not triggered)

**Steps**:
1. Detect state transition: previous_trigger_state = true, current = false
2. Check WatchPoint.notify_on_clear setting
3. If enabled:
   - Create 'cleared' notification
   - Include what changed
   - Queue for delivery
4. Reset escalation_level to 0

---

#### NOTIF-010: All Channels Failed Recovery

**Actor**: System (Event-Driven)
**Trigger**: All configured channels fail for a notification

**Steps**:
1. Detect: all notification_deliveries for notification are 'failed'
2. Mark notification as 'all_channels_failed'
3. Log error for ops review
4. On next evaluation cycle: retry notification if still triggered
5. If persistent (3+ cycles): alert ops team

---

## 4. Resource Lifecycle Flows

### 4.1 WatchPoint Lifecycle (WPLC)

---

#### WPLC-001: WatchPoint Creation (Event Mode)

**Actor**: API
**Trigger**: POST /v1/watchpoints with time_window

**Preconditions**:
- Valid authentication
- Organization exists and is active

**Steps**:
1. Validate request (→ VALID-001)
2. Check organization WatchPoint limit (→ BILL-008)
3. Compute tile_id from lat/lon
4. Check CONUS bounds for nowcast eligibility
5. Insert WatchPoint record
6. Initialize evaluation_state record
7. Fetch current forecast for location
8. Return WatchPoint with current_forecast snapshot

**Data Touchpoints**:
```
DB Reads:   organizations (limits), watchpoints (count)
DB Writes:  watchpoints, watchpoint_evaluation_state, audit_log
S3 Reads:   Current forecast Zarr (for snapshot)
```

**Downstream**: Next EVAL-001 will include this WatchPoint

**Errors**:
- 400: Validation failed (→ WPLC-012)
- 403: Limit exceeded
- 422: Invalid conditions

---

#### WPLC-002: WatchPoint Creation (Monitor Mode)

**Actor**: API
**Trigger**: POST /v1/watchpoints with monitor_config

**Steps**:
1. Same as WPLC-001 steps 1-4
2. Validate monitor_config:
   - window_hours: 6-168
   - active_hours: valid ranges
   - active_days: 1-7
3. Insert with monitor_config
4. Initialize evaluation_state with empty seen_threat_hashes
5. Return WatchPoint (no immediate notification on first eval)

---

#### WPLC-003: WatchPoint Update (Partial)

**Actor**: API
**Trigger**: PATCH /v1/watchpoints/{id}

**Steps**:
1. Validate WatchPoint ownership
2. Validate changed fields
3. If conditions/condition_logic/time_window changed:
   - Increment config_version (database trigger)
4. Update WatchPoint record
5. Log to audit trail
6. Return updated WatchPoint

---

#### WPLC-004: WatchPoint Update While Triggered

**Actor**: API
**Trigger**: PATCH on a WatchPoint where previous_trigger_state = true

**Steps**:
1. Same as WPLC-003
2. Config version increments
3. Set immediate_eval_flag (if supported)
4. Next evaluation re-evaluates with new conditions
5. May result in 'cleared' notification if conditions no longer met

---

#### WPLC-005: WatchPoint Pause

**Actor**: API
**Trigger**: POST /v1/watchpoints/{id}/pause

**Steps**:
1. Validate WatchPoint ownership
2. Validate current status is 'active'
3. Update status to 'paused'
4. Preserve evaluation_state (not cleared)
5. Log to audit trail
6. Return updated WatchPoint

**Note**: Paused WatchPoints skipped during evaluation

---

#### WPLC-006: WatchPoint Resume

**Actor**: API
**Trigger**: POST /v1/watchpoints/{id}/resume

**Steps**:
1. Validate WatchPoint ownership
2. Validate current status is 'paused'
3. Update status to 'active'
4. Trigger immediate evaluation (don't wait for cycle)
5. If currently triggered: notification sent (subject to cooldowns)
6. Log to audit trail
7. Return updated WatchPoint

---

#### WPLC-007: WatchPoint Resume with Quiet Hours Backlog

**Actor**: API
**Trigger**: Resume after paused during quiet hours with pending notifications

**Steps**:
1. Same as WPLC-006
2. Check for deferred notifications
3. Consolidate into single digest
4. Deliver consolidated notification
5. Clear deferred backlog

---

#### WPLC-008: WatchPoint Delete

**Actor**: API
**Trigger**: DELETE /v1/watchpoints/{id}

**Steps**:
1. Validate WatchPoint ownership
2. Mark as deleted (soft delete) or hard delete
3. Note: Notifications already in queue still deliver
4. Remove from active evaluation
5. Notification history remains queryable
6. Log to audit trail

---

#### WPLC-009: WatchPoint Automatic Archival (Event Mode)

**Actor**: System (Scheduled)
**Trigger**: Archiver job (EventBridge, every 15 minutes)

**Steps**:
1. Query Event Mode WatchPoints:
   ```sql
   SELECT * FROM watchpoints
   WHERE mode = 'event'
   AND status = 'active'
   AND time_window_end < now() - interval '1 hour'
   ```
2. For each:
   - Set status = 'archived'
   - Set archived_at = now()
   - Set archived_reason = 'event_passed'
3. Emit watchpoint.archived event
4. Trigger OBS-007 (post-event summary)

---

#### WPLC-010: Clone Archived WatchPoint

**Actor**: API
**Trigger**: POST /v1/watchpoints/{id}/clone

**Steps**:
1. Validate source WatchPoint exists
2. Copy all fields except:
   - id (generate new)
   - time_window (provided in request)
   - status (set to 'active')
   - created_at, updated_at
3. Reset evaluation_state
4. Validate new time_window
5. Insert as new WatchPoint
6. Return new WatchPoint

---

#### WPLC-011: Bulk WatchPoint Import

**Actor**: API
**Trigger**: POST /v1/watchpoints/bulk

**Steps**:
1. Validate array size (max 100)
2. For each WatchPoint in array:
   - Validate individually
   - Check limits (cumulative)
   - Prepare insert
3. Execute batch insert
4. Return: created IDs, validation errors per item

**Note**: Partial success allowed (returns both successes and failures)

---

#### WPLC-012: WatchPoint Validation Rejection

**Actor**: API
**Trigger**: Create/Update with invalid data

**Steps**:
1. Validation fails on any rule
2. Return 400/422 with detailed error:
   ```json
   {
     "error": {
       "code": "validation_failed",
       "message": "Invalid WatchPoint data",
       "details": {
         "conditions[0].threshold": "must be between 0 and 100 for humidity"
       }
     }
   }
   ```

---

### 4.2 User & Organization Management (USER)

---

#### USER-001: Organization Creation (Signup)

**Actor**: API
**Trigger**: New user registration

**Steps**:
1. Validate email uniqueness
2. Create organization record with default plan (free)
3. Create first user as 'owner' role
4. Initialize notification_preferences with defaults
5. Create Stripe customer (→ BILL-001)
6. Send welcome email
7. Return organization and user

---

#### USER-002: Organization Update

**Actor**: API
**Trigger**: PATCH /v1/organization

**Steps**:
1. Validate caller is owner or admin
2. Update allowed fields: name, billing_email, notification_preferences
3. Log to audit trail

---

#### USER-003: Organization Deletion (Soft Delete)

**Actor**: API
**Trigger**: Owner requests deletion

**Steps**:
1. Validate caller is owner
2. Soft delete: set deleted_at timestamp
3. Pause all WatchPoints
4. Cancel Stripe subscription
5. Data retained 30 days
6. After 30 days: MAINT-005 hard deletes

---

#### USER-004: User Invite

**Actor**: API
**Trigger**: POST /v1/users/invite

**Steps**:
1. Validate caller can invite (owner or admin)
2. Create user record with status = 'invited'
3. Generate invite token (expires 7 days)
4. Send invitation email with token link
5. Log to audit trail

---

#### USER-005: User Accepts Invite

**Actor**: API
**Trigger**: User clicks invite link, sets password

**Steps**:
1. Validate token not expired
2. Validate password meets requirements
3. Set status = 'active'
4. Store password_hash (bcrypt)
5. Create session
6. Return session token and user profile

---

#### USER-006: User Login (Password)

**Actor**: API
**Trigger**: POST /auth/login

**Steps**:
1. Lookup user by email
2. Check not locked out (→ SEC-001)
3. Validate password against hash
4. If invalid: increment failed_attempts, possibly lockout
5. If valid: reset failed_attempts
6. Create session token
7. Update last_login_at
8. Return session and user profile

---

#### USER-007: User Login (OAuth)

**Actor**: API
**Trigger**: OAuth callback from provider

**Steps**:
1. Exchange code for tokens with provider
2. Fetch user profile from provider
3. Match to existing user by email or provider_id
4. If new: create user and link
5. If existing: update link
6. Create session
7. Redirect to dashboard

---

#### USER-008: Password Reset

**Actor**: API
**Trigger**: POST /auth/forgot-password

**Steps**:
1. Lookup user by email
2. Generate reset token (expires 1 hour)
3. Send reset email with token link
4. On POST /auth/reset-password:
   - Validate token
   - Update password_hash
   - Invalidate all sessions
   - Return success

---

#### USER-009: User Role Change

**Actor**: API
**Trigger**: PATCH /v1/users/{id}

**Steps**:
1. Validate caller permissions:
   - Owner can change any role
   - Admin can change member roles
2. Cannot demote last owner
3. Update role
4. Log to audit trail

---

#### USER-010: User Removal

**Actor**: API
**Trigger**: DELETE /v1/users/{id}

**Steps**:
1. Validate caller permissions
2. Cannot remove last owner
3. Remove user from organization
4. Revoke all user's API keys
5. Invalidate all sessions
6. Log to audit trail

---

#### USER-011: API Key Creation

**Actor**: API
**Trigger**: POST /v1/api-keys

**Steps**:
1. Generate key: sk_live_xxx or sk_test_xxx
2. Store hash (bcrypt), not plaintext
3. Associate with creating user and organization
4. Return full key (once only)
5. Log to audit trail

---

#### USER-012: API Key Rotation

**Actor**: API
**Trigger**: POST /v1/api-keys/{id}/rotate

**Steps**:
1. Generate new key
2. Store new hash
3. Set grace_period_ends = now() + 24 hours
4. Both keys work during grace period
5. After grace: old key invalid
6. Log to audit trail

---

#### USER-013: API Key Revocation

**Actor**: API
**Trigger**: DELETE /v1/api-keys/{id}

**Steps**:
1. Set revoked_at timestamp
2. Key immediately invalid
3. Log to audit trail

---

#### USER-014: Notify Owner on Email Bounces

**Actor**: System (Event-Driven)
**Trigger**: Email channel reaches 3 bounces

**Steps**:
1. Lookup organization owner
2. Send email to owner explaining bounce issue
3. Include affected WatchPoint and channel
4. Suggest corrective action

---

### 4.3 Billing & Plans (BILL)

---

#### BILL-001: Stripe Customer Creation

**Actor**: System (Event-Driven)
**Trigger**: Organization created (USER-001)

**Steps**:
1. Call Stripe API to create customer
2. Use organization.billing_email
3. Store stripe_customer_id on organization

---

#### BILL-002: Subscription Creation (Upgrade from Free)

**Actor**: API
**Trigger**: User selects paid plan in dashboard

**Steps**:
1. Create Stripe Checkout Session
2. Include success_url and cancel_url
3. Return checkout URL to client
4. Client redirects to Stripe
5. On success webhook: update organization.plan and plan_limits
6. New limits effective immediately

---

#### BILL-003: Plan Upgrade (Mid-Cycle)

**Actor**: System (Event-Driven)
**Trigger**: Stripe webhook (subscription updated, upgrade)

**Steps**:
1. Stripe prorates charges automatically
2. Webhook received with new plan
3. Immediately update plan_limits
4. User can create more WatchPoints right away

---

#### BILL-004: Plan Downgrade

**Actor**: System (Event-Driven)
**Trigger**: Stripe webhook (subscription updated, downgrade)

**Steps**:
1. New limits effective at period end
2. If current usage exceeds new limits:
   - Start 14-day grace period
   - Send warning email
3. After grace: BILL-011 (auto-pause)

---

#### BILL-005: Payment Failure

**Actor**: System (Event-Driven)
**Trigger**: Stripe webhook (invoice.payment_failed)

**Steps**:
1. Mark organization with payment_failed_at
2. Start 7-day grace period
3. Notifications continue during grace
4. Send email to billing_email
5. After 7 days: BILL-006

---

#### BILL-006: Grace Period Expiration (Payment)

**Actor**: System (Scheduled)
**Trigger**: Scheduled job checks organizations with failed payment > 7 days

**Steps**:
1. Pause all WatchPoints
2. Send final warning email
3. Do NOT delete data
4. Can resume when payment resolves

---

#### BILL-007: Stripe Webhook Handler

**Actor**: API
**Trigger**: POST /v1/webhooks/stripe

**Steps**:
1. Verify Stripe signature
2. Parse event type
3. Route to appropriate handler:
   - checkout.session.completed → BILL-002
   - invoice.paid → BILL-013
   - invoice.payment_failed → BILL-005
   - customer.subscription.updated → BILL-003 or BILL-004
   - customer.subscription.deleted → Handle cancellation
4. Return 200 OK

---

#### BILL-008: WatchPoint Limit Check (At Creation)

**Actor**: System (Embedded)
**Trigger**: WatchPoint creation attempted

**Steps**:
1. Query current active WatchPoint count
2. Compare to plan_limits.watchpoints_max
3. If count >= limit:
   - Return 403 with upgrade_url
4. If under limit: allow creation

---

#### BILL-009: API Rate Limit Check

**Actor**: System (Embedded)
**Trigger**: Every API request

**Steps**:
1. Check daily API call count
2. Compare to plan_limits.api_calls_daily_max
3. At 100%: add warning header
4. At 150%: return 429
5. Resets at midnight UTC

---

#### BILL-010: Overage Warning (80% Threshold)

**Actor**: System (Event-Driven)
**Trigger**: Usage tracking detects 80% of any limit

**Steps**:
1. Check if warning already sent this billing cycle
2. If not: send warning email
3. Mark warning sent in organization metadata
4. One warning per limit type per cycle

---

#### BILL-011: Downgrade Grace Period Handling

**Actor**: System (Scheduled)
**Trigger**: Downgrade with usage > new limits + 14 days elapsed

**Steps**:
1. Query WatchPoints ordered by created_at ASC
2. Pause oldest WatchPoints until under limit
3. Send notification listing paused WatchPoints

---

#### BILL-012: Usage Tracking & Recording

**Actor**: System (Embedded)
**Trigger**: Various (API calls, notifications sent)

**Steps**:
1. Increment counters (Redis or DB)
2. Aggregate for /v1/usage endpoint
3. Store historical data for /v1/usage/history

---

#### BILL-013: Invoice Generation & Delivery

**Actor**: System (Event-Driven)
**Trigger**: Stripe webhook (invoice.paid)

**Steps**:
1. Store invoice reference
2. PDF URL from Stripe
3. Available via /v1/billing/invoices

---

## 5. Operational Flows

### 5.1 Observability & Verification (OBS)

---

#### OBS-001: Dead Man's Switch — Medium-Range Forecast

**Actor**: System (Passive)
**Trigger**: CloudWatch alarm (no ForecastReady metric for medium_range in 8 hours)

**Steps**:
1. TreatMissingDataAsBreaching catches silent failures
2. Alarm fires SNS notification
3. Ops team investigates
4. May indicate upstream data issue or RunPod failure

---

#### OBS-002: Dead Man's Switch — Nowcast Forecast

**Actor**: System (Passive)
**Trigger**: CloudWatch alarm (no ForecastReady metric for nowcast in 30 minutes)

**Steps**:
1. Same pattern as OBS-001
2. Tighter threshold for imminent alerts
3. Critical priority

---

#### OBS-003: Evaluation Queue Depth Alarm

**Actor**: System (Passive)
**Trigger**: CloudWatch alarm (queue depth > 100 for 10 minutes)

**Steps**:
1. Alarm fires
2. Ops investigates:
   - Slow workers?
   - Spike in WatchPoints?
   - Database contention?
3. May trigger EVAL-006

---

#### OBS-004: Dead Letter Queue Alarm

**Actor**: System (Passive)
**Trigger**: CloudWatch alarm (DLQ has any messages)

**Steps**:
1. Alarm fires
2. Requires manual investigation
3. Review failed message content
4. Fix issue and replay or discard

---

#### OBS-005: Forecast Verification Pipeline

**Actor**: System (Scheduled)
**Trigger**: Daily batch job

**Steps**:
1. Collect forecasts from 24-48 hours ago
2. Fetch observed weather data (NOAA ISD, MRMS)
3. Compare forecast vs observed
4. Compute metrics:
   - Bias
   - RMSE
   - Brier score (precipitation probability)
   - Hit rate, false alarm rate
5. Store results
6. Aggregate by lead time, region, season

---

#### OBS-006: Calibration Coefficient Update

**Actor**: System (Scheduled)
**Trigger**: Weekly batch job

**Steps**:
1. Analyze verification data
2. Calculate new precipitation probability calibration coefficients
3. Update calibration_coefficients table
4. Next nowcast uses new coefficients

---

#### OBS-007: Customer Post-Event Summary Generation

**Actor**: System (Event-Driven)
**Trigger**: Event Mode WatchPoint archived (WPLC-009)

**Steps**:
1. Fetch final forecast and evaluation history
2. Fetch observed weather for location and time window
3. Compare predicted vs actual
4. Generate summary:
   - "We predicted 30% rain, light rain occurred"
   - Verification metrics for this event
5. Store with archived WatchPoint

---

#### OBS-008: Canary WatchPoint Evaluation

**Actor**: System (Event-Driven)
**Trigger**: Every forecast_ready event

**Steps**:
1. Evaluate synthetic canary WatchPoints
2. Known locations with predictable conditions
3. Verify evaluation produces expected results
4. Alert if canary fails when expected to trigger (or vice versa)

---

#### OBS-009: Anomaly Detection — Trigger Rate

**Actor**: System (Continuous)
**Trigger**: Monitoring of notification rate

**Steps**:
1. Track notifications per minute/hour
2. Detect sudden spike (>3x normal)
3. Possible bug causing false alerts
4. May trigger automatic rate limiting
5. Alert ops for investigation

---

#### OBS-010: Metrics Emission (Distributed)

**Actor**: All Components
**Trigger**: Throughout all flows

**Metrics Emitted**:
- ForecastReady (Batcher)
- EvaluationCount, EvaluationDuration (Eval Workers)
- NotificationDelivered, NotificationFailed (Notification Workers)
- APIRequestCount, APILatency (API)

---

#### OBS-011: Audit Log Query

**Actor**: API
**Trigger**: GET /v1/audit-logs (internal/admin)

**Steps**:
1. Query audit_log table
2. Filter by: time range, actor, resource type, action
3. Return paginated results

---

### 5.2 Maintenance & Cleanup (MAINT)

---

#### MAINT-001: Forecast Data Tier Transition

**Actor**: System (Scheduled)
**Trigger**: Daily job

**Steps**:
1. Apply S3 lifecycle policies or explicit job:
   - Nowcasts: delete after 7 days
   - Medium-range: hot → warm after 24h
   - Medium-range: warm → cold after 7 days
   - Medium-range: cold → delete after 90 days
2. Update forecast_runs status

---

#### MAINT-002: Archived WatchPoint Cleanup

**Actor**: System (Scheduled)
**Trigger**: Daily job

**Steps**:
1. Query: status = 'archived' AND archived_at < now() - 90 days
2. Hard delete WatchPoint and evaluation_state
3. Notification history may be retained longer

---

#### MAINT-003: Notification History Retention

**Actor**: System (Scheduled)
**Trigger**: Monthly job

**Steps**:
1. Archive or delete notifications older than 1 year
2. Delete notification_deliveries older than 90 days

---

#### MAINT-004: Audit Log Retention & Archival

**Actor**: System (Scheduled)
**Trigger**: Monthly job

**Steps**:
1. 2-year retention requirement
2. After 2 years: archive to cold storage
3. Query interface spans both hot and archived

---

#### MAINT-005: Soft-Deleted Organization Cleanup

**Actor**: System (Scheduled)
**Trigger**: Daily job

**Steps**:
1. Query: deleted_at < now() - 30 days
2. Hard delete: organization, users, API keys, WatchPoints, notifications
3. Log for compliance audit trail
4. GDPR compliance

---

#### MAINT-006: Expired Invite Cleanup

**Actor**: System (Scheduled)
**Trigger**: Daily job

**Steps**:
1. Query: status = 'invited' AND invite_expires_at < now()
2. Delete expired invite records

---

#### MAINT-007: Stale Evaluation State Cleanup

**Actor**: System (Scheduled)
**Trigger**: Weekly job

**Steps**:
1. Find orphaned evaluation_state records
2. Clean up records where WatchPoint no longer exists
3. Trim oversized seen_threat_hashes arrays

---

### 5.3 Scheduled Jobs (SCHED)

---

#### SCHED-001: Rate Limit Counter Reset

**Actor**: System (Scheduled)
**Trigger**: Daily at midnight UTC

**Steps**:
1. Reset daily API call counters
2. If Redis: automatic via key expiry
3. If DB: explicit update

---

#### SCHED-002: Usage Aggregation Job

**Actor**: System (Scheduled)
**Trigger**: Hourly or daily

**Steps**:
1. Aggregate raw usage events
2. Calculate period totals
3. Populate usage_history table

---

#### SCHED-003: Stripe Subscription Sync

**Actor**: System (Scheduled)
**Trigger**: Daily

**Steps**:
1. Query Stripe for all active subscriptions
2. Compare to local plan data
3. Fix any discrepancies (missed webhooks)
4. Safety net, not primary sync

---

#### SCHED-004: Canary WatchPoint Verification

**Actor**: System (Scheduled)
**Trigger**: Every 30 minutes

**Steps**:
1. Check canary WatchPoint evaluation results
2. Alert if unexpected state
3. Complements OBS-008

---

#### SCHED-005: Dead Letter Queue Processing

**Actor**: Manual/Semi-automated
**Trigger**: Daily review

**Steps**:
1. Ops reviews DLQ messages
2. Options: replay after fix, or acknowledge and discard
3. Document handling procedure

---

#### SCHED-006: Webhook Health Pre-Check (Optional)

**Actor**: System (Scheduled)
**Trigger**: Daily or weekly

**Steps**:
1. Test webhook URLs with HEAD/OPTIONS
2. Mark channels as 'unhealthy' if unreachable
3. Warn organization before critical failure

---

## 6. API & Integration Flows

### 6.1 API Mechanics (API)

---

#### API-001: Request Authentication & Authorization

**Actor**: System (Embedded)
**Trigger**: Every API request

**Steps**:
1. Extract token from Authorization header
2. Determine type: sk_live_*, sk_test_*, or sess_*
3. Validate: not expired, not revoked, org exists
4. Check scope permissions against endpoint
5. Inject organization_id into request context
6. Proceed or return 401/403

---

#### API-002: Webhook URL Validation with SSRF Protection

**Actor**: System (Embedded)
**Trigger**: WatchPoint create/update with webhook channel

**Steps**:
1. Validate URL format (must be HTTPS)
2. Resolve DNS to get IP address
3. Check against blocklist:
   - localhost, 127.x.x.x
   - Private ranges: 10.x, 172.16-31.x, 192.168.x
   - Link-local: 169.254.x.x
   - Cloud metadata: 169.254.169.254
4. Reject if blocked
5. Allow if passes

---

#### API-003: Health Check Endpoint

**Actor**: API
**Trigger**: GET /v1/health

**Steps**:
1. Check database connectivity
2. Check S3 accessibility
3. Check queue accessibility
4. Return aggregate health status
5. Used by load balancers, monitoring

---

#### API-004: Idempotency Key Handling

**Actor**: System (Embedded)
**Trigger**: POST request with Idempotency-Key header

**Steps**:
1. Check if key seen in last 24 hours
2. If seen: return cached response
3. If new: process request, cache response
4. Prevents duplicate resource creation

---

### 6.2 Forecast Queries (FQRY)

---

#### FQRY-001: Point Forecast Retrieval

**Actor**: API
**Trigger**: GET /v1/forecasts/point

**Steps**:
1. Validate: location, time range, variables
2. Determine model(s):
   - Within 6h and CONUS: nowcast
   - Beyond 6h: medium-range
   - Overlap: blend both
3. Load Zarr chunk for tile
4. Extract point (bilinear interpolation)
5. Return with source attribution

---

#### FQRY-002: Batch Forecast Retrieval

**Actor**: API
**Trigger**: POST /v1/forecasts/points

**Steps**:
1. Validate: max 50 locations
2. Group by tile
3. Load each tile once
4. Extract multiple points
5. Return keyed by location ID

---

#### FQRY-003: Variable Availability Query

**Actor**: API
**Trigger**: GET /v1/forecasts/variables

**Steps**:
1. Return static list of available variables
2. Include: name, description, unit, range, models
3. Cacheable (changes only on deployment)

---

#### FQRY-004: Forecast Status/Health Query

**Actor**: API
**Trigger**: GET /v1/forecasts/status

**Steps**:
1. Query forecast_runs for latest per model
2. Calculate staleness
3. Return health status per model

---

#### FQRY-005: Forecast Data for WatchPoint

**Actor**: API (Embedded)
**Trigger**: GET /v1/watchpoints/{id}

**Steps**:
1. Include current_forecast in response
2. Reuse FQRY-001 logic
3. Cache per WatchPoint

---

### 6.3 Informational / Read (INFO)

---

#### INFO-001: Notification History Retrieval

**Actor**: API
**Trigger**: GET /v1/watchpoints/{id}/notifications

**Steps**:
1. Query notifications by watchpoint_id
2. Join with notification_deliveries
3. Order by created_at DESC
4. Paginate with cursor

---

#### INFO-002: Organization Notification History

**Actor**: API
**Trigger**: GET /v1/notifications

**Steps**:
1. Scope to organization_id
2. Filterable by: date range, event_type, watchpoint_id, urgency

---

#### INFO-003: Usage Dashboard Data

**Actor**: API
**Trigger**: GET /v1/usage

**Steps**:
1. Aggregate current period metrics
2. Calculate percentages
3. Near-real-time

---

#### INFO-004: Usage History Retrieval

**Actor**: API
**Trigger**: GET /v1/usage/history

**Steps**:
1. Query pre-aggregated data
2. Return time series

---

#### INFO-005: Invoice Retrieval

**Actor**: API
**Trigger**: GET /v1/billing/invoices

**Steps**:
1. Query Stripe or cached records
2. Return list with PDF URLs

---

#### INFO-006: Subscription Status Retrieval

**Actor**: API
**Trigger**: GET /v1/billing/subscription

**Steps**:
1. Query Stripe for current state
2. Return plan, status, dates

---

#### INFO-007: Forecast Verification Results Display

**Actor**: API
**Trigger**: GET /v1/forecasts/verification

**Steps**:
1. Query verification results from OBS-005
2. Aggregate by variable, region, lead time

---

#### INFO-008: WatchPoint Evaluation State Retrieval

**Actor**: API (Embedded)
**Trigger**: GET /v1/watchpoints/{id}

**Steps**:
1. Include evaluation_state in response
2. last_evaluated_at, trigger state, escalation

---

### 6.4 Dashboard / UI (DASH)

---

#### DASH-001: Session-Based Authentication (Login)

**Actor**: API
**Trigger**: POST /auth/login

**Steps**:
1. Validate email/password
2. Create session token (sess_xxx)
3. Set HTTP-only cookie
4. Include CSRF token
5. Return user profile

---

#### DASH-002: Session-Based Authentication (OAuth)

**Actor**: API
**Trigger**: GET /auth/oauth/{provider}/callback

**Steps**:
1. Exchange OAuth code
2. Create/link user
3. Create session
4. Redirect to dashboard

---

#### DASH-003: Session Refresh

**Actor**: System (Embedded)
**Trigger**: API request with near-expiry session

**Steps**:
1. Detect session near expiry
2. Extend session
3. Return new token in header

---

#### DASH-004: Session Logout

**Actor**: API
**Trigger**: POST /auth/logout

**Steps**:
1. Invalidate session
2. Clear cookie

---

#### DASH-005: Stripe Checkout Flow (Plan Upgrade)

**Actor**: API
**Trigger**: POST /v1/billing/checkout-session

**Steps**:
1. Create Stripe Checkout Session
2. Return checkout URL
3. User redirected to Stripe
4. On success: webhook updates plan

---

#### DASH-006: Stripe Customer Portal

**Actor**: API
**Trigger**: POST /v1/billing/portal-session

**Steps**:
1. Create Stripe Portal Session
2. Return portal URL
3. User manages billing on Stripe

---

#### DASH-007: Real-Time Notification Status (Optional)

**Actor**: API
**Trigger**: WebSocket connection

**Steps**:
1. Subscribe to organization events
2. Server pushes updates
3. Enables live updating feed
4. May be v2 feature

---

#### DASH-008: Dashboard-Specific List Views

**Actor**: API
**Trigger**: GET /v1/watchpoints (dashboard context)

**Steps**:
1. Include aggregated stats
2. More denormalized for rendering
3. Optimized for UI, not programmatic access

---

### 6.5 Webhook Operations (HOOK)

---

#### HOOK-001: Webhook Secret Rotation

**Actor**: API
**Trigger**: POST /v1/watchpoints/{id}/channels/{channel_id}/rotate-secret

**Steps**:
1. Generate new secret
2. Both secrets valid for 24 hours
3. Signature header includes both
4. After 24h: old secret invalid

---

#### HOOK-002: Webhook URL Update on Existing WatchPoint

**Actor**: API (Embedded)
**Trigger**: PATCH with changed webhook URL

**Steps**:
1. SSRF validation on new URL
2. If platform changes: formatting changes automatically
3. Pending notifications use OLD URL (already enqueued)

---

#### HOOK-003: Webhook Signature Verification (Customer-Side)

**Documentation Flow** (for customers):
```
1. Parse X-Watchpoint-Signature header
2. Extract timestamp and signature: t=...,v1=...
3. Reconstruct signed payload: {timestamp}.{body}
4. Compute HMAC-SHA256 with secret
5. Constant-time compare
6. Reject if timestamp > 5 minutes old
```

---

#### HOOK-004: Platform-Specific Webhook Deprecation Warning

**Actor**: System (Embedded)
**Trigger**: WatchPoint with deprecated URL pattern

**Steps**:
1. Detect deprecated pattern (e.g., old Teams connectors)
2. Return warning in API response
3. Include migration guidance

---

#### HOOK-005: Webhook Delivery Confirmation Tracking

**Actor**: System (Event-Driven)
**Trigger**: Successful webhook delivery

**Steps**:
1. Record: timestamp, response time, status code
2. Track success rate per channel
3. Feed into channel health scoring

---

### 6.6 Vertical App Integration (VERT)

---

#### VERT-001: Vertical App API Key Provisioning

**Actor**: API
**Trigger**: Vertical app developer onboards

**Steps**:
1. Create organization for vertical app
2. Create API key with specific scopes
3. source field tracks origin

---

#### VERT-002: WatchPoint Creation from Vertical App

**Actor**: API
**Trigger**: POST /v1/watchpoints (from vertical app)

**Steps**:
1. Same as WPLC-001/002
2. Additional fields:
   - source: "wedding_app"
   - template_set: "wedding"
   - metadata: vertical-specific JSON

---

#### VERT-003: Template Set Selection and Rendering

**Actor**: System (Embedded)
**Trigger**: Notification being formatted

**Steps**:
1. Lookup template_set from WatchPoint
2. Load template configuration
3. Apply placeholders
4. Fall back to "default" if not found

---

#### VERT-004: Vertical App Usage Attribution

**Actor**: System (Embedded)
**Trigger**: Usage tracking

**Steps**:
1. Group by source field
2. Enable per-vertical reporting

---

## 7. Supporting Flows

### 7.1 Validation (VALID)

---

#### VALID-001: Input Validation Pipeline

**Actor**: System (Embedded)
**Trigger**: WatchPoint create/update

**Validation Steps**:

1. **Location Validation**
   - Latitude: -90 to 90
   - Longitude: -180 to 180
   - CONUS check for nowcast eligibility

2. **Condition Validation**
   - Variable in supported set
   - Operator valid for threshold type
   - Threshold in variable's valid range
   - For `between`: threshold[0] < threshold[1]
   - Max 10 conditions

3. **Timezone Validation**
   - Valid IANA timezone string

4. **Time Window Validation**
   - Start before end
   - End not entirely in past

5. **Channel Validation**
   - Type supported
   - Email: valid format
   - Webhook: HTTPS, passes SSRF check

6. **Monitor Config Validation**
   - window_hours: 6-168
   - active_hours: valid ranges
   - active_days: 1-7

---

### 7.2 Security (SEC)

---

#### SEC-001: Brute Force Protection (Login)

**Actor**: System (Embedded)
**Trigger**: Failed login attempt

**Steps**:
1. Track failed attempts per email (sliding window)
2. After 5 failures in 15 min: 15 min lockout
3. After 10 failures: extended lockout, notify owner
4. Success resets counter

---

#### SEC-002: Brute Force Protection (API Key)

**Actor**: System (Embedded)
**Trigger**: Invalid API key used

**Steps**:
1. Track failed attempts per IP
2. After threshold: severe rate limit on IP
3. Prevents key enumeration

---

#### SEC-003: API Key Compromise Response

**Actor**: Manual
**Trigger**: Customer reports or ops detects suspicious activity

**Steps**:
1. Revoke compromised key immediately
2. Query audit logs for key usage
3. Identify actions taken
4. Notify organization owner
5. May need to undo attacker actions
6. Document incident

---

#### SEC-004: Suspicious Activity Detection

**Actor**: System (Continuous)
**Trigger**: Automated pattern monitoring

**Signals**:
- Unusual API volume
- WatchPoints in unusual locations
- Rapid key creation/deletion
- Unusual IP ranges

**Steps**:
1. Detect signal
2. Alert security team
3. May trigger SEC-003

---

#### SEC-005: Input Sanitization (Embedded)

**Actor**: System (Embedded)

**Measures**:
- SQL injection: parameterized queries only
- XSS: escape all output
- No user-provided file paths

---

#### SEC-006: Webhook Payload Signing (Outbound Security)

**Actor**: System (Embedded)

**Purpose**: Protects customers from spoofed webhooks

**Signature Format**:
```
X-Watchpoint-Signature: t=1612345678,v1=abc123...
```

---

### 7.3 Testing / Sandbox (TEST)

---

#### TEST-001: Test Mode API Behavior

**Actor**: System (Embedded)
**Trigger**: Request with sk_test_* key

**Behavior**:
- WatchPoints created with test_mode: true
- Plan limits consumed (tests limit logic)
- Notifications logged but not delivered
- No billing charges

---

#### TEST-002: Test Mode Notification Handling

**Actor**: System (Embedded)
**Trigger**: Notification for test WatchPoint

**Steps**:
1. Check test_mode flag
2. Log delivery details
3. Mark as 'delivered_test'
4. Skip actual HTTP/email

---

#### TEST-003: Test Mode WatchPoint Identification

**Actor**: System (Embedded)

**Implementation**:
- WatchPoints store test_mode boolean
- Dashboard filters by test flag
- Full evaluation pipeline runs

---

#### TEST-004: Test/Live Data Isolation

**Actor**: System (Configuration)

**Implementation**:
- Same database, filtered by test flag
- Dashboard toggle for test data view
- API responses include test_mode when true

---

### 7.4 Bulk Operations (BULK)

---

#### BULK-001: Bulk Import

See WPLC-011

---

#### BULK-002: Bulk Pause

**Actor**: API
**Trigger**: POST /v1/watchpoints/bulk/pause

**Steps**:
1. Accept filter criteria or ID list
2. Validate all belong to organization
3. Update status = 'paused' for all matching
4. Return count and IDs

---

#### BULK-003: Bulk Resume

**Actor**: API
**Trigger**: POST /v1/watchpoints/bulk/resume

**Steps**:
1. Inverse of BULK-002
2. Trigger immediate evaluation (batched)
3. Consolidate notifications to prevent flood

---

#### BULK-004: Bulk Delete

**Actor**: API
**Trigger**: POST /v1/watchpoints/bulk/delete

**Steps**:
1. Require confirmation or admin role
2. Soft/hard delete all matching
3. Audit log with full list

---

#### BULK-005: Bulk Tag Update

**Actor**: API
**Trigger**: PATCH /v1/watchpoints/bulk/tags

**Steps**:
1. Accept filter criteria
2. Tags to add, tags to remove
3. No config_version increment (tags don't affect eval)

---

#### BULK-006: Bulk Clone (Recurring Events)

**Actor**: API
**Trigger**: POST /v1/watchpoints/bulk/clone

**Steps**:
1. Accept list of archived WatchPoint IDs
2. Accept new time_window offset
3. Clone each with shifted times
4. Return old ID → new ID mapping

---

## 8. Failure & Recovery Flows

### 8.1 Failure Scenarios (FAIL)

---

#### FAIL-001: Database Primary Unavailable — Automatic Failover

**Actor**: Infrastructure
**Trigger**: Primary database unreachable

**Steps**:
1. Managed service promotes replica
2. Connection strings update (DNS or config)
3. Brief interruption, then normal operation

---

#### FAIL-002: Database Unavailable — Queue Writes for Replay

**Actor**: System (Event-Driven)
**Trigger**: All database connections fail

**Steps**:
1. Critical writes queued locally or to SQS
2. Replay when database recovers
3. Prevents data loss

---

#### FAIL-003: Database Connection Exhaustion

**Actor**: System (Infrastructure)
**Trigger**: Lambda concurrency exceeds pool

**Mitigations**:
- Lambda concurrency caps
- Connection pooling via Supabase (port 6543)
- Graceful 503 if exhausted

---

#### FAIL-004: GFS Data Unavailable

**Actor**: System (Event-Driven)
**Trigger**: Data poller cannot find GFS

**Steps**:
1. Try alternate mirrors (AWS, NCEP, Google)
2. If all fail: serve stale forecast
3. Alert after 2 hours

---

#### FAIL-005: GOES/Radar Data Unavailable

**Actor**: System (Event-Driven)
**Trigger**: Satellite/radar data missing

**Steps**:
1. Nowcast cannot run without input
2. Serve stale nowcast with warning
3. Fall back to medium-range for imminent alerts
4. Alert ops

---

#### FAIL-006: RunPod Inference Timeout

**Actor**: System (Event-Driven)
**Trigger**: GPU job exceeds duration

**Steps**:
1. Retry up to 3 times
2. If persistent: alert ops, serve stale

---

#### FAIL-007: RunPod Unavailable

**Actor**: System (Event-Driven)
**Trigger**: Cannot connect to RunPod API

**Steps**:
1. Retry with backoff
2. Could use alternative GPU provider
3. Alert ops

---

#### FAIL-008: Zarr File Corruption

**Actor**: System (Event-Driven)
**Trigger**: Eval worker cannot read Zarr chunk

**Steps**:
1. Retry fetch (transient S3 issue?)
2. If persistent: alert ops, skip this run
3. Previous forecast remains valid

---

#### FAIL-009: Bug Causing False Alerts (Kill Switch)

**Actor**: System (Event-Driven)
**Trigger**: OBS-009 detects spike

**Steps**:
1. Kill switch pauses ALL notification delivery
2. Notifications queued but not sent
3. Ops investigates and fixes
4. May send consolidated correction

---

#### FAIL-010: Bug Causing Missed Alerts

**Actor**: System (Event-Driven)
**Trigger**: Canary WatchPoints fail to trigger when expected

**Steps**:
1. Alert ops immediately
2. Manual investigation
3. May manually trigger notifications

---

#### FAIL-011: Lambda Function Error (Generic)

**Actor**: System (Event-Driven)
**Trigger**: Unhandled exception

**Steps**:
1. SQS message returns to queue
2. Retried up to 3 times
3. After max: moved to DLQ
4. OBS-004 fires

---

#### FAIL-012: Single AZ Failure

**Actor**: Infrastructure
**Trigger**: AWS AZ unavailable

**Steps**:
1. Automatic failover to other AZs
2. Multi-AZ database, Lambda runs in multiple AZs
3. Brief latency increase, then normal

---

#### FAIL-013: Region Failure — DR Failover

**Actor**: Infrastructure/Manual
**Trigger**: Entire AWS region unavailable

**Steps**:
1. Manual failover to DR region (RTO: 4 hours)
2. DR has: database replica, latest forecasts, deployable IaC
3. DNS update points traffic to DR

---

#### FAIL-014: S3 Unavailable

**Actor**: System (Event-Driven)
**Trigger**: Cannot read/write to forecast bucket

**Steps**:
1. Extremely rare (S3 is highly durable)
2. Retry with backoff
3. If persistent: alert ops, cannot generate new forecasts

---

#### FAIL-015: Stripe Unavailable

**Actor**: System (Event-Driven)
**Trigger**: Cannot reach Stripe API

**Steps**:
1. Billing operations fail gracefully
2. Existing subscriptions continue (local cache)
3. New signups/upgrades queued for retry
4. Alert ops if prolonged

---

### 8.2 Concurrency Handling (CONC)

---

#### CONC-001: Simultaneous PATCH to Same WatchPoint

**Resolution**: Last-write-wins with optional optimistic locking

**Steps**:
1. Default: second write overwrites first
2. Optional: If-Match header for conflict detection
3. Config version increments for both if conditions changed
4. Audit log captures both

---

#### CONC-002: Delete During Active Evaluation

**Resolution**: Graceful handling

**Steps**:
1. Eval Worker fetches WatchPoint, gets null
2. Skip evaluation, log warning
3. Already-queued notification still delivered

---

#### CONC-003: Pause During Active Evaluation

**Resolution**: Complete current, respect pause for next

**Steps**:
1. Complete current evaluation
2. Check status before delivery
3. If paused: suppress notification

---

#### CONC-004: Organization Downgrade During WatchPoint Creation

**Resolution**: Optimistic check

**Steps**:
1. Both try insert
2. One fails with limit error
3. Slight over-limit acceptable, corrected on next check

---

#### CONC-005: Config Update During Notification Delivery

**Resolution**: Deliver with original conditions

**Steps**:
1. Notification payload includes conditions at evaluation time
2. Next evaluation uses new conditions
3. No retroactive changes

---

#### CONC-006: Forecast Ready During Previous Evaluation Batch

**Resolution**: Queue both, process in order

**Steps**:
1. Batcher enqueues new batch
2. Workers process in order
3. WatchPoint evaluated twice (correct)
4. Notification deduplication prevents double-alert

---

### 8.3 Implementation Edge Cases (IMPL)

---

#### IMPL-001: Config Version Race Condition Resolution

See EVAL-004

---

#### IMPL-002: Hot Tile Sub-Batch Failure Handling

**Actor**: System (Event-Driven)
**Trigger**: One page of hot tile fails

**Steps**:
1. Each page is independent SQS message
2. Failed page retries independently
3. Successful pages don't re-run
4. Partial success acceptable

---

#### IMPL-003: Forecast Run Idempotency Check

**Actor**: System (Embedded)
**Trigger**: Batcher receives S3 event

**Steps**:
1. Check: run_timestamp already processed?
2. Query forecast_runs
3. If exists and complete: skip (duplicate)
4. If exists and running: skip (in progress)
5. If not exists: proceed

---

## 9. End-to-End Integration Flows

### INT-001: New User to First Alert

**Journey**: Complete onboarding to first notification

```
USER-001 (Org Creation)
    └── BILL-001 (Stripe Customer)
            └── USER-004/005 (User Setup)
                    └── WPLC-001 (Create WatchPoint)
                            └── [Next forecast_ready]
                                    └── EVAL-001 (Evaluation)
                                            └── [If triggered]
                                                    └── NOTIF-001 (Notification)
```

**Value**: Validates complete happy path for new customers

---

### INT-002: Forecast Generation to Customer Notification

**Journey**: Core value delivery loop

```
FCST-001/002 (Forecast Generated)
    └── S3 Event
            └── EVAL-001 (Batcher + Workers)
                    └── State Transitions
                            └── NOTIF-001 (Delivery)
```

**Value**: The primary value loop of the entire system

---

### INT-003: WatchPoint Complete Lifecycle

**Journey**: Full Event Mode journey

```
WPLC-001 (Create)
    └── Multiple EVAL cycles
            └── WPLC-005 (Pause)
                    └── WPLC-006 (Resume)
                            └── Event passes
                                    └── WPLC-009 (Archive)
                                            └── OBS-007 (Post-Event Summary)
                                                    └── WPLC-010 (Clone for next year)
```

**Value**: Shows full Event Mode journey including archival

---

### INT-004: Monitor Mode Daily Cycle

**Journey**: Ongoing monitoring pattern

```
WPLC-002 (Create Monitor)
    └── Daily EVAL-003 evaluations
            └── Threat deduplication
                    └── NOTIF-003 (Daily Digest)
                            └── Ongoing indefinitely until deleted
```

**Value**: Shows Monitor Mode's distinct operational pattern

---

## 10. Appendices

### Appendix A: Complete Flow Index

| ID | Name | Actor | Trigger |
|----|------|-------|---------|
| FCST-001 | Medium-Range Forecast Generation | Event | Data Poller detection |
| FCST-002 | Nowcast Forecast Generation | Event | Data Poller detection |
| FCST-003 | Data Poller Execution | Scheduled | Every 15 min |
| FCST-004 | Forecast Generation Retry | Event | Inference failure |
| FCST-005 | Fallback to Stale | Event | Retries exhausted |
| FCST-006 | Forecast Data Cleanup | Scheduled | Daily |
| EVAL-001 | Evaluation Pipeline | Event | S3 _SUCCESS |
| EVAL-002 | Hot Tile Pagination | Event | >500 WatchPoints |
| EVAL-003 | Monitor Mode Evaluation | Event | Same as EVAL-001 |
| EVAL-004 | Config Version Mismatch | Event | Version differs |
| EVAL-005 | First Evaluation | Event | New WatchPoint |
| EVAL-006 | Queue Backlog Recovery | Manual | Alarm fires |
| NOTIF-001 | Immediate Delivery | Event | Notification queued |
| NOTIF-002 | Quiet Hours Suppression | Event | During quiet hours |
| NOTIF-003 | Digest Generation | Scheduled | Per-org schedule |
| NOTIF-004 | Webhook Retry | Event | Delivery failure |
| NOTIF-005 | Rate Limit Handling | Event | 429 received |
| NOTIF-006 | Bounce Handling | Event | Bounce reported |
| NOTIF-007 | Platform Formatting | Embedded | (in NOTIF-001) |
| NOTIF-008 | Escalation | Event | Severity increased |
| NOTIF-009 | Cleared | Event | Conditions cleared |
| NOTIF-010 | All Channels Failed | Event | All fail |
| WPLC-001 | Create (Event Mode) | API | POST /watchpoints |
| WPLC-002 | Create (Monitor Mode) | API | POST /watchpoints |
| WPLC-003 | Update | API | PATCH /watchpoints/{id} |
| WPLC-004 | Update While Triggered | API | PATCH (triggered) |
| WPLC-005 | Pause | API | POST .../pause |
| WPLC-006 | Resume | API | POST .../resume |
| WPLC-007 | Resume with Backlog | API | Resume + backlog |
| WPLC-008 | Delete | API | DELETE /watchpoints/{id} |
| WPLC-009 | Automatic Archival | Scheduled | Every 15 min |
| WPLC-010 | Clone | API | POST .../clone |
| WPLC-011 | Bulk Import | API | POST /watchpoints/bulk |
| WPLC-012 | Validation Rejection | API | Invalid input |

*(Table continues for all ~160 flows...)*

---

### Appendix B: API Endpoint to Flow Mapping

| Endpoint | Method | Primary Flow(s) |
|----------|--------|-----------------|
| `/v1/watchpoints` | POST | WPLC-001, WPLC-002 |
| `/v1/watchpoints` | GET | INFO (list) |
| `/v1/watchpoints/{id}` | GET | INFO-008 |
| `/v1/watchpoints/{id}` | PATCH | WPLC-003, WPLC-004 |
| `/v1/watchpoints/{id}` | DELETE | WPLC-008 |
| `/v1/watchpoints/{id}/pause` | POST | WPLC-005 |
| `/v1/watchpoints/{id}/resume` | POST | WPLC-006, WPLC-007 |
| `/v1/watchpoints/{id}/clone` | POST | WPLC-010 |
| `/v1/watchpoints/{id}/notifications` | GET | INFO-001 |
| `/v1/watchpoints/bulk` | POST | WPLC-011 |
| `/v1/forecasts/point` | GET | FQRY-001 |
| `/v1/forecasts/points` | POST | FQRY-002 |
| `/v1/forecasts/variables` | GET | FQRY-003 |
| `/v1/forecasts/status` | GET | FQRY-004 |
| `/v1/organization` | GET | INFO |
| `/v1/organization` | PATCH | USER-002 |
| `/v1/users` | GET | INFO |
| `/v1/users/invite` | POST | USER-004 |
| `/v1/users/{id}` | PATCH | USER-009 |
| `/v1/users/{id}` | DELETE | USER-010 |
| `/v1/api-keys` | GET | INFO |
| `/v1/api-keys` | POST | USER-011 |
| `/v1/api-keys/{id}` | DELETE | USER-013 |
| `/v1/api-keys/{id}/rotate` | POST | USER-012 |
| `/v1/usage` | GET | INFO-003 |
| `/v1/usage/history` | GET | INFO-004 |
| `/v1/billing/invoices` | GET | INFO-005 |
| `/v1/billing/subscription` | GET | INFO-006 |
| `/v1/billing/checkout-session` | POST | DASH-005 |
| `/v1/billing/portal-session` | POST | DASH-006 |
| `/v1/notifications` | GET | INFO-002 |
| `/v1/webhooks/stripe` | POST | BILL-007 |
| `/v1/health` | GET | API-003 |
| `/auth/login` | POST | DASH-001 |
| `/auth/logout` | POST | DASH-004 |
| `/auth/forgot-password` | POST | USER-008 |
| `/auth/reset-password` | POST | USER-008 |
| `/auth/oauth/{provider}/callback` | GET | DASH-002 |
| `/auth/accept-invite` | POST | USER-005 |

---

### Appendix C: Database Table to Flow Mapping

| Table | Read By | Written By |
|-------|---------|------------|
| `organizations` | Most flows | USER-001, USER-002, BILL-* |
| `users` | USER-*, DASH-* | USER-001, USER-004, USER-005, USER-010 |
| `watchpoints` | EVAL-*, WPLC-*, INFO-* | WPLC-001 through WPLC-012 |
| `watchpoint_evaluation_state` | EVAL-001, EVAL-003 | EVAL-001, EVAL-003, EVAL-005 |
| `notifications` | NOTIF-*, INFO-001, INFO-002 | EVAL-001, EVAL-003, NOTIF-* |
| `notification_deliveries` | NOTIF-001 | NOTIF-001 through NOTIF-006 |
| `api_keys` | API-001 | USER-011, USER-012, USER-013 |
| `sessions` | DASH-* | DASH-001, DASH-002, DASH-004 |
| `audit_log` | OBS-011 | All mutating flows |
| `forecast_runs` | FCST-003, FQRY-004 | FCST-001, FCST-002, FCST-003 |
| `calibration_coefficients` | FCST-002 | OBS-006 |

---

### Appendix D: Error Code Reference

See `watchpoint-design-addendum-v3.md` Section 3 for complete error code reference.

---

### Appendix E: Glossary

| Term | Definition |
|------|------------|
| **WatchPoint** | The atomic unit of monitoring: Location × Time Window × Conditions × Channels |
| **Event Mode** | WatchPoint with finite time window (start/end) |
| **Monitor Mode** | WatchPoint with rolling window for ongoing surveillance |
| **Tile** | Geographic subdivision (64 global tiles) for batch processing |
| **Atlas** | Medium-range forecast model (1-15 days, global) |
| **StormScope** | Nowcast model (0-6 hours, CONUS only) |
| **Zarr** | Chunked array format for forecast data storage |
| **Batcher** | Component that groups WatchPoints by tile for evaluation |
| **Eval Worker** | Lambda function that evaluates conditions against forecasts |
| **Escalation** | Notification when conditions worsen while still triggered |
| **Threat Hash** | Deduplication key for Monitor Mode (prevents re-alerting same threat) |
| **Quiet Hours** | Time period when notifications are suppressed |
| **Config Version** | Incrementing counter to detect WatchPoint changes during evaluation |

---

*Document generated from 3-Question Expansion analysis conducted 2026-02-01*

*Total flows documented: ~160*

*Companion documents: watchpoint-platform-design-final.md, watchpoint-tech-stack-v3.3.md, watchpoint-design-addendum-v3.md*
