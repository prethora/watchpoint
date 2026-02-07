# WatchPoint Platform Design Specification

> A weather intelligence platform that monitors locations and times, evaluates conditions against forecasts, and delivers notifications when thresholds are crossed.

---

## Table of Contents

1. [Overview](#overview)
2. [Core Abstraction: The WatchPoint](#core-abstraction-the-watchpoint)
3. [Forecast System](#forecast-system)
4. [Evaluation Engine](#evaluation-engine)
5. [Notification System](#notification-system)
6. [Data Model](#data-model)
7. [API Design](#api-design)
8. [Authentication & Authorization](#authentication--authorization)
9. [Security](#security)
10. [Operations](#operations)
11. [Billing & Plans](#billing--plans)
12. [Observability & Monitoring](#observability--monitoring)
13. [Forecast Verification](#forecast-verification)
14. [Failure Scenarios & Resilience](#failure-scenarios--resilience)
15. [Edge Cases & Special Handling](#edge-cases--special-handling)
16. [System Boundaries](#system-boundaries)
17. [Integration Points](#integration-points)
18. [Architecture Diagram](#architecture-diagram)

---

## Overview

### What This System Does

The WatchPoint platform enables users to:

1. **Register a location and time window** (e.g., "Central Park, Saturday 3pm-8pm")
2. **Define weather conditions they care about** (e.g., "rain probability > 50%")
3. **Specify notification channels** (e.g., email, webhook)
4. **Receive alerts** when forecasts indicate their conditions will be met

### Key Design Principles

- **One forecast engine, many use cases**: We run global inference once, serve all customers
- **Horizontal platform, vertical applications**: Core is generic; vertical apps add domain-specific UX
- **Graceful degradation**: Stale data is better than no data; never silently fail
- **State-driven notifications**: Alert on state changes, not every evaluation

### Geographic Scope

- **Medium-range forecasts**: Global (but initially focused on US market)
- **Nowcast forecasts**: CONUS (Continental United States) only

---

## Core Abstraction: The WatchPoint

### Definition

A WatchPoint is the atomic unit of the system — a tuple of:

```
WatchPoint = Location × Time Window × Conditions × Notification Channels
```

Every feature resolves to one or more WatchPoints. WatchPoints are what get stored, evaluated, and trigger actions.

### Two Operating Modes

#### Event Mode

A WatchPoint with a **finite time window**. Has a clear start and end.

```
Lifecycle: created → active → event passes → archived
```

Example: "My wedding is Saturday 3pm. Alert me if rain probability exceeds 60%."

### WatchPoint Lifecycle Management

#### Automatic Archival (Event Mode)

A background job runs every 15 minutes to archive Event Mode WatchPoints whose time windows have passed:

```python
def archive_expired_watchpoints():
    cutoff = now() - timedelta(hours=1)  # 1 hour grace period
    
    expired = WatchPoint.query(
        status='active',
        time_window_end__lt=cutoff,
        time_window_end__isnot=None  # Event Mode only
    )
    
    for wp in expired:
        wp.status = 'archived'
        wp.archived_at = now()
        wp.archived_reason = 'event_passed'
        emit_event('watchpoint.archived', wp)
```

Archived WatchPoints:
- Stop being evaluated
- Remain queryable for 90 days
- Notifications history preserved
- Can be "cloned" to create new WatchPoint for future event

#### Pause/Resume Behavior

When a WatchPoint is **paused**:
- Evaluation stops immediately
- State is preserved (last_evaluated_at, trigger state, etc.)
- No notifications sent
- API returns `status: paused`

When a WatchPoint is **resumed**:
- Immediate evaluation triggered (don't wait for next cycle)
- If currently triggered, notification sent (subject to cooldowns)
- Evaluation state continues from where it left off

**Edge case**: WatchPoint paused during triggered state, resumed after conditions clear:
- On resume, evaluate immediately
- If no longer triggered, send "cleared" notification (if enabled)
- If still triggered, no notification (no state change)

#### Monitor Mode

A WatchPoint with **no end time** — ongoing surveillance until explicitly stopped.

```
Lifecycle: created → active → (paused ↔ active) → deleted
```

Example: "This is my construction site. Every day, tell me tomorrow's pour windows."

### Monitor Mode Mechanics

Monitor Mode WatchPoints require additional specification since they have no bounded time window.

#### Evaluation Window

Monitor Mode evaluates a **rolling window** into the future:

| Window Type | Default | Configurable |
|-------------|---------|--------------|
| `next_hours` | 24 hours | 6-168 hours |
| `specific_hours` | None | Array of hour ranges (e.g., `[[6,18]]` for 6am-6pm daily) |

```json
{
  "time_window": null,
  "monitor_config": {
    "window_type": "next_hours",
    "window_hours": 48,
    "active_hours": [[6, 18]],
    "active_days": [1, 2, 3, 4, 5]
  }
}
```

- `active_hours`: Only evaluate during these hours (local time). Default: all hours.
- `active_days`: Only evaluate on these days (1=Monday, 7=Sunday). Default: all days.

#### Notification Modes for Monitor

| Mode | Behavior |
|------|----------|
| `immediate` | Notify on each state change (same as Event Mode) |
| `digest` | Aggregate into scheduled summary |
| `both` | Immediate for threshold cross, digest for all-clear |

#### Digest Scheduling

```json
{
  "notification_preferences": {
    "monitor_mode": "digest",
    "digest_schedule": {
      "time": "07:00",
      "timezone": "America/New_York",
      "days": [1, 2, 3, 4, 5]
    }
  }
}
```

Digest contents:
- Forecast summary for the evaluation window
- Any conditions expected to trigger
- Comparison to previous digest ("Rain chance increased from 20% to 60%")

#### Daily Digest Timezone Clarification

**Daily digests are scheduled based on the USER's timezone, not the WatchPoint location's timezone.**

Rationale:
- Users want to receive digests at a time convenient for THEM
- A user might monitor locations in multiple timezones
- Consistency: all digests arrive at the same time for a given user

Configuration:

```json
{
  "digest_preferences": {
    "enabled": true,
    "frequency": "daily",
    "delivery_time": "07:00",
    "timezone": "America/New_York",
    "include_watchpoints": ["all"] 
  }
}
```

Behavior:

| User Timezone | WatchPoint Location | Digest Delivery Time |
|---------------|---------------------|----------------------|
| America/New_York | America/New_York | 07:00 ET |
| America/New_York | Asia/Tokyo | 07:00 ET |
| America/New_York | Europe/London | 07:00 ET |

When displaying times within the digest:

1. **Show times in WatchPoint's timezone** — "Rain expected 2pm-5pm local time (Tokyo)"
2. **Optionally show user's timezone** — "Rain expected 2pm-5pm Tokyo / 1am-4am ET"
3. **Always indicate the timezone** — Never show ambiguous times

Implementation:

```python
def schedule_daily_digest(organization):
    prefs = organization.digest_preferences
    
    # Parse delivery time in user's timezone
    user_tz = timezone(prefs['timezone'])
    delivery_time = time.fromisoformat(prefs['delivery_time'])
    
    # Calculate next delivery datetime
    now = datetime.now(user_tz)
    next_delivery = combine(now.date(), delivery_time)
    
    if next_delivery <= now:
        next_delivery += timedelta(days=1)
    
    # Schedule job
    scheduler.schedule(
        job_id=f"digest_{organization.id}",
        run_at=next_delivery,
        handler=generate_and_send_digest,
        args={'organization_id': organization.id}
    )
```

#### Monitor Mode Notification Example

```json
{
  "event_type": "monitor_digest",
  "watchpoint_name": "Downtown Construction Site",
  "digest_period": {
    "start": "2026-02-01T00:00:00Z",
    "end": "2026-02-02T00:00:00Z"
  },
  "forecast_summary": {
    "conditions_triggered": true,
    "triggered_periods": [
      {
        "start": "2026-02-01T14:00:00Z",
        "end": "2026-02-01T18:00:00Z",
        "precipitation_probability": 75,
        "precipitation_mm": 8
      }
    ],
    "safe_periods": [
      {
        "start": "2026-02-01T06:00:00Z",
        "end": "2026-02-01T14:00:00Z"
      }
    ]
  },
  "comparison_to_previous": {
    "precipitation_probability_change": +35,
    "direction": "worsening"
  }
}
```

### WatchPoint Record Structure

#### Required Fields

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier (e.g., `wp_8x7k2mNqPl`) |
| `organization_id` | string | Organization this belongs to |
| `location` | object | `{ lat, lon, display_name }` |
| `conditions` | array | At least one condition |
| `channels` | array | At least one notification channel |
| `status` | enum | `active`, `paused`, `archived` |
| `created_at` | timestamp | Creation time (UTC) |
| `updated_at` | timestamp | Last modification time (UTC) |

#### Optional Fields

| Field | Type | Description |
|-------|------|-------------|
| `time_window` | object | `{ start, end }` for Event Mode; null for Monitor Mode (stored as UTC) |
| `name` | string | Human-readable label |
| `timezone` | string | IANA timezone for display and digest timing (e.g., `America/New_York`) |
| `tags` | array | Freeform labels for organization |
| `metadata` | object | Vertical-specific data |
| `notification_preferences` | object | Throttling, quiet hours, escalation rules |
| `template_set` | string | Which notification templates to use |

Note: Evaluation state (last_evaluated_at, trigger state, etc.) is stored in a separate `watchpoint_evaluation_state` table to allow high-frequency updates without touching the main WatchPoint record.

### Timezone Handling

All timestamps follow these rules:

#### Storage

- **All timestamps stored as UTC** in the database
- `time_window.start` and `time_window.end` stored as UTC
- `timezone` field stores IANA timezone (e.g., `America/New_York`)

#### API Input

- Accept ISO 8601 with timezone offset: `2026-06-15T15:00:00-04:00`
- Accept ISO 8601 with Z (UTC): `2026-06-15T19:00:00Z`
- If no timezone provided, assume `timezone` field value, or UTC if not set

#### API Output

- Return UTC timestamps with `Z` suffix
- Include `timezone` field for client-side conversion

#### Display (Notifications, Digests)

- Convert to WatchPoint's `timezone` for human-readable display
- Use user's timezone for digest scheduling
- Handle DST transitions correctly (time may not exist or exist twice)

#### DST Edge Cases

```python
# Example: Event at 2:30 AM on DST transition day
# In America/New_York, 2:00 AM → 3:00 AM on March 9, 2026

# User inputs: "March 9, 2026 at 2:30 AM Eastern"
# This time doesn't exist! Two options:

# Option A (our choice): Shift forward to 3:30 AM
# Option B: Reject and ask user to clarify

# For "fall back" (November), 1:30 AM exists twice
# We use the first occurrence (before the transition)
```

### Condition Structure

```json
{
  "variable": "precipitation_probability",
  "operator": ">",
  "threshold": 50,
  "unit": "percent"
}
```

For the `between` operator, use an array threshold:

```json
{
  "variable": "temperature",
  "operator": "between",
  "threshold": [15, 25],
  "unit": "celsius"
}
```

The `between` operator is inclusive: `threshold[0] <= value <= threshold[1]`.

#### Supported Variables (Initial Set)

| Variable | Unit | Range | Description |
|----------|------|-------|-------------|
| `precipitation` | mm | 0-500 | Precipitation amount |
| `precipitation_probability` | % | 0-100 | Chance of precipitation |
| `temperature` | °C | -60 to 60 | Air temperature at 2m |
| `wind_speed` | km/h | 0-300 | Wind speed |
| `wind_gust` | km/h | 0-400 | Wind gust speed |
| `humidity` | % | 0-100 | Relative humidity |
| `cloud_cover` | % | 0-100 | Cloud cover percentage |
| `uv_index` | index | 0-15 | UV index |

#### Supported Operators

| Operator | Description | Threshold Type |
|----------|-------------|----------------|
| `>` | Greater than | Single number |
| `>=` | Greater than or equal | Single number |
| `<` | Less than | Single number |
| `<=` | Less than or equal | Single number |
| `==` | Equal to | Single number |
| `!=` | Not equal to | Single number |
| `between` | Inclusive range | Array of two numbers `[min, max]` |

#### Condition Logic

Multiple conditions combine via a `condition_logic` field:

- `ALL` — Every condition must be true (AND)
- `ANY` — At least one condition must be true (OR)

#### Validation Rules

1. `threshold` must be within the variable's valid range
2. For `between`, `threshold[0]` must be less than `threshold[1]`
3. `unit` must match the variable's expected unit
4. Maximum 10 conditions per WatchPoint

### Notification Channel Structure

```json
{
  "type": "email",
  "config": { "address": "user@example.com" },
  "enabled": true
}
```

#### Supported Channel Types (v1)

For the initial release, we support two channel types:

| Type | Config Fields | Description |
|------|---------------|-------------|
| `email` | `address` (string or array) | Direct email delivery |
| `webhook` | `url`, `secret` (optional), `headers` (optional) | HTTP POST to any URL |

**Future channels** (not in v1): SMS, native mobile push, direct Slack/Teams integrations via OAuth.

#### Webhook Channel with Platform Auto-Detection

The webhook channel automatically detects popular platforms and formats payloads appropriately:

| Platform | URL Pattern | Payload Format |
|----------|-------------|----------------|
| **Slack** | `hooks.slack.com/services/` | Block Kit |
| **Discord** | `discord.com/api/webhooks/` | Embeds |
| **Microsoft Teams** | `.webhook.office.com/` or `.logic.azure.com/` | Adaptive Cards |
| **Google Chat** | `chat.googleapis.com/` | Cards |
| **Generic** | Any other URL | Standard JSON |

This means users can paste their Slack, Discord, Teams, or Google Chat webhook URL directly — we handle the formatting automatically. No need for separate channel types per platform.

```json
{
  "type": "webhook",
  "config": {
    "url": "https://hooks.slack.com/services/T00000000/B00000000/XXXX",
    "secret": null
  },
  "enabled": true
}
```

The system detects this is a Slack URL and formats notifications using Slack's Block Kit format instead of our generic JSON.

---

## Forecast System

### Two Forecast Models

The system uses two NVIDIA Earth-2 models with fundamentally different characteristics:

#### Atlas (Medium-Range)

| Attribute | Value |
|-----------|-------|
| **Horizon** | 1-15 days |
| **Resolution** | 0.25° (~25km) |
| **Variables** | 70+ (temperature, wind, precipitation, humidity, etc.) |
| **Input Data** | GFS initial conditions (NOAA) |
| **Update Frequency** | Every 6 hours (when GFS updates) |
| **Inference Time** | ~2-5 minutes on GPU |
| **Coverage** | Global |

#### StormScope (Nowcast)

| Attribute | Value |
|-----------|-------|
| **Horizon** | 0-6 hours |
| **Resolution** | ~6km spatial, 10-minute temporal |
| **Variables** | Precipitation, radar reflectivity, storm motion |
| **Input Data** | GOES satellite imagery + ground radar |
| **Update Frequency** | Every 15-30 minutes |
| **Inference Time** | ~1-3 minutes on GPU |
| **Coverage** | CONUS only |

### Forecast Generation Pipeline

#### Medium-Range Pipeline

```
Trigger: GFS data availability (poll every 15 minutes)
         GFS publishes at ~00:00, 06:00, 12:00, 18:00 UTC
         Data available ~3.5 hours after run time

Sequence:
1. DETECT    - Poller sees new GFS run available
2. FETCH     - Download GFS initial conditions
3. INFERENCE - Run Atlas model on GPU
4. EXTRACT   - Extract variables we need
5. STORE     - Write to Zarr in forecast storage
6. SIGNAL    - Emit "medium_range_forecast_ready" event
7. EVALUATE  - Evaluation engine processes all WatchPoints
```

#### Nowcast Pipeline

```
Trigger: GOES satellite + radar data arrival (continuous)

Sequence:
1. DETECT    - New GOES CONUS scan + radar composite available
2. FETCH     - Download satellite bands + radar data
3. INFERENCE - Run StormScope model on GPU
4. EXTRACT   - Extract precipitation forecasts
5. STORE     - Write to Zarr in forecast storage
6. SIGNAL    - Emit "nowcast_forecast_ready" event
7. EVALUATE  - Evaluation engine processes imminent WatchPoints
```

### Forecast Storage

Forecasts stored as gridded data in Zarr format on object storage (S3/GCS):

```
/forecasts
  /medium_range
    /{run_timestamp}/forecast.zarr
      - time (dimension)
      - lat (dimension)
      - lon (dimension)
      - precipitation_mm (variable)
      - precipitation_probability (variable)
      - temperature_c (variable)
      - wind_speed_kmh (variable)
      - ... (other variables)
  /nowcast
    /{run_timestamp}/forecast.zarr
      - time (dimension, 10-min steps for 6 hours)
      - lat (dimension, CONUS only)
      - lon (dimension, CONUS only)
      - precipitation_mm (variable)
      - radar_reflectivity (variable)
```

### Variable Translation Layer

Different models output different native variables. We translate to a canonical schema:

#### Atlas Native → Canonical

| Atlas Output | Canonical Variable |
|--------------|-------------------|
| `tp` (total precipitation) | `precipitation_mm` |
| `t2m` (2m temperature) | `temperature_c` |
| `u10`, `v10` (wind components) | `wind_speed_kmh` (computed: √(u²+v²) × 3.6) |
| `r` (relative humidity) | `humidity_percent` |
| `tcc` (total cloud cover) | `cloud_cover_percent` |

#### StormScope Native → Canonical

StormScope outputs require more translation:

| StormScope Output | Translation | Canonical Variable |
|-------------------|-------------|-------------------|
| `radar_reflectivity` (dBZ) | Z-R relationship | `precipitation_mm` |
| `storm_motion_vector` | Direction + speed | `storm_direction_deg`, `storm_speed_kmh` |
| `precip_type` | Classification | `precipitation_type` (rain/snow/mix) |
| `echo_top_height` | Direct | `echo_top_km` |

#### Precipitation Probability from StormScope

StormScope doesn't directly output probability — it outputs deterministic precipitation forecasts. We derive probability using:

1. **Ensemble spread** (if running ensemble): Percentage of members showing precip > 0.1mm
2. **Calibrated confidence**: Apply location-specific calibration based on historical verification
3. **Threshold-based**: If precip_mm > 0.5mm, probability = 90%; if > 0.1mm, probability = 70%; else 20%

```python
def derive_precip_probability(nowcast_precip_mm, location, time):
    # Load calibration coefficients for this location
    cal = get_calibration(location)
    
    if nowcast_precip_mm >= cal.high_threshold:
        return min(95, cal.high_base + nowcast_precip_mm * cal.high_slope)
    elif nowcast_precip_mm >= cal.low_threshold:
        return cal.mid_base + nowcast_precip_mm * cal.mid_slope
    else:
        return max(5, cal.low_base)
```

Calibration coefficients are updated weekly based on verification data.

#### Variable Availability by Model

| Canonical Variable | Atlas | StormScope | Notes |
|-------------------|-------|------------|-------|
| `precipitation_mm` | ✓ | ✓ | StormScope higher resolution |
| `precipitation_probability` | ✓ | Derived | See above |
| `temperature_c` | ✓ | ✗ | Use Atlas only |
| `wind_speed_kmh` | ✓ | ✗ | Use Atlas only |
| `wind_gust_kmh` | ✓ | ✗ | Use Atlas only |
| `humidity_percent` | ✓ | ✗ | Use Atlas only |
| `cloud_cover_percent` | ✓ | ✗ | Use Atlas only |
| `uv_index` | ✓ | ✗ | Use Atlas only |
| `precipitation_type` | ✓ | ✓ | StormScope more accurate short-term |
| `radar_reflectivity` | ✗ | ✓ | Nowcast only |

When a variable is only available from one model, we use that model's data even outside its primary time range (with appropriate confidence flags).

### Data Retention

| Tier | Data | Retention |
|------|------|-----------|
| **Hot** | Latest forecasts, past 24h nowcasts | Always available |
| **Warm** | Past 7 days medium-range | Fast retrieval |
| **Cold** | Past 90 days medium-range | Archived |
| **Deleted** | Nowcasts > 7 days, medium-range > 90 days | Purged |

### Event Bus Message Schemas

Internal events drive the system. All events use a common envelope:

```json
{
  "event_id": "evt_abc123",
  "event_type": "forecast.medium_range.ready",
  "timestamp": "2026-01-31T12:15:00Z",
  "payload": { ... }
}
```

#### forecast.medium_range.ready

Emitted when a new medium-range forecast is stored and ready for evaluation.

```json
{
  "event_type": "forecast.medium_range.ready",
  "payload": {
    "run_id": "run_xyz789",
    "model": "atlas",
    "model_version": "1.2.0",
    "source_data_timestamp": "2026-01-31T06:00:00Z",
    "inference_completed_at": "2026-01-31T12:14:55Z",
    "storage_path": "s3://forecasts/medium_range/2026-01-31T06:00:00Z/forecast.zarr",
    "coverage": "global",
    "horizon_hours": 360,
    "variables": ["precipitation_mm", "precipitation_probability", "temperature_c", "..."]
  }
}
```

#### forecast.nowcast.ready

Emitted when a new nowcast forecast is stored.

```json
{
  "event_type": "forecast.nowcast.ready",
  "payload": {
    "run_id": "run_now456",
    "model": "stormscope",
    "model_version": "2.1.0",
    "source_data_timestamp": "2026-01-31T12:10:00Z",
    "inference_completed_at": "2026-01-31T12:14:30Z",
    "storage_path": "s3://forecasts/nowcast/2026-01-31T12:10:00Z/forecast.zarr",
    "coverage": "CONUS",
    "horizon_hours": 6,
    "variables": ["precipitation_mm", "precipitation_probability", "radar_reflectivity"]
  }
}
```

#### evaluation.batch.started

Emitted when evaluation engine begins processing a batch.

```json
{
  "event_type": "evaluation.batch.started",
  "payload": {
    "batch_id": "batch_eval123",
    "trigger": "forecast.medium_range.ready",
    "trigger_run_id": "run_xyz789",
    "watchpoint_count": 2847,
    "priority_breakdown": {
      "p0": 12,
      "p1": 156,
      "p2": 1893,
      "p3": 786
    }
  }
}
```

#### evaluation.batch.completed

Emitted when evaluation batch finishes.

```json
{
  "event_type": "evaluation.batch.completed",
  "payload": {
    "batch_id": "batch_eval123",
    "duration_ms": 34500,
    "watchpoints_evaluated": 2847,
    "notifications_queued": 47,
    "errors": 0
  }
}
```

#### notification.queued

Emitted when a notification is placed in the delivery queue.

```json
{
  "event_type": "notification.queued",
  "payload": {
    "notification_id": "notif_abc123",
    "watchpoint_id": "wp_8x7k2mNqPl",
    "organization_id": "org_def456",
    "event_type": "threshold_crossed",
    "urgency": "warning",
    "channels": ["email", "webhook"]
  }
}
```

#### notification.delivered / notification.failed

```json
{
  "event_type": "notification.delivered",
  "payload": {
    "notification_id": "notif_abc123",
    "delivery_id": "del_xyz789",
    "channel": "email",
    "provider": "ses",
    "provider_message_id": "sg_12345",
    "delivered_at": "2026-01-31T12:16:00Z"
  }
}
```

```json
{
  "event_type": "notification.failed",
  "payload": {
    "notification_id": "notif_abc123",
    "delivery_id": "del_xyz789",
    "channel": "webhook",
    "attempt": 3,
    "error_code": "timeout",
    "error_message": "Connection timed out after 10s",
    "will_retry": false
  }
}
```

#### watchpoint.archived

Emitted when a WatchPoint is automatically archived.

```json
{
  "event_type": "watchpoint.archived",
  "payload": {
    "watchpoint_id": "wp_8x7k2mNqPl",
    "organization_id": "org_def456",
    "reason": "event_passed",
    "event_end_time": "2026-01-31T20:00:00Z",
    "archived_at": "2026-02-01T00:15:00Z"
  }
}
```

### GPU Compute Strategy

**Scheduled serverless** (RunPod, Modal, or cloud spot instances):

| Model | Daily GPU Time | Cost Estimate |
|-------|----------------|---------------|
| Medium-range | ~20 minutes | ~$72/month |
| Nowcast | ~90 minutes | ~$324/month |
| **Total** | ~110 minutes | ~$400/month |

---

## Evaluation Engine

### Evaluation Triggers

| Trigger | Scope | Frequency |
|---------|-------|-----------|
| Medium-range forecast ready | ALL active WatchPoints | ~4x/day |
| Nowcast forecast ready | WatchPoints with time_window in next 6 hours | Every 15-30 min |

### Processing Strategy

**Batched parallel processing with prioritization:**

```
1. FORECAST READY signal arrives

2. QUERY active WatchPoints
   - Medium-range: ALL active
   - Nowcast: Only where time_window intersects next 6h

3. PRIORITIZE into queues:

   P0 CRITICAL (immediate):
     - Event starts within 2 hours
     - Previously triggered + still active

   P1 URGENT (< 1 min):
     - Event starts within 24 hours
     - Monitor mode marked "urgent"

   P2 STANDARD (< 5 min):
     - Event within 7 days
     - Monitor mode standard

   P3 BACKGROUND (< 15 min):
     - Event 7-15 days out

4. BATCH by geographic proximity
   - Group WatchPoints near each other
   - Load forecast chunk once, evaluate many
   - Batch size: ~100 WatchPoints

5. PARALLEL execution
   - Multiple workers process batches
   - Workers pull from priority queues (P0 first)
   - Target: 10,000 WatchPoints in < 60 seconds

6. AGGREGATE results
   - Collect triggered WatchPoints
   - Pass to notification decision layer
```

**Queue Routing**: The P0-P3 priority levels determine processing order *within* each physical queue. Nowcast-triggered evaluations always route to the Urgent queue (regardless of priority level), while medium-range evaluations route to the Standard queue. Within each queue, workers process P0 items first.

### Tile-Based Evaluation Architecture

Instead of evaluating each WatchPoint individually (which loads forecast data repeatedly), we group WatchPoints by geographic tile and evaluate in batches.

#### Tile Definition

Tiles align with Zarr chunk boundaries:
- **Size**: 22.5° latitude × 45° longitude
- **Count**: 8 × 8 = 64 tiles globally
- **Chunk**: Each tile corresponds to one Zarr chunk

```
Global tile grid (at equator):

     0°      45°     90°    135°    180°   225°    270°   315°   360°
    ┌───────┬───────┬───────┬───────┬───────┬───────┬───────┬───────┐
90° │ 0.0   │ 0.1   │ 0.2   │ 0.3   │ 0.4   │ 0.5   │ 0.6   │ 0.7   │
    ├───────┼───────┼───────┼───────┼───────┼───────┼───────┼───────┤
67° │ 1.0   │ 1.1   │ 1.2   │ 1.3   │ 1.4   │ 1.5   │ 1.6   │ 1.7   │
    ├───────┼───────┼───────┼───────┼───────┼───────┼───────┼───────┤
45° │ 2.0   │ 2.1   │ 2.2   │ 2.3   │ 2.4   │ 2.5   │ 2.6   │ 2.7   │
    ├───────┼───────┼───────┼───────┼───────┼───────┼───────┼───────┤
22° │ 3.0   │ 3.1   │ 3.2   │ 3.3   │ 3.4   │ 3.5   │ 3.6   │ 3.7   │
    ├───────┼───────┼───────┼───────┼───────┼───────┼───────┼───────┤
 0° │ 4.0   │ 4.1   │ 4.2   │ 4.3   │ 4.4   │ 4.5   │ 4.6   │ 4.7   │
    ├───────┼───────┼───────┼───────┼───────┼───────┼───────┼───────┤
-22°│ 5.0   │ 5.1   │ 5.2   │ 5.3   │ 5.4   │ 5.5   │ 5.6   │ 5.7   │
    ├───────┼───────┼───────┼───────┼───────┼───────┼───────┼───────┤
-45°│ 6.0   │ 6.1   │ 6.2   │ 6.3   │ 6.4   │ 6.5   │ 6.6   │ 6.7   │
    ├───────┼───────┼───────┼───────┼───────┼───────┼───────┼───────┤
-67°│ 7.0   │ 7.1   │ 7.2   │ 7.3   │ 7.4   │ 7.5   │ 7.6   │ 7.7   │
    └───────┴───────┴───────┴───────┴───────┴───────┴───────┴───────┘
-90°
```

#### Evaluation Flow

```
┌────────────────────────────────────────────────────────────────────────┐
│  1. FORECAST READY EVENT                                                │
│                                                                         │
│  S3 Event: _SUCCESS marker written                                     │
│  Payload: { bucket, prefix, forecast_type, run_timestamp }             │
└────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│  2. BATCHER LAMBDA                                                      │
│                                                                         │
│  a. Query DB: SELECT id, location_lat, location_lon                    │
│               FROM watchpoints                                          │
│               WHERE status = 'active'                                   │
│               AND (time_window_start IS NULL                           │
│                    OR time_window_start <= now() + interval '15 days') │
│                                                                         │
│  b. Group by tile:                                                      │
│     tiles = {}                                                          │
│     for wp in watchpoints:                                              │
│         tile_id = calculate_tile(wp.lat, wp.lon)                       │
│         tiles[tile_id].append(wp.id)                                   │
│                                                                         │
│  c. Enqueue per tile:                                                   │
│     for tile_id, wp_ids in tiles.items():                              │
│         queue = urgent_queue if forecast_type == 'nowcast'             │
│                 else standard_queue                                     │
│         queue.send({                                                    │
│             tile_id: tile_id,                                           │
│             watchpoint_ids: wp_ids,                                     │
│             run_timestamp: run_timestamp                                │
│         })                                                              │
└────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│  3. EVAL WORKER LAMBDA (per tile)                                       │
│                                                                         │
│  Input: { tile_id: "3.6", watchpoint_ids: [...], run_timestamp }       │
│                                                                         │
│  a. Load Zarr chunk for tile (one S3 GET, ~10-50 MB compressed)        │
│                                                                         │
│  b. Decompress and parse to float32 array                              │
│                                                                         │
│  c. Batch-fetch WatchPoint configs from DB                             │
│                                                                         │
│  d. For each WatchPoint in batch:                                       │
│     - Extract point data from chunk (fast, in-memory)                  │
│     - Evaluate conditions against forecast values                       │
│     - Determine if state changed                                        │
│     - If changed: prepare notification                                  │
│                                                                         │
│  e. Batch-update evaluation states in DB                               │
│                                                                         │
│  f. Batch-enqueue notifications to notification queue                  │
└────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│  4. NOTIFICATION WORKERS                                                │
│                                                                         │
│  - Email Worker: Sends via SES                                         │
│  - Webhook Worker: Auto-detects platform, formats, POSTs               │
│  - Both check quiet hours before sending                               │
└────────────────────────────────────────────────────────────────────────┘
```

#### Tile-Based Evaluation Benefits

| Benefit | Description |
|---------|-------------|
| **Memory efficiency** | Load one chunk (~50MB), evaluate 100s of WatchPoints |
| **Network efficiency** | One S3 GET per tile instead of per WatchPoint |
| **Parallelism** | Each tile processes independently, scales horizontally |
| **Cost reduction** | Fewer Lambda invocations, less data transfer |

#### Tile Edge Cases

**WatchPoint on tile boundary:**
- Use the tile containing the exact (lat, lon)
- Grid resolution (0.25°) handles interpolation

**Empty tiles:**
- Don't enqueue tiles with no active WatchPoints
- Batcher filters these out

**Large tiles:**
- If a tile has >1000 WatchPoints, split into sub-batches
- Prevents Lambda timeout

### Separate Evaluation Queues

**Nowcast alerts are urgent.** They indicate imminent weather (rain starting in 20 minutes). If these get stuck behind a large medium-range evaluation batch, users miss critical alerts.

#### Queue Configuration

| Queue | Purpose | Target Latency | Concurrency |
|-------|---------|----------------|-------------|
| `eval-queue-urgent` | Nowcast tiles | <60 seconds | Reserved 5 |
| `eval-queue-standard` | Medium-range tiles | <5 minutes | Auto-scale |

#### Routing Logic (in Batcher)

```python
def route_to_queue(forecast_type):
    if forecast_type == 'nowcast':
        return 'eval-queue-urgent'
    else:
        return 'eval-queue-standard'
```

#### Worker Configuration

Both queues use the same worker Lambda code, but:
- **Urgent workers**: Reserved concurrency (always available)
- **Standard workers**: Shared concurrency pool (cost-efficient)

```yaml
# SAM template
EvalWorkerUrgent:
  Type: AWS::Serverless::Function
  Properties:
    ReservedConcurrentExecutions: 5
    Events:
      SQSEvent:
        Type: SQS
        Properties:
          Queue: !GetAtt EvalQueueUrgent.Arn
          BatchSize: 1  # Process immediately

EvalWorkerStandard:
  Type: AWS::Serverless::Function
  Properties:
    # No reserved concurrency — shares pool
    Events:
      SQSEvent:
        Type: SQS
        Properties:
          Queue: !GetAtt EvalQueueStandard.Arn
          BatchSize: 10  # Batch for efficiency
```

#### Queue Monitoring

Alert if urgent queue age exceeds threshold:

```yaml
UrgentQueueAgeAlarm:
  Type: AWS::CloudWatch::Alarm
  Properties:
    MetricName: ApproximateAgeOfOldestMessage
    Namespace: AWS/SQS
    Dimensions:
      - Name: QueueName
        Value: !GetAtt EvalQueueUrgent.QueueName
    Threshold: 60  # seconds
    ComparisonOperator: GreaterThanThreshold
    EvaluationPeriods: 2
    Period: 60
    AlarmActions:
      - !Ref AlertTopic
```

### Evaluation Profiles by Time-to-Event

| Time Until Event | Model Used | Evaluation Frequency | Notification Sensitivity |
|------------------|------------|---------------------|-------------------------|
| 15+ days | Medium-range only | Every 6 hours | Significant change only |
| 7-15 days | Medium-range only | Every 6 hours | Threshold cross or change |
| 48h - 7 days | Medium-range | Every 6 hours | Any threshold cross |
| 6h - 48h | Both (medium primary) | 6h + 30min | Any threshold cross |
| 0 - 6h | Nowcast primary | Every 15 minutes | Immediate on trigger |
| During event | Nowcast only | Every 15 minutes | Critical only |
| Event passed | None | None | Optional summary |

### Evaluation State (Per WatchPoint)

```json
{
  "last_evaluated_at": "2026-01-31T12:15:00Z",
  "last_evaluation_result": {
    "triggered": false,
    "conditions_matched": [],
    "forecast_snapshot": { ... },
    "model_used": "medium_range"
  },
  "previous_trigger_state": false,
  "trigger_state_changed_at": "2026-01-30T06:00:00Z",
  "last_notified_at": "2026-01-30T06:00:00Z",
  "last_notified_state": "triggered",
  "notification_count_24h": 2,
  "last_forecast_summary": {
    "rain_probability": 25,
    "captured_at": "2026-01-31T06:00:00Z"
  },
  "escalation_level": 0
}
```

### Notification Decision Logic

```python
def should_notify(watchpoint, new_evaluation):
    state = watchpoint.evaluation_state

    # State transition: not triggered → triggered
    if new_evaluation.triggered and not state.previous_trigger_state:
        return { "notify": True, "reason": "newly_triggered" }

    # State transition: triggered → not triggered
    if not new_evaluation.triggered and state.previous_trigger_state:
        if watchpoint.notify_on_clear:
            return { "notify": True, "reason": "cleared" }

    # Still triggered but conditions worsened
    if new_evaluation.triggered and state.previous_trigger_state:
        if severity_increased(state, new_evaluation):
            if cooldown_passed(state.last_notified_at, ESCALATION_COOLDOWN):
                return { "notify": True, "reason": "escalated" }

    # Significant forecast change
    if forecast_significantly_changed(state.last_forecast_summary, new_evaluation):
        if cooldown_passed(state.last_notified_at, CHANGE_COOLDOWN):
            return { "notify": True, "reason": "forecast_changed" }

    return { "notify": False }
```

### Forecast Change Thresholds

A forecast change is "significant" when it exceeds these thresholds:

| Variable | Significant Change | Notes |
|----------|-------------------|-------|
| `precipitation_probability` | ±20 percentage points | e.g., 30% → 55% |
| `precipitation_mm` | ±5mm or ±50% (whichever is larger) | Relative for small amounts |
| `temperature_c` | ±5°C | |
| `wind_speed_kmh` | ±15 km/h or ±30% | |
| `wind_gust_kmh` | ±20 km/h or ±30% | |
| `humidity_percent` | ±20 percentage points | |
| `cloud_cover_percent` | ±30 percentage points | Less sensitive |

These thresholds apply to the **peak value** within the WatchPoint's time window, not moment-to-moment changes.

```python
def forecast_significantly_changed(previous, current):
    for variable in TRACKED_VARIABLES:
        prev_val = previous.get(variable)
        curr_val = current.get(variable)
        threshold = CHANGE_THRESHOLDS[variable]
        
        if abs(curr_val - prev_val) >= threshold.absolute:
            return True
        if threshold.relative and prev_val > 0:
            if abs(curr_val - prev_val) / prev_val >= threshold.relative:
                return True
    return False
```

### Escalation Logic

Escalation tracks **worsening severity** for already-triggered WatchPoints.

#### Escalation Levels

| Level | Meaning | Trigger |
|-------|---------|---------|
| 0 | Initial trigger | First time conditions met |
| 1 | Elevated | Conditions 25%+ worse than trigger threshold |
| 2 | Severe | Conditions 50%+ worse than trigger threshold |
| 3 | Critical | Conditions 100%+ worse than trigger threshold |

Example: If threshold is "rain > 50%", and forecast shows 80%, that's 60% over the 50% threshold = Level 2.

```python
def calculate_escalation_level(condition, actual_value):
    threshold = condition.threshold
    
    if condition.operator in ['>', '>=']:
        overage = (actual_value - threshold) / threshold
    elif condition.operator in ['<', '<=']:
        overage = (threshold - actual_value) / threshold
    else:
        return 0  # No escalation for equality operators
    
    if overage >= 1.0:
        return 3  # Critical
    elif overage >= 0.5:
        return 2  # Severe
    elif overage >= 0.25:
        return 1  # Elevated
    else:
        return 0  # Initial
```

#### Escalation Notification Rules

- Level increase triggers notification (with escalation cooldown)
- Level decrease does NOT trigger notification (only full clear does)
- Escalation cooldown: 2 hours minimum between escalation notifications
- Maximum escalation notifications per WatchPoint: 3 per 24 hours

---

## Notification System

### Notification Payload Structure

Every notification contains this canonical structure (channel-agnostic):

```json
{
  "notification_id": "notif_abc123",
  "watchpoint_id": "wp_8x7k2mNqPl",
  "watchpoint_name": "Sarah's Wedding",

  "event_type": "threshold_crossed",
  "triggered_at": "2026-01-31T14:00:00Z",

  "location": {
    "lat": 40.7128,
    "lon": -74.006,
    "display_name": "Central Park, NYC"
  },

  "time_window": {
    "start": "2026-06-15T15:00:00-04:00",
    "end": "2026-06-15T20:00:00-04:00"
  },
  "forecast_valid_for": "2026-06-15T15:00:00-04:00",

  "forecast_snapshot": {
    "precipitation_mm": 5,
    "precipitation_probability": 65,
    "temperature_c": 22,
    "wind_speed_kmh": 18
  },

  "conditions_evaluated": [
    {
      "variable": "precipitation_probability",
      "operator": ">",
      "threshold": 50,
      "actual_value": 65,
      "matched": true
    }
  ],

  "urgency": "warning",
  "source_model": "medium_range"
}
```

### Event Types

| Type | Description |
|------|-------------|
| `threshold_crossed` | Conditions just became true |
| `threshold_cleared` | Conditions just became false |
| `forecast_changed` | Significant change even without crossing |
| `imminent_alert` | Nowcast detected approaching weather |

### Urgency Levels

| Level | Meaning |
|-------|---------|
| `routine` | Informational, no immediate action needed |
| `watch` | Pay attention, conditions developing |
| `warning` | Action likely needed |
| `critical` | Immediate attention required |

### Quiet Hours (Do Not Disturb)

Users can configure "quiet hours" to suppress non-critical notifications during specific times. This prevents alert fatigue and respects user boundaries (e.g., no 3 AM alerts for a 20% rain chance 5 days out).

#### Configuration

Add to `notification_preferences` object:

```json
{
  "notification_preferences": {
    "quiet_hours": {
      "enabled": true,
      "schedule": [
        {
          "days": ["mon", "tue", "wed", "thu", "fri"],
          "start": "22:00",
          "end": "07:00"
        },
        {
          "days": ["sat", "sun"],
          "start": "23:00",
          "end": "09:00"
        }
      ],
      "timezone": "America/New_York",
      "exceptions": {
        "allow_urgency": ["critical"],
        "allow_escalation_level_gte": 3,
        "allow_event_mode_within_hours": 24
      }
    },
    "cooldown_minutes": 120,
    "escalation_enabled": true
  }
}
```

#### Quiet Hours Fields

| Field | Type | Description |
|-------|------|-------------|
| `enabled` | boolean | Master switch for quiet hours |
| `schedule` | array | List of quiet periods with days and times |
| `schedule[].days` | array | Days of week (`mon`, `tue`, etc.) |
| `schedule[].start` | string | Start time in 24h format (HH:MM) |
| `schedule[].end` | string | End time in 24h format (HH:MM) |
| `timezone` | string | IANA timezone for evaluating quiet hours |
| `exceptions.allow_urgency` | array | Urgency levels that bypass quiet hours |
| `exceptions.allow_escalation_level_gte` | integer | Escalation level that bypasses quiet hours |
| `exceptions.allow_event_mode_within_hours` | integer | Event Mode WatchPoints bypass when event is within N hours |

#### Quiet Hours Decision Logic

```python
def should_suppress_notification(notification, preferences):
    qh = preferences.get('quiet_hours', {})
    
    # Feature disabled
    if not qh.get('enabled', False):
        return False
    
    # Get current time in user's timezone
    user_tz = timezone(qh.get('timezone', 'UTC'))
    now = datetime.now(user_tz)
    current_day = now.strftime('%a').lower()
    current_time = now.strftime('%H:%M')
    
    # Check if we're in a quiet period
    in_quiet_period = False
    for period in qh.get('schedule', []):
        if current_day in period['days']:
            if is_time_in_range(current_time, period['start'], period['end']):
                in_quiet_period = True
                break
    
    if not in_quiet_period:
        return False
    
    # Check exceptions
    exceptions = qh.get('exceptions', {})
    
    # Exception: Critical urgency bypasses
    if notification.urgency in exceptions.get('allow_urgency', []):
        return False
    
    # Exception: High escalation level bypasses
    min_escalation = exceptions.get('allow_escalation_level_gte', 999)
    if notification.escalation_level >= min_escalation:
        return False
    
    # Exception: Event Mode within threshold bypasses
    event_threshold = exceptions.get('allow_event_mode_within_hours')
    if event_threshold and notification.watchpoint.is_event_mode:
        hours_until_event = (notification.watchpoint.time_window.start - now).hours
        if hours_until_event <= event_threshold:
            return False
    
    # Suppress this notification
    return True
```

#### Suppressed Notification Handling

When a notification is suppressed:

1. **Log the suppression** — Record that notification was delayed due to quiet hours
2. **Calculate resume time** — End of current quiet period
3. **Option A: Defer** — Reschedule for delivery at resume time
4. **Option B: Batch** — Aggregate with other suppressed notifications into morning digest
5. **Option C: Drop** — For low-priority, time-sensitive info that's stale by morning

Default behavior: **Defer** for `warning`/`critical` urgency, **Batch** for `info`.

#### Quiet Hours API

**Get quiet hours status:**
```
GET /v1/organization/notification-preferences
```

**Update quiet hours:**
```
PATCH /v1/organization/notification-preferences
{
  "quiet_hours": {
    "enabled": true,
    "schedule": [...],
    "timezone": "America/New_York"
  }
}
```

### Delivery Pipeline

```
Evaluation Engine
       │
       ▼
┌──────────────────┐
│  Notification    │  ← Applies spam prevention, cooldowns, quiet hours
│  Decision Layer  │
└────────┬─────────┘
         │
         ▼
┌──────────────────┐
│  Notification    │  ← Persistent queue (one per channel type)
│  Queue           │
└────────┬─────────┘
         │
    ┌────┴────┐
    ▼         ▼
 Webhook   Email
 Worker    Worker
    │         │
    ▼         ▼
┌─────────────────────────────────────────┐
│  Delivery Status Tracker                │
│  (success, failed, retrying, bounced)   │
└─────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────┐
│  External Providers                     │
│  • Webhook: Direct HTTP POST            │
│  • Email: AWS SES                       │
└─────────────────────────────────────────┘
```

The webhook worker handles all webhook deliveries, including auto-detection and platform-specific formatting for Slack, Discord, Teams, and Google Chat.

### Spam Prevention

1. **State-based triggering**: Only notify on state transitions
2. **Cooldown periods**: Minimum interval between notifications (default: 1 hour medium-range, 30 min nowcast)
3. **Digest mode**: Option to batch notifications into daily/weekly summaries

### Failure Handling by Channel

| Channel | Retry Strategy | Timeout |
|---------|----------------|---------|
| Webhook | 3 attempts, exponential backoff (1s, 5s, 30s) | 10 seconds |
| Email | Delegate to provider retry logic | N/A |

### Webhook Platform Specifications

#### Platform Detection Logic

```go
func detectPlatform(url string) string {
    if strings.Contains(url, "hooks.slack.com/services/") {
        return "slack"
    }
    if strings.Contains(url, "discord.com/api/webhooks/") {
        return "discord"
    }
    if strings.Contains(url, ".webhook.office.com/") || 
       strings.Contains(url, ".logic.azure.com/") {
        return "teams"
    }
    if strings.Contains(url, "chat.googleapis.com/") {
        return "google_chat"
    }
    return "generic"
}
```

#### Slack Payload Format

URL Pattern: `https://hooks.slack.com/services/{workspace}/{channel}/{token}`

```json
{
  "text": "⚠️ Weather Alert: Sarah's Wedding",
  "blocks": [
    {
      "type": "header",
      "text": {
        "type": "plain_text",
        "text": "⚠️ Weather Alert"
      }
    },
    {
      "type": "section",
      "text": {
        "type": "mrkdwn",
        "text": "*Sarah's Wedding*\nRain probability increased to *65%*"
      }
    },
    {
      "type": "section",
      "fields": [
        { "type": "mrkdwn", "text": "*Location:*\nCentral Park, NYC" },
        { "type": "mrkdwn", "text": "*Time:*\nJune 15, 3-8pm" }
      ]
    },
    {
      "type": "context",
      "elements": [
        { "type": "mrkdwn", "text": "Source: Medium-range forecast • Updated 2 hours ago" }
      ]
    }
  ]
}
```

Notes:
- Webhooks must be created via Slack Apps (legacy custom integrations deprecated)
- Cannot override channel/username/icon at runtime
- `text` field required as fallback for notifications

#### Discord Payload Format

URL Pattern: `https://discord.com/api/webhooks/{id}/{token}`

```json
{
  "content": "⚠️ Weather Alert",
  "embeds": [
    {
      "title": "Sarah's Wedding",
      "description": "Rain probability increased to 65%",
      "color": 16744256,
      "fields": [
        { "name": "Location", "value": "Central Park, NYC", "inline": true },
        { "name": "Time", "value": "June 15, 3-8pm", "inline": true },
        { "name": "Precipitation", "value": "65%", "inline": true }
      ],
      "footer": {
        "text": "Medium-range forecast • Updated 2 hours ago"
      }
    }
  ],
  "username": "WatchPoint",
  "avatar_url": "https://watchpoint.io/icon.png"
}
```

Notes:
- Most straightforward API, no deprecation concerns
- Can override `username` and `avatar_url` per request
- Color is decimal (16744256 = orange #FFA500)
- Rate limit: ~30 messages/minute, returns HTTP 429 with `Retry-After`

#### Microsoft Teams Payload Format (Adaptive Cards)

URL Patterns: 
- Old (deprecated March 2026): `https://*.webhook.office.com/webhookb2/...`
- New (Workflow): `https://*.logic.azure.com/workflows/...`

```json
{
  "type": "message",
  "attachments": [
    {
      "contentType": "application/vnd.microsoft.card.adaptive",
      "content": {
        "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
        "type": "AdaptiveCard",
        "version": "1.2",
        "body": [
          {
            "type": "TextBlock",
            "text": "⚠️ Weather Alert: Sarah's Wedding",
            "weight": "bolder",
            "size": "large"
          },
          {
            "type": "TextBlock",
            "text": "Rain probability increased to 65%",
            "wrap": true
          },
          {
            "type": "FactSet",
            "facts": [
              { "title": "Location", "value": "Central Park, NYC" },
              { "title": "Time", "value": "June 15, 3-8pm" },
              { "title": "Precipitation", "value": "65%" }
            ]
          }
        ]
      }
    }
  ]
}
```

**Important: Microsoft Teams Transition**
- Office 365 Connectors retiring March 31, 2026
- Users must migrate to Power Automate Workflow webhooks
- We detect both URL patterns and use Adaptive Card format
- Consider warning users if they provide old-style connector URLs

#### Google Chat Payload Format

URL Pattern: `https://chat.googleapis.com/v1/spaces/.../messages?key=...&token=...`

```json
{
  "cards": [
    {
      "header": {
        "title": "⚠️ Weather Alert",
        "subtitle": "Sarah's Wedding"
      },
      "sections": [
        {
          "widgets": [
            {
              "textParagraph": {
                "text": "Rain probability increased to <b>65%</b>"
              }
            },
            {
              "keyValue": {
                "topLabel": "Location",
                "content": "Central Park, NYC"
              }
            },
            {
              "keyValue": {
                "topLabel": "Time",
                "content": "June 15, 3-8pm"
              }
            }
          ]
        }
      ]
    }
  ]
}
```

Notes:
- Requires Google Workspace (Business/Enterprise) — not consumer Gmail
- Simpler card format than other platforms
- Basic HTML supported in `textParagraph` (`<b>`, `<i>`, `<a>`)

#### Generic Webhook Payload Format

For any URL not matching the above platforms, we send our standard payload:

```json
{
  "notification_id": "notif_abc123",
  "watchpoint_id": "wp_8x7k2mNqPl",
  "watchpoint_name": "Sarah's Wedding",
  "event_type": "threshold_crossed",
  "triggered_at": "2026-01-31T14:00:00Z",
  "location": {
    "lat": 40.7128,
    "lon": -74.006,
    "display_name": "Central Park, NYC"
  },
  "time_window": {
    "start": "2026-06-15T15:00:00-04:00",
    "end": "2026-06-15T20:00:00-04:00"
  },
  "forecast_snapshot": {
    "precipitation_probability": 65,
    "precipitation_mm": 5,
    "temperature_c": 22
  },
  "conditions_evaluated": [
    {
      "variable": "precipitation_probability",
      "operator": ">",
      "threshold": 50,
      "actual_value": 65,
      "matched": true
    }
  ],
  "urgency": "warning",
  "source_model": "medium_range"
}
```

This format is ideal for:
- Custom integrations
- Zapier, Make, IFTTT (they transform the data as needed)
- Internal systems that parse JSON

### Webhook Signature Specification

All webhook deliveries include a signature for verification.

#### Headers Sent

```
Content-Type: application/json
X-Watchpoint-Signature: t=1706745600,v1=5d41402abc4b2a76b9719d911017c592
X-Watchpoint-Event: threshold_crossed
X-Watchpoint-Delivery-Id: del_xyz789
```

#### Signature Format

```
X-Watchpoint-Signature: t={timestamp},v1={signature}
```

- `t`: Unix timestamp when signature was generated
- `v1`: HMAC-SHA256 signature (hex encoded)

#### Signature Computation

```python
import hmac
import hashlib
import time

def compute_signature(payload_json: str, secret: str, timestamp: int) -> str:
    # Construct the signed payload
    signed_payload = f"{timestamp}.{payload_json}"
    
    # Compute HMAC-SHA256
    signature = hmac.new(
        secret.encode('utf-8'),
        signed_payload.encode('utf-8'),
        hashlib.sha256
    ).hexdigest()
    
    return f"t={timestamp},v1={signature}"
```

#### Verification (Receiver Side)

```python
def verify_webhook(payload: str, signature_header: str, secret: str) -> bool:
    # Parse header
    parts = dict(p.split('=', 1) for p in signature_header.split(','))
    timestamp = int(parts['t'])
    received_sig = parts['v1']
    
    # Reject if timestamp is too old (prevent replay attacks)
    if abs(time.time() - timestamp) > 300:  # 5 minute tolerance
        return False
    
    # Compute expected signature
    signed_payload = f"{timestamp}.{payload}"
    expected_sig = hmac.new(
        secret.encode('utf-8'),
        signed_payload.encode('utf-8'),
        hashlib.sha256
    ).hexdigest()
    
    # Constant-time comparison
    return hmac.compare_digest(received_sig, expected_sig)
```

#### Webhook Secret Rotation

Webhook secrets can be rotated via API. During rotation:
- Both old and new secrets are valid for 24 hours
- Signature header may include both: `t=...,v1=...,v1_old=...`
- Receiver should check `v1` first, fall back to `v1_old`

### Template Sets

Templates are configuration, not code. Each WatchPoint has a `template_set` field:

```
Template Set: "wedding"
  → "Your event {watchpoint_name} may see rain..."

Template Set: "construction"
  → "Pour window alert for {watchpoint_name}..."

Template Set: "default"
  → "Weather alert for {watchpoint_name}..."
```

---

## Data Model

### Database Tables

#### organizations

```sql
CREATE TABLE organizations (
  id                  VARCHAR PRIMARY KEY,  -- "org_xxxxx"
  name                VARCHAR NOT NULL,
  billing_email       VARCHAR UNIQUE NOT NULL,
  plan                VARCHAR NOT NULL,     -- free, starter, pro, business
  plan_limits         JSONB,                -- { watchpoints_max, api_calls_max }
  stripe_customer_id  VARCHAR,
  notification_preferences JSONB DEFAULT '{
    "quiet_hours": {
      "enabled": false,
      "schedule": [],
      "timezone": "UTC",
      "exceptions": {
        "allow_urgency": ["critical"],
        "allow_escalation_level_gte": 3,
        "allow_event_mode_within_hours": 24
      }
    },
    "digest": {
      "enabled": false,
      "frequency": "daily",
      "delivery_time": "07:00",
      "timezone": "UTC"
    }
  }',
  created_at          TIMESTAMP NOT NULL,
  updated_at          TIMESTAMP NOT NULL
);

CREATE INDEX idx_org_stripe ON organizations(stripe_customer_id);
```

#### users

```sql
CREATE TABLE users (
  id                  VARCHAR PRIMARY KEY,  -- "user_xxxxx"
  organization_id     VARCHAR NOT NULL REFERENCES organizations(id),
  email               VARCHAR UNIQUE NOT NULL,
  password_hash       VARCHAR,              -- nullable if OAuth
  role                VARCHAR NOT NULL,     -- owner, admin, member
  created_at          TIMESTAMP NOT NULL,
  last_login_at       TIMESTAMP
);

CREATE INDEX idx_users_org ON users(organization_id);
```

#### api_keys

```sql
CREATE TABLE api_keys (
  id                  VARCHAR PRIMARY KEY,  -- "key_xxxxx"
  organization_id     VARCHAR NOT NULL REFERENCES organizations(id),
  created_by_user_id  VARCHAR REFERENCES users(id),
  key_hash            VARCHAR UNIQUE NOT NULL,
  key_prefix          VARCHAR NOT NULL,     -- "sk_live_abc..." for display
  name                VARCHAR,
  scopes              VARCHAR[] NOT NULL,
  last_used_at        TIMESTAMP,
  expires_at          TIMESTAMP,
  revoked_at          TIMESTAMP,
  created_at          TIMESTAMP NOT NULL
);

CREATE INDEX idx_apikeys_org ON api_keys(organization_id, revoked_at);
```

#### watchpoints

```sql
CREATE TABLE watchpoints (
  id                    VARCHAR PRIMARY KEY,  -- "wp_xxxxx"
  organization_id       VARCHAR NOT NULL REFERENCES organizations(id),
  name                  VARCHAR(200),
  location_lat          DECIMAL NOT NULL CHECK (location_lat BETWEEN -90 AND 90),
  location_lon          DECIMAL NOT NULL CHECK (location_lon BETWEEN -180 AND 180),
  location_display_name VARCHAR(200),
  timezone              VARCHAR(50),
  time_window_start     TIMESTAMP,            -- nullable for monitor mode
  time_window_end       TIMESTAMP,
  monitor_config        JSONB,                -- for monitor mode: window_hours, active_hours, etc.
  conditions            JSONB NOT NULL,
  condition_logic       VARCHAR NOT NULL CHECK (condition_logic IN ('ANY', 'ALL')),
  channels              JSONB NOT NULL,
  preferences           JSONB,
  template_set          VARCHAR DEFAULT 'default',
  tags                  VARCHAR(50)[],
  metadata              JSONB,
  status                VARCHAR NOT NULL CHECK (status IN ('active', 'paused', 'archived')),
  source                VARCHAR(100),         -- api, wedding_app, etc.
  archived_at           TIMESTAMP,
  archived_reason       VARCHAR(50),
  tile_id               TEXT GENERATED ALWAYS AS (
    CONCAT(
      FLOOR((90.0 - location_lat) / 22.5)::INT,
      '.',
      FLOOR(
        CASE 
          WHEN location_lon >= 0 THEN location_lon 
          ELSE 360.0 + location_lon 
        END / 45.0
      )::INT
    )
  ) STORED,
  created_at            TIMESTAMP NOT NULL DEFAULT NOW(),
  updated_at            TIMESTAMP NOT NULL DEFAULT NOW(),
  
  -- Ensure either event mode (time_window) or monitor mode (monitor_config)
  CONSTRAINT valid_mode CHECK (
    (time_window_start IS NOT NULL AND time_window_end IS NOT NULL AND monitor_config IS NULL)
    OR
    (time_window_start IS NULL AND time_window_end IS NULL AND monitor_config IS NOT NULL)
  )
);

CREATE INDEX idx_wp_org_status ON watchpoints(organization_id, status);
CREATE INDEX idx_wp_status_time ON watchpoints(status, time_window_start) 
  WHERE time_window_start IS NOT NULL;
CREATE INDEX idx_wp_active_monitor ON watchpoints(status) 
  WHERE status = 'active' AND monitor_config IS NOT NULL;
CREATE INDEX idx_wp_location ON watchpoints USING GIST (
  ll_to_earth(location_lat, location_lon)
);  -- Requires earthdistance extension
CREATE INDEX idx_wp_tags ON watchpoints USING GIN(tags);
CREATE INDEX idx_wp_archived ON watchpoints(archived_at) 
  WHERE status = 'archived';
CREATE INDEX idx_watchpoints_tile ON watchpoints(tile_id) WHERE status = 'active';
```

The `tile_id` column enables efficient grouping for tile-based evaluation:

```sql
SELECT tile_id, array_agg(id) as watchpoint_ids
FROM watchpoints
WHERE status = 'active'
GROUP BY tile_id;
```

#### watchpoint_evaluation_state

```sql
CREATE TABLE watchpoint_evaluation_state (
  watchpoint_id           VARCHAR PRIMARY KEY REFERENCES watchpoints(id),
  last_evaluated_at       TIMESTAMP,
  last_evaluation_result  JSONB,
  previous_trigger_state  BOOLEAN,
  trigger_state_changed_at TIMESTAMP,
  last_notified_at        TIMESTAMP,
  last_notified_state     VARCHAR,
  notification_count_24h  INTEGER DEFAULT 0,
  last_forecast_summary   JSONB,
  escalation_level        INTEGER DEFAULT 0
);

CREATE INDEX idx_eval_state_time ON watchpoint_evaluation_state(last_evaluated_at);
```

#### notifications

```sql
CREATE TABLE notifications (
  id                  VARCHAR PRIMARY KEY,  -- "notif_xxxxx"
  watchpoint_id       VARCHAR NOT NULL REFERENCES watchpoints(id),
  organization_id     VARCHAR NOT NULL,     -- denormalized
  event_type          VARCHAR NOT NULL,
  urgency             VARCHAR NOT NULL,
  payload             JSONB NOT NULL,
  created_at          TIMESTAMP NOT NULL
);

CREATE INDEX idx_notif_wp ON notifications(watchpoint_id, created_at DESC);
CREATE INDEX idx_notif_org ON notifications(organization_id, created_at DESC);
```

#### notification_deliveries

```sql
CREATE TABLE notification_deliveries (
  id                  VARCHAR PRIMARY KEY,
  notification_id     VARCHAR NOT NULL REFERENCES notifications(id),
  channel_type        VARCHAR NOT NULL,
  channel_config      JSONB NOT NULL,
  status              VARCHAR NOT NULL,     -- pending, sent, delivered, failed, bounced
  attempt_count       INTEGER DEFAULT 0,
  last_attempt_at     TIMESTAMP,
  delivered_at        TIMESTAMP,
  failure_reason      VARCHAR,
  provider_message_id VARCHAR
);

CREATE INDEX idx_delivery_notif ON notification_deliveries(notification_id);
CREATE INDEX idx_delivery_retry ON notification_deliveries(status, last_attempt_at);
```

#### forecast_runs

```sql
CREATE TABLE forecast_runs (
  id                    VARCHAR PRIMARY KEY,
  model                 VARCHAR NOT NULL,   -- medium_range, nowcast
  run_timestamp         TIMESTAMP NOT NULL,
  source_data_timestamp TIMESTAMP NOT NULL,
  storage_path          VARCHAR NOT NULL,
  status                VARCHAR NOT NULL,   -- running, complete, failed
  inference_duration_ms INTEGER,
  created_at            TIMESTAMP NOT NULL
);

CREATE INDEX idx_forecast_model_time ON forecast_runs(model, run_timestamp DESC);
CREATE INDEX idx_forecast_model_status ON forecast_runs(model, status);
```

### Multi-Tenancy

**Approach**: Logical separation with `organization_id`

All tenant data in shared tables, isolated by `organization_id`:

1. **API Gateway**: Extracts `organization_id` from token, injects into context
2. **Data Access Layer**: All queries include `WHERE organization_id = ?`
3. **Row-Level Security** (optional): Database policies enforce isolation

Forecast data is **shared infrastructure** — not tenant-specific.

---

## API Design

### Base URL

```
https://api.watchpoint.io/v1
```

### WatchPoints API

#### Create WatchPoint

```
POST /v1/watchpoints

Request:
{
  "name": "Sarah's Wedding",
  "location": {
    "lat": 40.7128,
    "lon": -74.0060,
    "display_name": "Central Park, NYC"
  },
  "time_window": {
    "start": "2026-06-15T15:00:00-04:00",
    "end": "2026-06-15T20:00:00-04:00"
  },
  "timezone": "America/New_York",
  "conditions": [
    {
      "variable": "precipitation_probability",
      "operator": ">",
      "threshold": 50,
      "unit": "percent"
    }
  ],
  "condition_logic": "ANY",
  "channels": [
    {
      "type": "email",
      "config": { "address": "sarah@example.com" },
      "enabled": true
    }
  ],
  "preferences": {
    "notify_on_clear": true,
    "notify_on_forecast_change": true
  },
  "tags": ["wedding", "outdoor"],
  "template_set": "wedding"
}

Response: 201 Created
{
  "id": "wp_8x7k2mNqPl",
  "status": "active",
  "created_at": "2026-01-31T10:30:00Z",
  "current_forecast": {
    "precipitation_probability": 25,
    "summary": "Partly cloudy, low rain chance",
    "fetched_at": "2026-01-31T06:00:00Z"
  },
  ... (all fields echoed back)
}
```

#### Create Monitor Mode WatchPoint

```
POST /v1/watchpoints

Request:
{
  "name": "Downtown Construction Site",
  "location": {
    "lat": 40.7128,
    "lon": -74.0060,
    "display_name": "123 Main St, NYC"
  },
  "timezone": "America/New_York",
  "monitor_config": {
    "window_type": "next_hours",
    "window_hours": 48,
    "active_hours": [[6, 18]],
    "active_days": [1, 2, 3, 4, 5]
  },
  "conditions": [
    {
      "variable": "precipitation_probability",
      "operator": ">",
      "threshold": 40,
      "unit": "percent"
    },
    {
      "variable": "wind_speed",
      "operator": ">",
      "threshold": 30,
      "unit": "kmh"
    }
  ],
  "condition_logic": "ANY",
  "channels": [
    {
      "type": "email",
      "config": { "address": "foreman@construction.com" },
      "enabled": true
    },
    {
      "type": "webhook",
      "config": { 
        "url": "https://construction-app.com/webhooks/weather",
        "secret": "whsec_abc123"
      },
      "enabled": true
    }
  ],
  "preferences": {
    "monitor_mode": "digest",
    "digest_schedule": {
      "time": "05:00",
      "timezone": "America/New_York",
      "days": [1, 2, 3, 4, 5]
    },
    "notify_on_clear": false
  },
  "tags": ["construction", "concrete-pour"],
  "template_set": "construction"
}

Response: 201 Created
{
  "id": "wp_construction_456",
  "status": "active",
  "mode": "monitor",
  ...
}
```

#### List WatchPoints

```
GET /v1/watchpoints?status=active,triggered&tags=wedding&limit=50

Response: 200 OK
{
  "data": [ ... ],
  "pagination": {
    "next_cursor": "eyJpZCI6IndwXzl5OG0...",
    "has_more": true
  }
}
```

#### Get WatchPoint

```
GET /v1/watchpoints/{id}

Response: 200 OK
{
  ... (full watchpoint),
  "evaluation_state": { ... },
  "current_forecast": { ... },
  "forecast_history": [ ... ]
}
```

#### Update WatchPoint

```
PATCH /v1/watchpoints/{id}

Request: (partial update)
{
  "conditions": [ ... ]
}

Response: 200 OK
```

#### Delete WatchPoint

```
DELETE /v1/watchpoints/{id}

Response: 204 No Content
```

#### Pause / Resume

```
POST /v1/watchpoints/{id}/pause
POST /v1/watchpoints/{id}/resume

Response: 200 OK
```

#### Get Notification History

```
GET /v1/watchpoints/{id}/notifications

Response: 200 OK
{
  "data": [
    {
      "id": "notif_abc123",
      "sent_at": "2026-01-30T14:00:00Z",
      "event_type": "threshold_crossed",
      "channels": [
        { "type": "email", "status": "delivered" }
      ],
      "forecast_snapshot": { ... }
    }
  ]
}
```

### Forecasts API

#### Point Forecast

```
GET /v1/forecasts/point?lat=40.7128&lon=-74.006&start=2026-06-15T12:00:00Z&end=2026-06-15T22:00:00Z&variables=precipitation_probability,temperature

Response: 200 OK
{
  "location": { "lat": 40.7128, "lon": -74.006 },
  "generated_at": "2026-01-31T12:00:00Z",
  "model_runs": {
    "medium_range": "2026-01-31T06:00:00Z",
    "nowcast": "2026-01-31T11:45:00Z"
  },
  "forecast": [
    {
      "valid_time": "2026-06-15T12:00:00Z",
      "source": "medium_range",
      "precipitation_probability": 25,
      "temperature_c": 24
    },
    ...
  ]
}
```

#### Multi-Point Forecast (Batch)

```
POST /v1/forecasts/points

Request:
{
  "locations": [
    { "id": "venue_a", "lat": 40.7128, "lon": -74.006 },
    { "id": "venue_b", "lat": 40.7580, "lon": -73.985 }
  ],
  "start": "2026-06-15T12:00:00Z",
  "end": "2026-06-15T22:00:00Z",
  "variables": ["precipitation_probability", "temperature"]
}

Response: 200 OK
{
  "forecasts": {
    "venue_a": { ... },
    "venue_b": { ... }
  }
}
```

#### Available Variables

```
GET /v1/forecasts/variables

Response: 200 OK
{
  "variables": [
    {
      "name": "precipitation_probability",
      "description": "Probability of precipitation",
      "unit": "percent",
      "range": [0, 100],
      "available_in": ["medium_range", "nowcast"]
    },
    ...
  ]
}
```

#### Forecast Status

```
GET /v1/forecasts/status

Response: 200 OK
{
  "medium_range": {
    "latest_run": "2026-01-31T12:00:00Z",
    "coverage": "global",
    "horizon_days": 15,
    "status": "healthy"
  },
  "nowcast": {
    "latest_run": "2026-01-31T14:45:00Z",
    "coverage": "CONUS",
    "horizon_hours": 6,
    "status": "healthy"
  }
}
```

### Organization API

#### Get Organization

```
GET /v1/organization

Response: 200 OK
{
  "id": "org_abc123",
  "name": "Acme Corp",
  "billing_email": "billing@acme.com",
  "plan": "pro",
  "plan_limits": {
    "watchpoints_max": 100,
    "api_calls_daily_max": 1000
  },
  "created_at": "2025-06-15T10:00:00Z"
}
```

#### Update Organization

```
PATCH /v1/organization

Request:
{
  "name": "Acme Corporation",
  "billing_email": "finance@acme.com"
}

Response: 200 OK
```

### Users API

#### List Users

```
GET /v1/users

Response: 200 OK
{
  "data": [
    {
      "id": "user_xyz789",
      "email": "alice@acme.com",
      "role": "owner",
      "created_at": "2025-06-15T10:00:00Z",
      "last_login_at": "2026-01-31T09:00:00Z"
    }
  ]
}
```

#### Invite User

```
POST /v1/users/invite

Request:
{
  "email": "bob@acme.com",
  "role": "member"
}

Response: 201 Created
{
  "id": "user_abc456",
  "email": "bob@acme.com",
  "role": "member",
  "status": "invited",
  "invite_expires_at": "2026-02-07T12:00:00Z"
}
```

#### Update User Role

```
PATCH /v1/users/{id}

Request:
{
  "role": "admin"
}

Response: 200 OK
```

#### Remove User

```
DELETE /v1/users/{id}

Response: 204 No Content
```

### API Keys API

#### List API Keys

```
GET /v1/api-keys

Response: 200 OK
{
  "data": [
    {
      "id": "key_abc123",
      "name": "Production Key",
      "key_prefix": "sk_live_abc...",
      "scopes": ["watchpoints:read", "watchpoints:write", "forecasts:read"],
      "created_at": "2025-08-01T10:00:00Z",
      "last_used_at": "2026-01-31T11:30:00Z",
      "expires_at": null
    }
  ]
}
```

#### Create API Key

```
POST /v1/api-keys

Request:
{
  "name": "CI/CD Pipeline",
  "scopes": ["watchpoints:read", "watchpoints:write"],
  "expires_in_days": 365
}

Response: 201 Created
{
  "id": "key_xyz789",
  "name": "CI/CD Pipeline",
  "key": "sk_live_xyzABC123...",  // Only shown once!
  "key_prefix": "sk_live_xyz...",
  "scopes": ["watchpoints:read", "watchpoints:write"],
  "expires_at": "2027-01-31T12:00:00Z"
}
```

**Important**: The full `key` is only returned on creation. Store it securely.

#### Revoke API Key

```
DELETE /v1/api-keys/{id}

Response: 204 No Content
```

#### Rotate API Key

```
POST /v1/api-keys/{id}/rotate

Response: 200 OK
{
  "id": "key_xyz789",
  "key": "sk_live_newKEY456...",  // New key
  "key_prefix": "sk_live_new...",
  "previous_key_valid_until": "2026-02-01T12:00:00Z"  // 24h grace period
}
```

Rotation provides a 24-hour grace period where both old and new keys work.

### Usage API

#### Get Current Usage

```
GET /v1/usage

Response: 200 OK
{
  "period": {
    "start": "2026-01-01T00:00:00Z",
    "end": "2026-01-31T23:59:59Z"
  },
  "watchpoints": {
    "active": 47,
    "limit": 100,
    "percent_used": 47
  },
  "api_calls": {
    "today": 234,
    "daily_limit": 1000,
    "percent_used": 23.4
  },
  "notifications": {
    "sent_this_period": 1247,
    "by_channel": {
      "email": 892,
      "webhook": 355
    }
  }
}
```

#### Get Usage History

```
GET /v1/usage/history?period=daily&days=30

Response: 200 OK
{
  "data": [
    {
      "date": "2026-01-30",
      "api_calls": 456,
      "notifications_sent": 23,
      "watchpoints_active": 45
    },
    ...
  ]
}
```

### Billing API

#### Get Invoices

```
GET /v1/billing/invoices

Response: 200 OK
{
  "data": [
    {
      "id": "inv_abc123",
      "period_start": "2026-01-01",
      "period_end": "2026-01-31",
      "amount_cents": 9900,
      "currency": "usd",
      "status": "paid",
      "paid_at": "2026-01-01T10:00:00Z",
      "pdf_url": "https://..."
    }
  ]
}
```

#### Get Current Subscription

```
GET /v1/billing/subscription

Response: 200 OK
{
  "plan": "pro",
  "status": "active",
  "current_period_start": "2026-01-01T00:00:00Z",
  "current_period_end": "2026-01-31T23:59:59Z",
  "cancel_at_period_end": false,
  "payment_method": {
    "type": "card",
    "last4": "4242",
    "exp_month": 12,
    "exp_year": 2027
  }
}
```

### Error Responses

```json
{
  "error": {
    "code": "watchpoint_limit_reached",
    "message": "You have reached your plan's WatchPoint limit",
    "details": {
      "limit": 25,
      "current": 25,
      "upgrade_url": "https://watchpoint.io/upgrade"
    }
  }
}
```

| HTTP Code | Meaning |
|-----------|---------|
| 400 | Bad request (validation error) |
| 401 | Unauthorized (missing/invalid auth) |
| 403 | Forbidden (limit reached, wrong scope) |
| 404 | Not found |
| 409 | Conflict (duplicate, concurrent modification) |
| 422 | Unprocessable entity (semantically invalid) |
| 429 | Rate limit exceeded |
| 500 | Internal server error |
| 503 | Service unavailable |

### Rate Limiting

#### Rate Limit Headers

All responses include rate limit headers:

```
X-RateLimit-Limit: 1000
X-RateLimit-Remaining: 847
X-RateLimit-Reset: 1706745600
```

When rate limited (429):

```
Retry-After: 3600
X-RateLimit-Limit: 1000
X-RateLimit-Remaining: 0
X-RateLimit-Reset: 1706745600
```

#### Rate Limit Tiers

| Plan | Requests/Day | Burst (per minute) |
|------|--------------|-------------------|
| Free | 100 | 10 |
| Starter | 1,000 | 60 |
| Pro | 10,000 | 300 |
| Business | 100,000 | 1,000 |

Batch endpoints (`POST /v1/forecasts/points`) count as 1 request regardless of locations queried (up to 50).

### Idempotency

For `POST` requests that create resources, include an idempotency key:

```
Idempotency-Key: unique-request-id-123
```

Behavior:
- If same key seen within 24 hours, return cached response
- Useful for retry logic after network failures
- Keys are scoped to organization

```
POST /v1/watchpoints
Idempotency-Key: wp-create-wedding-2026

Response: 201 Created (first time)
Response: 200 OK (subsequent, returns cached)
```

### Input Validation

#### String Lengths

| Field | Max Length |
|-------|------------|
| `name` | 200 characters |
| `display_name` | 200 characters |
| `tag` | 50 characters |
| `metadata` (total) | 4KB |
| `webhook_url` | 2048 characters |

#### Location Validation

```json
{
  "lat": 40.7128,   // Must be -90 to 90
  "lon": -74.006    // Must be -180 to 180
}
```

For nowcast-dependent features, location must be within CONUS bounds:
- Latitude: 24.0° to 50.0°
- Longitude: -125.0° to -66.0°

WatchPoints outside CONUS will only receive medium-range forecasts.

#### Webhook URL Validation

- Must be HTTPS (HTTP rejected)
- Must not be localhost, 127.0.0.1, or private IP ranges
- Must not be internal cloud metadata endpoints (169.254.x.x)
- Domain must resolve to public IP

### CORS

API supports CORS for browser-based clients:

```
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, POST, PATCH, DELETE, OPTIONS
Access-Control-Allow-Headers: Authorization, Content-Type, Idempotency-Key
Access-Control-Max-Age: 86400
```

For webhook endpoints receiving our notifications, we do NOT send CORS preflight (server-to-server).

### API Versioning

Current version: `v1`

Versioning strategy:
- Breaking changes get new version (`v2`)
- Non-breaking additions to existing version
- Deprecated features announced 6 months before removal
- `Sunset` header indicates deprecation date

```
Sunset: Sat, 01 Jan 2028 00:00:00 GMT
Deprecation: true
Link: <https://docs.watchpoint.io/migration/v2>; rel="successor-version"
```

---

## Authentication & Authorization

### Authentication Methods

#### API Keys (Primary)

```
Header: Authorization: Bearer sk_live_xxxxxxxxxxxx
```

Key types:
- `sk_live_*` — Production keys
- `sk_test_*` — Test mode (no real notifications)

#### Session Tokens (Dashboard)

```
Header: Authorization: Bearer sess_xxxxxxxxxxxx
```

Cookie-based after OAuth/password login.

#### Webhook Verification

```
Header: X-Signature: sha256=xxxxxxxx
Secret: whsec_xxxxxxxxxxxx
```

### Authorization Scopes

| Scope | Description |
|-------|-------------|
| `watchpoints:read` | List and get WatchPoints |
| `watchpoints:write` | Create, update, delete WatchPoints |
| `forecasts:read` | Query forecast data |
| `notifications:read` | View notification history |
| `account:read` | View organization settings |
| `account:write` | Modify organization settings |

### Hierarchy

```
Organization (billing entity)
  └── Users (people with login)
        └── API Keys (programmatic access)

WatchPoints belong to Organizations.
```

---

## Security

### Data Protection

#### Encryption at Rest

| Data Type | Encryption |
|-----------|------------|
| Database (Postgres) | AES-256 via managed service |
| Object Storage (S3) | SSE-S3 or SSE-KMS |
| API Keys | Hashed with bcrypt (cost=12) |
| Webhook Secrets | Hashed with bcrypt |

#### Encryption in Transit

- All API traffic over TLS 1.2+
- Webhook deliveries require HTTPS
- Internal service communication over TLS

#### PII Handling

| Field | Storage | Notes |
|-------|---------|-------|
| Email addresses | Plaintext (needed for delivery) | Encrypted at rest via DB |
| Phone numbers | Plaintext (needed for delivery) | Encrypted at rest via DB |
| Passwords | bcrypt hash | Never stored plaintext |
| Location data | Plaintext | Not considered PII |

### Secret Management

Production secrets stored in:
- Cloud provider secret manager (AWS Secrets Manager, GCP Secret Manager)
- Environment variables injected at deploy time
- Never in code, config files, or logs

Secrets include:
- Database credentials
- API keys for external services (Stripe)
- RunPod API key
- Webhook signing keys

### Audit Logging

All sensitive operations logged:

```json
{
  "timestamp": "2026-01-31T12:00:00Z",
  "event": "api_key.created",
  "actor": {
    "type": "user",
    "id": "user_xyz789",
    "email": "alice@acme.com"
  },
  "organization_id": "org_abc123",
  "resource": {
    "type": "api_key",
    "id": "key_new456"
  },
  "ip_address": "203.0.113.42",
  "user_agent": "Mozilla/5.0..."
}
```

Events logged:
- Authentication (login, logout, failed attempts)
- API key operations (create, revoke, rotate)
- User management (invite, remove, role change)
- WatchPoint operations (create, delete, pause)
- Billing changes (plan change, payment method update)

Retention: 2 years (compliance requirement)

### Input Sanitization

All user input validated and sanitized:
- String length limits enforced
- JSON schema validation for complex objects
- SQL injection prevention via parameterized queries
- XSS prevention in any rendered output
- Path traversal prevention in any file operations

---

## Operations

### Deployment Strategy

#### Release Process

1. **Development** → Feature branches, PR review
2. **Staging** → Automatic deploy on merge to main
3. **Production** → Manual promotion with approval

#### Deployment Method

Blue-green deployment:
- Two identical production environments
- New version deployed to inactive ("green")
- Traffic shifted after health checks pass
- Instant rollback by shifting traffic back

For high-frequency components (nowcast pipeline):
- Canary deployment: 5% traffic → 25% → 100%
- Automated rollback on error rate increase

### Backup & Recovery

#### Database

| Type | Frequency | Retention |
|------|-----------|-----------|
| Continuous WAL | Real-time | 7 days |
| Daily snapshot | Daily at 03:00 UTC | 30 days |
| Weekly snapshot | Sunday 03:00 UTC | 90 days |

Recovery objectives:
- **RTO** (Recovery Time Objective): 1 hour
- **RPO** (Recovery Point Objective): 5 minutes

#### Object Storage (Forecasts)

- Cross-region replication for latest forecasts
- Versioning enabled (30-day retention)
- No backup needed for historical forecasts (regenerable from source data)

### Disaster Recovery

| Scenario | Response |
|----------|----------|
| Single AZ failure | Automatic failover to standby |
| Region failure | Manual failover to DR region (RTO: 4 hours) |
| Data corruption | Point-in-time recovery from WAL |
| Accidental deletion | Restore from snapshot + WAL |

DR region maintains:
- Database replica (async, <1 minute lag)
- Copy of latest forecasts
- Deployable infrastructure (IaC)

### Runbooks

Critical runbooks documented:
- Forecast pipeline failure
- Notification backlog
- Database failover
- API key compromise response
- Customer data deletion request (GDPR)
- Incident response and communication

---

## Billing & Plans

### Plan Tiers

| Plan | Price | WatchPoints | Channels | API Calls/Day | Nowcast | Support |
|------|-------|-------------|----------|---------------|---------|---------|
| **Free** | $0 | 3 | Email only | None | No | Community |
| **Starter** | $29/mo | 25 | Email, Webhook | 100 | Yes | Email |
| **Pro** | $99/mo | 100 | Email, Webhook | 1,000 | Yes | Priority |
| **Business** | $299/mo | 500 | Email, Webhook | 10,000 | Yes | Phone |
| **Enterprise** | Custom | Unlimited | Email, Webhook | Custom | Yes | Dedicated |

### Billing Model

**Primary**: Flat subscription

**Secondary**: Usage add-ons
- Additional WatchPoints: $10/month per 25
- Additional API calls: $20/month per 5,000

### Limit Enforcement

| Limit | Type | Behavior |
|-------|------|----------|
| WatchPoints | Hard | Cannot create beyond limit |
| API rate | Hard with burst | Soft at 100%, hard at 150% |
| Notifications | Soft with throttle | Throttle at 10x normal, never hard block |

### Overage Handling

- Warning at 80% of limit
- Clear messaging at 100%
- 14-day grace period on downgrade

---

## Observability & Monitoring

### Critical Metrics

#### Forecast Pipeline

| Metric | Alert Threshold |
|--------|-----------------|
| `forecast.age_seconds{model="medium_range"}` | > 8 hours |
| `forecast.age_seconds{model="nowcast"}` | > 45 minutes |
| `forecast.generation.success_rate` | < 95% over 1 hour |

#### Evaluation Engine

| Metric | Alert Threshold |
|--------|-----------------|
| `evaluation.queue_depth` | > 1000 |
| `evaluation.latency_ms{priority="p0"}` | > 60 seconds |

#### Notification Delivery

| Metric | Alert Threshold |
|--------|-----------------|
| `notification.delivery_rate{channel}` | < 98% for 15 minutes |
| `notification.queue_depth{channel}` | Growing trend |

#### API Health

| Metric | Alert Threshold |
|--------|-----------------|
| `api.error_rate` | > 5% for 5 minutes |
| `api.latency_p99` | > 2 seconds |

### Critical Alerts (Page On-Call)

- Medium-range forecast > 10 hours old
- Nowcast forecast > 1 hour old
- Notification delivery rate < 90% for 15 minutes
- Evaluation queue > 5000 for 10 minutes
- Database connection failures

### Dashboards

1. **System Health** — Ops team daily view
2. **Forecast Pipeline** — Debugging inference
3. **Customer Impact** — Support team view
4. **Business Overview** — Leadership metrics

---

## Forecast Verification

### Purpose

- Build customer trust
- Identify systematic biases
- Enable transparency
- Compare model performance over time

### Verification Pipeline

Daily batch job:

1. Collect forecasts from 24-48 hours ago
2. Fetch observed weather (NOAA ISD, MRMS)
3. Compare forecast vs. observed
4. Compute metrics (bias, RMSE, Brier score)
5. Store and aggregate

### Key Metrics

For precipitation:

| | Observed Rain | Observed Dry |
|---|---|---|
| **Forecast Rain** | Hit (good) | False Alarm (bad) |
| **Forecast Dry** | Miss (bad) | Correct Rejection |

- **Hit Rate** = Hits / (Hits + Misses)
- **False Alarm Rate** = False Alarms / (False Alarms + Correct)
- **Critical Success Index** = Hits / (Hits + Misses + FA)

Track by: lead time, region, season.

### Customer-Facing

Post-event summary:

```
Your Event: Sarah's Wedding (June 15)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
What we predicted:  30% chance of rain
What happened:      Light rain 4-5pm
Our accuracy:       Correctly predicted
```

---

## Failure Scenarios & Resilience

### Scenario 1: Forecast Generation Fails

**Impact**: No new forecasts, stale data

**Defenses**:
- Serve stale forecast with warning
- Multiple retry attempts
- Fallback to external API (NOAA GFS direct)
- Alert after 2 hours

### Scenario 2: Evaluation Engine Backlog

**Impact**: Delayed or missed alerts

**Defenses**:
- Priority queue (imminent first)
- Horizontal scaling
- If severely behind: skip old, process newest

### Scenario 3: Notification Delivery Fails

**Impact**: Customer doesn't receive alert

**Defenses**:
- Multiple channels configured (if webhook fails, email still goes)
- Retry with exponential backoff
- P0 alerts attempt all configured channels simultaneously

### Scenario 4: Database Unavailable

**Impact**: Can't read/write state

**Defenses**:
- Read replicas
- Automatic failover
- Cache hot data in Redis
- Queue writes for replay

### Scenario 5: Upstream Data Source Fails

**Impact**: Can't generate new forecasts

**Defenses**:
- Serve stale with warning
- Try alternate mirrors (AWS, NCEP, Google)
- Proactive customer communication

### Scenario 6: Bug Causes False Alerts

**Impact**: Customer loses trust

**Defenses**:
- Anomaly detection on trigger rate
- Rate limiting per WatchPoint
- Kill switch to pause all notifications
- Gradual rollout of changes

### Scenario 7: Bug Causes Missed Alerts

**Impact**: Customer's event ruined (worst case)

**Defenses**:
- Synthetic canary WatchPoints
- Monitor for "expected but missing" notifications
- Post-event verification

### Defense Principle

> No single failure should cause missed alerts.

| Component | Redundancy |
|-----------|------------|
| Forecast source | Stale data, alternate mirrors |
| GPU inference | Retry, fallback API |
| Evaluation | Priority queue, horizontal scale |
| Database | Replication, cache, queue writes |
| Notification | Multi-channel, retry |

---

## Edge Cases & Special Handling

### WatchPoint Edge Cases

| Scenario | Handling |
|----------|----------|
| Time window entirely in the past | Reject on create (400 error) unless `allow_historical: true` flag for testing |
| Time window starts in past, ends in future | Accept; evaluate from now forward |
| Duplicate WatchPoints (same location, conditions) | Allow; user may have different channels or names |
| Location outside valid bounds | Reject (400 error) |
| Location outside CONUS | Accept with warning; nowcast unavailable, medium-range only |
| Conditions update while triggered | Immediate re-evaluation; may clear or remain triggered |
| WatchPoint deleted while notification in queue | Deliver notification anyway (already committed) |
| Organization deleted | Soft delete; WatchPoints paused, data retained 30 days |

### Notification Edge Cases

| Scenario | Handling |
|----------|----------|
| Webhook URL returns redirect | Follow up to 3 redirects, final must be HTTPS |
| Webhook URL is localhost/private IP | Reject on WatchPoint create (security) |
| Webhook times out | Retry with exponential backoff (1s, 5s, 30s) |
| Webhook returns 429 (rate limit) | Respect `Retry-After` header, retry later |
| Email bounces | Mark channel as failed; notify org owner after 3 bounces |
| All channels fail | Log error; retry on next evaluation cycle; alert ops if persistent |
| Platform-specific webhook deprecated | Warn user, suggest updating URL (e.g., Teams connector migration) |

### Timing Edge Cases

| Scenario | Handling |
|----------|----------|
| DST transition during event | Time window stored as UTC; no ambiguity |
| Leap second | Ignore; timestamps have second precision |
| Event spans multiple days | Evaluate each forecast cycle; aggregate notifications |
| Monitor mode during maintenance | Queue evaluations; process when back online |

### Billing Edge Cases

| Scenario | Handling |
|----------|----------|
| Downgrade with too many WatchPoints | 14-day grace; auto-pause oldest if not resolved |
| Payment fails | 7-day grace; notifications continue; pause after |
| Plan upgrade mid-cycle | Prorate immediately; new limits active instantly |
| Refund requested | Per-policy; no automatic handling |
| Free tier abuse (automation) | Rate limiting; manual review if patterns detected |

### Data Edge Cases

| Scenario | Handling |
|----------|----------|
| Forecast has NaN/missing values | Use previous valid value; flag in response |
| Location on grid cell boundary | Bilinear interpolation from 4 nearest cells |
| Extreme values (100°C temperature) | Sanity check against physical limits; reject if impossible |
| Zarr file corrupted | Retry fetch; serve stale; alert ops |

---

## System Boundaries

### In Scope (Core Platform)

- Forecast generation (GFS fetch, GPU inference, storage)
- WatchPoint CRUD and state management
- Evaluation engine (condition matching, state tracking)
- Notification system (multi-channel, spam prevention)
- API (WatchPoints, forecasts, account)
- Billing and plan enforcement
- Observability and verification

### Out of Scope (Vertical Apps / Future)

- Vertical-specific UI (wedding, construction flows)
- Vertical integrations (Honeybook, Procore, etc.)
- Consumer mobile app
- Complex condition logic (nested AND/OR)
- Derived variables ("pour window score")
- International nowcasting
- White-label / reseller features
- ML personalization

---

## Integration Points

### Upstream (Data Sources)

| Source | URL | Format | Frequency |
|--------|-----|--------|-----------|
| GFS | s3://noaa-gfs-bdp-pds/ | GRIB2 | 6 hours |
| GOES | s3://noaa-goes16/, s3://noaa-goes18/ | NetCDF | 5 min |
| MRMS Radar | s3://noaa-mrms-pds/ | GRIB2 | 2 min |
| Observations | NOAA ISD | Various | Daily batch |

### Downstream (Customer Systems)

| Channel | Integration | Auth |
|---------|-------------|------|
| Webhook | HTTPS POST | HMAC signature (optional) |
| Email | AWS SES API | AWS SDK (IAM auth) |

Webhook auto-detects and formats for:
- Slack (Block Kit)
- Discord (Embeds)
- Microsoft Teams (Adaptive Cards)
- Google Chat (Cards)
- Generic (Standard JSON)

### Infrastructure Services

| Service | Provider Options |
|---------|------------------|
| GPU Compute | RunPod, Modal, AWS Batch |
| Object Storage | S3, GCS |
| Database | Managed Postgres |
| Queue | SQS, Redis |
| Cache | Redis (ElastiCache, Upstash) |
| Payments | Stripe |
| Monitoring | Datadog, Grafana Cloud |

---

## Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         EXTERNAL DATA SOURCES                               │
│                                                                             │
│   ┌─────────┐         ┌─────────┐         ┌─────────┐                      │
│   │  NOAA   │         │  GOES   │         │  MRMS   │                      │
│   │  GFS    │         │ Satellite│         │  Radar  │                      │
│   └────┬────┘         └────┬────┘         └────┬────┘                      │
│        │                   └────────┬──────────┘                            │
│        ▼                            ▼                                       │
│   ┌─────────────────────────────────────────────────────────────────────┐  │
│   │                   FORECAST GENERATION PIPELINE                      │  │
│   │                                                                     │  │
│   │   ┌─────────────────────┐       ┌─────────────────────┐            │  │
│   │   │  MEDIUM-RANGE JOB   │       │    NOWCAST JOB      │            │  │
│   │   │  (Atlas, 4x/day)    │       │ (StormScope, ~30min)│            │  │
│   │   └──────────┬──────────┘       └──────────┬──────────┘            │  │
│   └──────────────┼──────────────────────────────┼───────────────────────┘  │
│                  │                              │                          │
│                  ▼                              ▼                          │
│   ┌─────────────────────────────────────────────────────────────────────┐  │
│   │                    FORECAST STORAGE (S3 + Zarr)                     │  │
│   └────────────────────────────────┬────────────────────────────────────┘  │
│                                    │                                       │
│                                    │ forecast_ready                        │
│                                    ▼                                       │
│   ┌─────────────────────────────────────────────────────────────────────┐  │
│   │                       EVALUATION ENGINE                             │  │
│   │                                                                     │  │
│   │  ┌─────────────────────────────────────────────────────────────┐   │  │
│   │  │  BATCHER: Group WatchPoints by Tile → Route to Queues       │   │  │
│   │  └────────────────────────────┬────────────────────────────────┘   │  │
│   │                               │                                     │  │
│   │          ┌────────────────────┴────────────────────┐               │  │
│   │          ▼                                         ▼               │  │
│   │  ┌───────────────────┐                 ┌───────────────────┐       │  │
│   │  │  URGENT QUEUE     │                 │  STANDARD QUEUE   │       │  │
│   │  │  (Nowcast tiles)  │                 │  (Med-range tiles)│       │  │
│   │  └─────────┬─────────┘                 └─────────┬─────────┘       │  │
│   │            │                                     │                  │  │
│   │            ▼                                     ▼                  │  │
│   │  ┌─────────────────────────────────────────────────────────────┐   │  │
│   │  │  EVAL WORKERS: Load tile → Evaluate WatchPoints → Trigger   │   │  │
│   │  └─────────────────────────────────────────────────────────────┘   │  │
│   └───────────────────────────────┬─────────────────────────────────────┘  │
│                                   │                                        │
│          ┌────────────────────────┼────────────────────────┐               │
│          ▼                        │                        ▼               │
│   ┌─────────────────┐             │             ┌───────────────────────┐  │
│   │   DATABASE      │             │             │ NOTIFICATION SERVICE  │  │
│   │   (Postgres)    │◄────────────┘             │                       │  │
│   │                 │                           │  Queue → Workers →    │  │
│   │  • organizations│                           │  Email / Webhook      │  │
│   │  • watchpoints  │                           │  (+ Quiet Hours)      │  │
│   │  • notifications│                           └───────────┬───────────┘  │
│   └────────┬────────┘                                       │              │
│            │                                                ▼              │
│            │                                    ┌───────────────────────┐  │
│   ┌────────┴────────┐                           │  EXTERNAL DELIVERY    │  │
│   │    API LAYER    │                           │  AWS SES (email),     │  │
│   │                 │                           │  HTTP POST (webhooks) │  │
│   │ /v1/watchpoints │                           │  auto-formatted for   │  │
│   │ /v1/forecasts   │                           │  Slack/Discord/Teams  │  │
│   └────────┬────────┘                           └───────────────────────┘  │
│            │                                                               │
└────────────┼───────────────────────────────────────────────────────────────┘
             │
             ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                              CLIENTS                                        │
│                                                                             │
│   ┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐               │
│   │ Vertical │   │   API    │   │ Webhook  │   │Dashboard │               │
│   │   Apps   │   │  Users   │   │Receivers │   │  (Web)   │               │
│   └──────────┘   └──────────┘   └──────────┘   └──────────┘               │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Data Flow Summary

1. **Forecast Flow** (scheduled):
   GFS/GOES/Radar → Fetch → GPU Inference → Zarr Storage → forecast_ready event

2. **Evaluation Flow** (event-driven):
   forecast_ready → Batcher → Tile Queues (Urgent/Standard) → Eval Workers → Queue Notifs

3. **Notification Flow** (async):
   Queue → Channel Workers → Quiet Hours Check → External Providers → Delivery Tracking

4. **API Flow** (request/response):
   Client → API Gateway → Handlers → Database/Forecast Store → Response

---

## Summary

The WatchPoint platform is a **weather intelligence system** that:

1. **Monitors** locations and time windows defined by users
2. **Evaluates** weather conditions against AI-powered forecasts
3. **Notifies** users when thresholds are crossed

### Key Architectural Decisions

| Decision | Rationale |
|----------|-----------|
| WatchPoint as atomic unit | Single abstraction covers all use cases |
| Two forecast models | Different strengths for different time horizons |
| Variable translation layer | Unified schema regardless of source model |
| State-driven notifications | Prevents spam, ensures meaningful alerts |
| Shared forecast infrastructure | Run inference once, serve all customers |
| Logical multi-tenancy | Simple operations, efficient resources |
| Flat subscription billing | Predictable for customers, aligned with our costs |
| Priority-based evaluation | Imminent events always processed first |
| Event-driven architecture | Loose coupling, horizontal scalability |
| Tile-based evaluation | Memory-efficient batching, reduced costs |
| Separate evaluation queues | Urgent nowcast alerts never blocked by batch jobs |
| Quiet hours support | Respects user boundaries, prevents alert fatigue |

### What This Enables

- **Vertical apps** can layer domain-specific UX on top
- **API users** can build custom integrations
- **Scalability** — marginal cost per customer is near zero
- **Reliability** — multiple layers of redundancy and graceful degradation

### Document Completeness Checklist

| Area | Status |
|------|--------|
| Core abstraction (WatchPoint) | ✓ Complete with Event + Monitor modes |
| Forecast system (Atlas + StormScope) | ✓ Including variable translation |
| Evaluation engine | ✓ With tile-based batching and separate queues |
| Notification system | ✓ With webhook signatures and quiet hours |
| Data model | ✓ Full schema with tile_id column |
| API design | ✓ All CRUD + admin endpoints |
| Authentication | ✓ Keys, sessions, scopes |
| Security | ✓ Encryption, audit logging |
| Operations | ✓ Deployment, backup, DR |
| Billing | ✓ Plans, limits, enforcement |
| Observability | ✓ Metrics, alerts, dashboards |
| Edge cases | ✓ Comprehensive handling |
| Integration points | ✓ Upstream and downstream |

---

*Document Version: 3.1 (Final)*
*Generated: 2026-02-01*
*Merged from: watchpoint-platform-design.md v2.0 + watchpoint-platform-design-addendum-v2.md*
*Methodology: Iterative Q&A Design Process*
