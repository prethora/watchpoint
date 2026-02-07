# WatchPoint Platform — Design Addendum v3

> Specifications, clarifications, and decisions made during flows analysis that extend the core design documents.

**Companion to**: 
- `watchpoint-platform-design-final.md`
- `watchpoint-tech-stack-v3_3.md`

**Generated from**: Flows Analysis Process (2026-02-01)

---

## Table of Contents

1. [Purpose](#purpose)
2. [Behavior Specifications](#behavior-specifications)
3. [API Additions](#api-additions)
4. [Error Handling Standards](#error-handling-standards)
5. [Data Model Additions](#data-model-additions)
6. [Event Catalog](#event-catalog)
7. [Scope Boundaries](#scope-boundaries)
8. [Reference Mappings](#reference-mappings)

---

## Purpose

During the comprehensive flows analysis of the WatchPoint platform, several specifications emerged that were either:

- **Implicit** in the original design but not explicitly documented
- **Discovered** as necessary to complete flow definitions
- **Decided** to resolve ambiguities in the original specification

This addendum captures these items to ensure they are not lost and can inform the architecture document. These specifications are considered **approved design decisions** equivalent to the original design documents.

---

## Behavior Specifications

### 2.1 Test Mode Behavior

The design documents mention `sk_test_*` API keys but do not fully specify test mode behavior.

#### Test vs Live Mode Comparison

| Aspect | Live Mode (`sk_live_*`) | Test Mode (`sk_test_*`) |
|--------|------------------------|------------------------|
| WatchPoints created | Real, evaluated | Real, evaluated (with `test_mode: true` flag) |
| Notifications sent | Yes, to real channels | No — logged but not delivered |
| Forecast data used | Real forecasts | Real forecasts (same data) |
| Plan limits consumed | Yes | Yes (enables limit testing) |
| Billing charges | Yes | No |
| Visible in dashboard | Yes | Filtered to separate "Test Data" view |
| Stripe integration | Live Stripe | Stripe test mode |

#### Test Mode Notification Handling

When a notification is queued for a WatchPoint created via a test API key:

1. Notification record created in database with `test_mode: true` flag
2. Notification worker checks flag before delivery:
   - If `test_mode: true`: Log delivery details, mark as `delivered_test`, skip actual HTTP/email
   - If `test_mode: false`: Proceed with real delivery
3. Test notifications appear in notification history with clear test indicator
4. Enables full integration testing without spamming real endpoints

#### Test Mode Data Isolation

- Same database tables, filtered by `test_mode` boolean flag
- Test WatchPoints go through full evaluation pipeline with real forecast data
- Dashboard provides toggle to show/hide test data
- API responses include `test_mode` field when true

---

### 2.2 Session-Based Authentication

The design documents focus on API key authentication. Session-based authentication for the dashboard requires additional specification.

#### Session Token Format

```
sess_{random_32_chars}
```

Example: `sess_a8f3k2m9p1x7q4w6e0r5t8y2u3i9o6`

#### Session Lifecycle

| Event | Behavior |
|-------|----------|
| Login (password) | Create session, set HTTP-only cookie, return user profile |
| Login (OAuth) | Create/link account, create session, set cookie, redirect to dashboard |
| API Request | Validate session from cookie, extend if near expiry |
| Logout | Invalidate session, clear cookie |
| Expiry | Session invalid after 7 days of inactivity |

#### Session Storage

```sql
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,  -- "sess_xxxxx"
    user_id TEXT NOT NULL REFERENCES users(id),
    organization_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    last_activity_at TIMESTAMPTZ NOT NULL,
    ip_address TEXT,
    user_agent TEXT
);

CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expiry ON sessions(expires_at);
```

#### CSRF Protection

All session-authenticated state-changing requests (POST, PATCH, DELETE) require CSRF protection:

1. On login, generate CSRF token and store in session
2. Return CSRF token to client (in response body or header)
3. Client sends token via `X-CSRF-Token` header on subsequent requests
4. Server validates token matches session before processing

#### Session Refresh (Sliding Expiration)

```python
def refresh_session_if_needed(session):
    # If session expires within 1 day, extend it
    if session.expires_at < now() + timedelta(days=1):
        session.expires_at = now() + timedelta(days=7)
        session.last_activity_at = now()
        db.save(session)
```

---

### 2.3 Concurrency Resolution Strategies

The design documents mention `config_version` for evaluation race conditions but do not specify general concurrency handling.

#### Scenario 1: Simultaneous PATCH to Same WatchPoint

**Resolution: Last-Write-Wins with Optional Optimistic Locking**

Default behavior (no locking):
- Second write overwrites first
- Both writes succeed
- `config_version` increments for each if conditions changed
- Audit log captures both updates with timestamps

Optional optimistic locking:
- Client sends `If-Match: "{etag}"` header (etag = config_version or hash)
- Server compares etag to current state
- If mismatch: return `409 Conflict` with current state
- Client can retry with fresh data

#### Scenario 2: Delete During Active Evaluation

**Resolution: Graceful Skip**

- Eval Worker fetches WatchPoint by ID
- If not found (deleted): skip evaluation, log warning, continue batch
- Any already-queued notification still delivered (committed to queue)

#### Scenario 3: Pause During Active Evaluation

**Resolution: Complete Current, Respect Next**

- Eval Worker does not check status mid-evaluation
- Current evaluation completes normally
- If notification generated, delivery worker checks status before sending
- If paused at delivery time: suppress notification, log suppression

#### Scenario 4: Organization Limit Race (Two Simultaneous Creates)

**Resolution: Optimistic with Clear Error**

- Both requests attempt insert
- Limit check occurs at insert time
- One succeeds, one fails with `403 limit_watchpoint_exceeded`
- Slight over-limit (101/100) acceptable, corrected on next check
- Error includes current count and limit for clarity

#### Scenario 5: Config Update During Notification Delivery

**Resolution: Deliver Original**

- Notification payload captures conditions at evaluation time
- Config changes after queuing do not affect pending notifications
- Next evaluation cycle uses new conditions

#### Scenario 6: New Forecast During Previous Evaluation Batch

**Resolution: Queue Both, Process Sequentially**

- Batcher enqueues new batch to SQS
- Workers process messages in order received
- WatchPoint may be evaluated twice (correct behavior)
- Notification deduplication via state tracking prevents duplicate alerts

---

### 2.4 Brute Force Protection

#### Login Brute Force Protection

| Threshold | Action |
|-----------|--------|
| 5 failures in 15 minutes (per email) | Temporary lockout: 15 minutes |
| 10 failures in 1 hour (per email) | Extended lockout: 1 hour, notify account owner |
| 20 failures in 24 hours (per email) | Account locked, manual unlock required |

Tracking:
```sql
CREATE TABLE login_attempts (
    id SERIAL PRIMARY KEY,
    email TEXT NOT NULL,
    ip_address TEXT NOT NULL,
    attempted_at TIMESTAMPTZ NOT NULL,
    success BOOLEAN NOT NULL
);

CREATE INDEX idx_login_attempts_email_time 
ON login_attempts(email, attempted_at DESC);
```

Lockout check:
```python
def check_login_allowed(email):
    recent_failures = db.query("""
        SELECT COUNT(*) FROM login_attempts
        WHERE email = %s 
        AND success = false
        AND attempted_at > NOW() - INTERVAL '15 minutes'
    """, email)
    
    if recent_failures >= 5:
        return False, "Too many failed attempts. Try again in 15 minutes."
    return True, None
```

#### API Key Brute Force Protection

| Threshold | Action |
|-----------|--------|
| 10 invalid keys in 1 minute (per IP) | Rate limit IP to 1 req/sec for 10 minutes |
| 100 invalid keys in 1 hour (per IP) | Block IP for 1 hour |

This prevents API key enumeration attacks.

---

### 2.5 Webhook Secret Rotation

The design mentions webhook secrets can be rotated with a grace period. Full specification:

#### Rotation Flow

1. Client calls rotation endpoint (e.g., `POST /v1/watchpoints/{id}/rotate-webhook-secret`)
2. Server generates new secret
3. Both old and new secrets marked valid
4. Response includes new secret and grace period end time

#### Dual-Validity Period

- Duration: 24 hours
- Both old and new secrets can verify signatures
- Signature header includes both versions during grace period:

```
X-Watchpoint-Signature: t=1706745600,v1=abc123...,v1_old=def456...
```

#### Verification (Receiver Side, During Rotation)

```python
def verify_webhook(payload, signature_header, secrets):
    parts = dict(p.split('=', 1) for p in signature_header.split(','))
    timestamp = int(parts['t'])
    
    # Reject stale timestamps
    if abs(time.time() - timestamp) > 300:
        return False
    
    signed_payload = f"{timestamp}.{payload}"
    
    # Try current secret first
    expected_v1 = hmac.new(secrets['current'], signed_payload, sha256).hexdigest()
    if hmac.compare_digest(parts.get('v1', ''), expected_v1):
        return True
    
    # Try old secret if present
    if 'v1_old' in parts and secrets.get('previous'):
        expected_old = hmac.new(secrets['previous'], signed_payload, sha256).hexdigest()
        if hmac.compare_digest(parts['v1_old'], expected_old):
            return True
    
    return False
```

#### Grace Period Expiration

- After 24 hours: old secret invalidated
- Subsequent webhooks only include `v1` signature
- Scheduled job cleans up expired old secrets

---

### 2.6 Digest Comparison Storage

The design mentions digests include "comparison to previous" but doesn't specify storage.

#### Storage Approach

Add to `watchpoint_evaluation_state` table:

```sql
ALTER TABLE watchpoint_evaluation_state
ADD COLUMN last_digest_content JSONB,
ADD COLUMN last_digest_sent_at TIMESTAMPTZ;
```

#### Digest Content Structure

```json
{
  "forecast_summary": {
    "precipitation_probability_max": 45,
    "precipitation_mm_max": 3.2,
    "temperature_c_high": 24,
    "temperature_c_low": 18
  },
  "triggered_periods": [...],
  "safe_periods": [...]
}
```

#### Comparison Calculation

```python
def calculate_digest_comparison(previous, current):
    if not previous:
        return None
    
    comparison = {}
    
    # Precipitation probability change
    prev_precip = previous['forecast_summary']['precipitation_probability_max']
    curr_precip = current['forecast_summary']['precipitation_probability_max']
    delta = curr_precip - prev_precip
    
    if abs(delta) >= 10:  # Only report significant changes
        comparison['precipitation_probability_change'] = delta
        comparison['direction'] = 'worsening' if delta > 0 else 'improving'
    
    # Similar for other variables...
    
    return comparison if comparison else None
```

---

## API Additions

### 3.1 Bulk Operations Endpoints

These endpoints enable efficient batch operations on WatchPoints.

#### Bulk Pause

```
POST /v1/watchpoints/bulk/pause

Request:
{
  "filter": {
    "tags": ["construction"],
    "status": "active"
  }
}
// OR
{
  "ids": ["wp_abc123", "wp_def456", ...]
}

Response: 200 OK
{
  "paused_count": 47,
  "paused_ids": ["wp_abc123", ...],
  "errors": []
}
```

#### Bulk Resume

```
POST /v1/watchpoints/bulk/resume

Request:
{
  "filter": {
    "tags": ["construction"],
    "status": "paused"
  },
  "options": {
    "consolidate_notifications": true  // Prevent notification flood
  }
}

Response: 200 OK
{
  "resumed_count": 47,
  "resumed_ids": ["wp_abc123", ...],
  "immediate_evaluation_triggered": true
}
```

#### Bulk Delete

```
POST /v1/watchpoints/bulk/delete

Request:
{
  "filter": {
    "status": "archived",
    "archived_before": "2025-01-01T00:00:00Z"
  },
  "confirm": true  // Required safety flag
}

Response: 200 OK
{
  "deleted_count": 156,
  "deleted_ids": ["wp_abc123", ...]
}
```

#### Bulk Tag Update

```
PATCH /v1/watchpoints/bulk/tags

Request:
{
  "filter": {
    "tags": ["wedding"]
  },
  "add_tags": ["priority", "2026"],
  "remove_tags": ["draft"]
}

Response: 200 OK
{
  "updated_count": 23,
  "updated_ids": ["wp_abc123", ...]
}
```

#### Bulk Clone

```
POST /v1/watchpoints/bulk/clone

Request:
{
  "source_ids": ["wp_archived_1", "wp_archived_2"],
  "time_shift": {
    "years": 1  // Shift all time windows forward 1 year
  },
  "override": {
    "status": "active"
  }
}

Response: 201 Created
{
  "created_count": 2,
  "mapping": {
    "wp_archived_1": "wp_new_abc",
    "wp_archived_2": "wp_new_def"
  }
}
```

#### Bulk Operations Limits

| Constraint | Limit |
|------------|-------|
| Max IDs per request | 500 |
| Max matching filter | 1000 (paginate for more) |
| Rate limit | 10 bulk operations per minute |

---

### 3.2 Dashboard-Specific Endpoints

#### Create Stripe Checkout Session

```
POST /v1/billing/checkout-session

Request:
{
  "plan": "pro",
  "success_url": "https://dashboard.watchpoint.io/billing?success=true",
  "cancel_url": "https://dashboard.watchpoint.io/billing?canceled=true"
}

Response: 200 OK
{
  "checkout_url": "https://checkout.stripe.com/c/pay/cs_xxx...",
  "session_id": "cs_xxx",
  "expires_at": "2026-02-01T13:00:00Z"
}
```

#### Create Stripe Customer Portal Session

```
POST /v1/billing/portal-session

Request:
{
  "return_url": "https://dashboard.watchpoint.io/billing"
}

Response: 200 OK
{
  "portal_url": "https://billing.stripe.com/p/session/xxx...",
  "expires_at": "2026-02-01T13:00:00Z"
}
```

---

### 3.3 Health Check Endpoint

```
GET /v1/health

Response: 200 OK
{
  "status": "healthy",
  "timestamp": "2026-02-01T12:00:00Z",
  "components": {
    "database": {
      "status": "healthy",
      "latency_ms": 2
    },
    "forecast_storage": {
      "status": "healthy",
      "latency_ms": 45
    },
    "eval_queue": {
      "status": "healthy",
      "depth": 12
    },
    "notification_queue": {
      "status": "healthy",
      "depth": 3
    }
  },
  "forecasts": {
    "medium_range": {
      "latest_run": "2026-02-01T06:00:00Z",
      "age_hours": 6,
      "status": "fresh"
    },
    "nowcast": {
      "latest_run": "2026-02-01T11:45:00Z",
      "age_minutes": 15,
      "status": "fresh"
    }
  }
}
```

Health status codes:
- `healthy`: All systems operational
- `degraded`: Some non-critical issues (stale forecasts, queue backlog)
- `unhealthy`: Critical issues (database down, no recent forecasts)

Authentication: None required (for load balancer probes), but rate-limited.

---

### 3.4 WatchPoint Clone Endpoint

```
POST /v1/watchpoints/{id}/clone

Request:
{
  "time_window": {
    "start": "2027-06-15T15:00:00-04:00",
    "end": "2027-06-15T20:00:00-04:00"
  },
  "name": "Sarah's Wedding - 2027"  // Optional override
}

Response: 201 Created
{
  "id": "wp_new_xyz",
  "source_id": "wp_archived_abc",
  ... // Full WatchPoint object
}
```

- Source WatchPoint can be any status (active, paused, archived)
- All fields copied except: id, status (set to active), time_window (from request), timestamps
- Evaluation state NOT copied (starts fresh)

---

### 3.5 Webhook Secret Rotation Endpoint

```
POST /v1/watchpoints/{id}/channels/{channel_index}/rotate-secret

Response: 200 OK
{
  "new_secret": "whsec_newXYZ123...",
  "previous_secret_valid_until": "2026-02-02T12:00:00Z",
  "rotated_at": "2026-02-01T12:00:00Z"
}
```

Note: `channel_index` is the zero-based index of the webhook channel in the channels array.

---

## Error Handling Standards

### 4.1 Standard Error Response Format

All API errors return this structure:

```json
{
  "error": {
    "code": "machine_readable_code",
    "message": "Human-readable message for display",
    "details": {
      "field": "specific_field",
      "reason": "additional context"
    },
    "request_id": "req_abc123xyz"
  }
}
```

- `code`: Machine-readable, stable across versions, use for programmatic handling
- `message`: Human-readable, may change, use for display
- `details`: Optional, provides context for debugging
- `request_id`: Always present, use for support requests

---

### 4.2 Error Code Categories

| Prefix | Category | HTTP Status | Example |
|--------|----------|-------------|---------|
| `validation_` | Input validation failed | 400 | `validation_invalid_latitude` |
| `auth_` | Authentication issue | 401 | `auth_token_expired` |
| `permission_` | Authorization issue | 403 | `permission_scope_insufficient` |
| `not_found_` | Resource doesn't exist | 404 | `not_found_watchpoint` |
| `conflict_` | State conflict | 409 | `conflict_already_paused` |
| `limit_` | Rate or plan limit | 403/429 | `limit_watchpoints_exceeded` |
| `internal_` | Server error | 500 | `internal_database_error` |
| `upstream_` | Dependency failure | 502/503 | `upstream_stripe_unavailable` |

#### Complete Error Code List

**Validation Errors (400):**
- `validation_invalid_latitude` — Latitude not in -90 to 90 range
- `validation_invalid_longitude` — Longitude not in -180 to 180 range
- `validation_invalid_timezone` — Not a valid IANA timezone
- `validation_invalid_conditions` — Conditions array malformed
- `validation_threshold_out_of_range` — Threshold outside variable's valid range
- `validation_time_window_invalid` — Start after end, or entirely in past
- `validation_missing_required_field` — Required field not provided
- `validation_invalid_email` — Email format invalid
- `validation_invalid_webhook_url` — URL not HTTPS or invalid format
- `validation_too_many_conditions` — Exceeds 10 conditions per WatchPoint

**Authentication Errors (401):**
- `auth_token_missing` — No Authorization header
- `auth_token_invalid` — Token not recognized
- `auth_token_expired` — Token past expiration
- `auth_token_revoked` — Token explicitly revoked
- `auth_session_expired` — Session timed out

**Permission Errors (403):**
- `permission_scope_insufficient` — API key lacks required scope
- `permission_organization_mismatch` — Resource belongs to different org
- `permission_role_insufficient` — User role cannot perform action
- `limit_watchpoints_exceeded` — At plan's WatchPoint limit
- `limit_api_rate_exceeded` — Daily API call limit reached

**Not Found Errors (404):**
- `not_found_watchpoint` — WatchPoint ID doesn't exist
- `not_found_organization` — Organization ID doesn't exist
- `not_found_user` — User ID doesn't exist
- `not_found_api_key` — API key ID doesn't exist
- `not_found_notification` — Notification ID doesn't exist

**Conflict Errors (409):**
- `conflict_already_paused` — WatchPoint already paused
- `conflict_already_active` — WatchPoint already active
- `conflict_email_exists` — Email already registered
- `conflict_concurrent_modification` — Resource changed (optimistic lock)
- `conflict_idempotency_mismatch` — Same idempotency key, different request

**Rate Limit Errors (429):**
- `rate_limit_exceeded` — Too many requests, check Retry-After header

**Internal Errors (500):**
- `internal_database_error` — Database operation failed
- `internal_unexpected_error` — Unhandled exception

**Upstream Errors (502/503):**
- `upstream_stripe_unavailable` — Cannot reach Stripe
- `upstream_email_provider_unavailable` — Cannot reach AWS SES
- `upstream_forecast_unavailable` — Cannot read forecast data

---

### 4.3 Retry Policies by Operation Type

| Operation Type | Retry Strategy | Max Attempts | Backoff |
|----------------|---------------|--------------|---------|
| **SQS Message Processing** | Automatic via SQS visibility timeout | 3 | Visibility timeout (30s default) |
| **Webhook Delivery** | Explicit in worker code | 3 | 1s, 5s, 30s (exponential) |
| **Email Delivery** | Delegated to provider | Per provider | Per provider |
| **External API (Stripe)** | Explicit in code | 3 | 1s, 2s, 4s (exponential) |
| **External API (AWS SES)** | Explicit in code | 3 | 1s, 2s, 4s (exponential) |
| **Database Operations** | Connection retry only | 2 | 100ms fixed |
| **S3 Operations** | AWS SDK automatic | 3 | SDK default (exponential) |
| **RunPod Inference** | Explicit in poller | 3 | 30s, 60s, 120s |

#### Retry Headers for Clients

When returning `429` or `503`, include:
```
Retry-After: 3600  // Seconds until retry is appropriate
X-RateLimit-Reset: 1706745600  // Unix timestamp when limit resets
```

---

### 4.4 Idempotency Requirements

| Operation | Idempotent? | Mechanism |
|-----------|-------------|-----------|
| **GET requests** | Yes | Naturally idempotent |
| **WatchPoint Create** | Via header | `Idempotency-Key` header, 24h window |
| **WatchPoint Update (PATCH)** | Yes | PATCH is naturally idempotent |
| **WatchPoint Delete** | Yes | Deleting deleted resource returns 204 |
| **WatchPoint Pause** | Yes | Pausing paused resource is no-op |
| **WatchPoint Resume** | Yes | Resuming active resource is no-op |
| **Batcher Processing** | Yes | Same forecast run produces same messages |
| **Eval Worker Processing** | Yes | Re-evaluation is safe, state-based |
| **Notification Delivery** | Partially | Delivery ID prevents duplicate sends |
| **Webhook Secret Rotation** | No | Generates new secret each time |
| **API Key Creation** | Via header | `Idempotency-Key` header |

#### Idempotency Key Handling

```
POST /v1/watchpoints
Idempotency-Key: unique-client-generated-id

First request:  201 Created (processes normally)
Second request: 200 OK (returns cached response)
```

- Keys scoped to organization
- Keys expire after 24 hours
- Different request body with same key returns `409 conflict_idempotency_mismatch`

---

## Data Model Additions

### 5.1 Test Mode Support

Add to `watchpoints` table:

```sql
ALTER TABLE watchpoints
ADD COLUMN test_mode BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX idx_watchpoints_test_mode 
ON watchpoints(organization_id, test_mode, status);
```

Add to `notifications` table:

```sql
ALTER TABLE notifications
ADD COLUMN test_mode BOOLEAN NOT NULL DEFAULT false;
```

Add to `api_keys` table:

```sql
-- Already implied by key prefix (sk_test_ vs sk_live_)
-- But explicit column aids queries
ALTER TABLE api_keys
ADD COLUMN test_mode BOOLEAN NOT NULL DEFAULT false;
```

---

### 5.2 Session Storage

New table:

```sql
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,  -- "sess_xxxxx"
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    last_activity_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ip_address TEXT,
    user_agent TEXT,
    csrf_token TEXT NOT NULL
);

CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expiry ON sessions(expires_at);
```

---

### 5.3 Login Attempts Tracking

New table:

```sql
CREATE TABLE login_attempts (
    id BIGSERIAL PRIMARY KEY,
    email TEXT NOT NULL,
    ip_address TEXT NOT NULL,
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    success BOOLEAN NOT NULL,
    failure_reason TEXT  -- 'invalid_password', 'account_locked', etc.
);

CREATE INDEX idx_login_attempts_email_time 
ON login_attempts(email, attempted_at DESC);

CREATE INDEX idx_login_attempts_ip_time 
ON login_attempts(ip_address, attempted_at DESC);
```

Cleanup job removes records older than 30 days.

---

### 5.4 Digest History Storage

Add to `watchpoint_evaluation_state`:

```sql
ALTER TABLE watchpoint_evaluation_state
ADD COLUMN last_digest_content JSONB,
ADD COLUMN last_digest_sent_at TIMESTAMPTZ;
```

---

### 5.5 Webhook Secret Rotation Support

Add to channels JSONB structure:

```json
{
  "type": "webhook",
  "config": {
    "url": "https://...",
    "secret": "whsec_current...",
    "previous_secret": "whsec_old...",
    "previous_secret_expires_at": "2026-02-02T12:00:00Z"
  }
}
```

Cleanup: Scheduled job removes expired `previous_secret` fields.

---

## Event Catalog

### 6.1 Internal Events

Events used for system-to-system communication:

#### Forecast Events

| Event Type | Trigger | Payload |
|------------|---------|---------|
| `forecast.medium_range.ready` | _SUCCESS marker written to S3 | `{ run_id, model, storage_path, horizon_hours, variables[] }` |
| `forecast.nowcast.ready` | _SUCCESS marker written to S3 | `{ run_id, model, storage_path, horizon_hours, coverage }` |

#### Evaluation Events

| Event Type | Trigger | Payload |
|------------|---------|---------|
| `evaluation.batch.started` | Batcher begins processing | `{ batch_id, trigger_run_id, watchpoint_count, tile_count }` |
| `evaluation.batch.completed` | Batcher finishes | `{ batch_id, duration_ms, messages_queued }` |
| `evaluation.tile.queued` | Batcher enqueues tile message | `{ tile_id, watchpoint_count, forecast_type, queue }` |
| `evaluation.tile.completed` | Eval worker finishes tile | `{ tile_id, evaluated_count, notifications_queued, duration_ms }` |

#### Notification Events

| Event Type | Trigger | Payload |
|------------|---------|---------|
| `notification.queued` | Eval worker creates notification | `{ notification_id, watchpoint_id, event_type, channels[] }` |
| `notification.delivery.attempted` | Worker attempts delivery | `{ delivery_id, channel, attempt_number }` |
| `notification.delivered` | Successful delivery | `{ delivery_id, channel, provider_message_id }` |
| `notification.failed` | All retries exhausted | `{ delivery_id, channel, error_code, error_message }` |
| `notification.suppressed` | Quiet hours or dedup | `{ notification_id, reason, resume_at }` |

#### WatchPoint Events

| Event Type | Trigger | Payload |
|------------|---------|---------|
| `watchpoint.created` | API creates WatchPoint | `{ watchpoint_id, organization_id, mode, source }` |
| `watchpoint.updated` | API updates WatchPoint | `{ watchpoint_id, changed_fields[], config_version }` |
| `watchpoint.paused` | API pauses WatchPoint | `{ watchpoint_id }` |
| `watchpoint.resumed` | API resumes WatchPoint | `{ watchpoint_id, immediate_eval }` |
| `watchpoint.archived` | Archiver job | `{ watchpoint_id, reason, event_end_time }` |
| `watchpoint.deleted` | API deletes WatchPoint | `{ watchpoint_id }` |

#### Organization Events

| Event Type | Trigger | Payload |
|------------|---------|---------|
| `organization.created` | Signup | `{ organization_id, plan }` |
| `organization.updated` | Settings change | `{ organization_id, changed_fields[] }` |
| `organization.deleted` | Deletion initiated | `{ organization_id, hard_delete_at }` |

#### User Events

| Event Type | Trigger | Payload |
|------------|---------|---------|
| `user.invited` | Invite sent | `{ user_id, organization_id, role, invited_by }` |
| `user.joined` | Invite accepted | `{ user_id, organization_id }` |
| `user.removed` | User removed | `{ user_id, organization_id, removed_by }` |
| `user.role_changed` | Role updated | `{ user_id, old_role, new_role, changed_by }` |

#### API Key Events

| Event Type | Trigger | Payload |
|------------|---------|---------|
| `apikey.created` | Key created | `{ key_id, organization_id, scopes[], created_by }` |
| `apikey.rotated` | Key rotated | `{ key_id, grace_period_ends }` |
| `apikey.revoked` | Key revoked | `{ key_id, revoked_by }` |

#### Billing Events

| Event Type | Trigger | Payload |
|------------|---------|---------|
| `billing.subscription.created` | Stripe webhook | `{ organization_id, plan, stripe_subscription_id }` |
| `billing.subscription.updated` | Stripe webhook | `{ organization_id, old_plan, new_plan }` |
| `billing.subscription.canceled` | Stripe webhook | `{ organization_id, cancel_at }` |
| `billing.payment.succeeded` | Stripe webhook | `{ organization_id, amount, invoice_id }` |
| `billing.payment.failed` | Stripe webhook | `{ organization_id, amount, failure_reason }` |

---

### 6.2 External Events (Customer Webhook Payloads)

Events delivered to customer webhook endpoints:

| Event Type | Meaning | Trigger Condition |
|------------|---------|-------------------|
| `threshold_crossed` | Conditions just became true | State: false → true |
| `threshold_cleared` | Conditions just became false | State: true → false (if notify_on_clear) |
| `forecast_changed` | Significant change without crossing | Change exceeds defined thresholds |
| `imminent_alert` | Approaching weather detected | Nowcast triggers within 6 hours |
| `escalated` | Conditions worsened while triggered | Escalation level increased |
| `monitor_digest` | Scheduled digest | Digest schedule fires |

---

### 6.3 Event Envelope Standard

All internal events use this envelope:

```json
{
  "event_id": "evt_a1b2c3d4e5f6",
  "event_type": "namespace.action",
  "timestamp": "2026-02-01T12:00:00.000Z",
  "source": "component_name",
  "version": "1.0",
  "payload": {
    // Event-specific data
  },
  "metadata": {
    "correlation_id": "corr_xyz",
    "trace_id": "trace_abc"
  }
}
```

- `event_id`: Unique, used for deduplication
- `event_type`: Dot-namespaced type
- `timestamp`: ISO 8601 with milliseconds, UTC
- `source`: Component that emitted the event
- `version`: Schema version for evolution
- `metadata`: Optional tracing/correlation IDs

---

## Scope Boundaries

### 7.1 Explicitly Out of Scope (v1)

The following capabilities are **NOT** part of v1 and should **NOT** be designed into the architecture:

#### Notification Channels

| Feature | Reason |
|---------|--------|
| SMS notifications | Complexity, cost, carrier relationships |
| Native mobile push | Requires mobile app (out of scope) |
| Direct Slack OAuth | Webhook sufficient, OAuth adds complexity |
| Direct Teams OAuth | Webhook sufficient, OAuth adds complexity |

#### Condition Logic

| Feature | Reason |
|---------|--------|
| Nested AND/OR groups | Complexity, limited demand |
| Derived/computed variables | Requires expression engine |
| Custom user-defined variables | Complexity, abuse potential |
| ML-based condition suggestions | Future enhancement |

#### Geographic

| Feature | Reason |
|---------|--------|
| International nowcasting | Data sources CONUS only |
| Address geocoding | Adds external dependency |
| Polygon/area monitoring | Requires spatial processing |

#### Business

| Feature | Reason |
|---------|--------|
| White-label / reseller | Complexity, limited initial demand |
| Multi-organization hierarchies | Complexity |
| Revenue sharing with verticals | Business model not finalized |
| Usage-based billing | Flat subscription simpler |

#### Data

| Feature | Reason |
|---------|--------|
| Historical archive access (>90 days) | Storage costs, limited demand |
| Raw model output download | IP concerns, bandwidth |
| Custom variable extraction | Complexity |

#### Integration

| Feature | Reason |
|---------|--------|
| Bidirectional vertical sync | Complexity |
| Calendar integrations | Out of core scope |
| CRM integrations | Out of core scope |

---

### 7.2 Future Considerations (v2+)

Architecture should **not block** these potential future features:

#### Channels (v2)

- **SMS via Twilio**: Same worker pattern as email, new provider
- **Mobile push via FCM/APNs**: New worker type, requires device registration

#### Condition Logic (v2)

- **Nested conditions**: Requires expression evaluator, tree structure
- **Template conditions**: Pre-built condition sets ("Wedding Weather Package")

#### Geographic (v2+)

- **European nowcasting**: New data sources (EUMETSAT), same architecture
- **Polygon monitoring**: PostGIS spatial queries, area-averaged forecasts

#### Integration (v2+)

- **OAuth-based Slack app**: Richer UX, channel selection, slash commands
- **Public forecast API**: Monetized access to forecasts without WatchPoints

#### Scale (v2+)

- **Multi-region active-active**: Database replication, request routing
- **Edge caching**: CloudFront/CloudFlare for forecast queries

---

### 7.3 Architecture Guidance

When making architecture decisions:

1. **DO NOT** over-engineer for out-of-scope features
2. **DO** use abstractions that don't block future features
3. **DO** document extension points where future features plug in

#### Extension Points

| Extension Point | Abstraction |
|-----------------|-------------|
| Notification channels | Channel interface with `format()` and `deliver()` methods |
| Condition evaluation | Evaluator interface, swappable for complex logic |
| Forecast sources | Source interface for different models |
| Webhook platforms | Platform detector with formatter registry |

**Example**: Notification channel is an interface. v1 implements Email and Webhook. SMS would be a new implementation — no architecture changes needed.

```go
type NotificationChannel interface {
    Type() string
    Validate(config json.RawMessage) error
    Format(notification Notification) ([]byte, error)
    Deliver(ctx context.Context, payload []byte) error
}
```

---

## Reference Mappings

### 8.1 Data Touchpoint Matrix

Which components access which data stores:

#### Database Tables

| Table | API | Batcher | Eval Worker | Notif Workers | Scheduled Jobs |
|-------|-----|---------|-------------|---------------|----------------|
| `organizations` | RW | R | R | R | RW |
| `users` | RW | — | — | — | RW |
| `api_keys` | RW | — | — | — | R |
| `sessions` | RW | — | — | — | RW |
| `watchpoints` | RW | R | R | R | RW |
| `watchpoint_evaluation_state` | R | — | RW | R | RW |
| `notifications` | R | — | W | RW | R |
| `notification_deliveries` | R | — | W | RW | R |
| `forecast_runs` | R | W | R | — | RW |
| `audit_log` | W | — | — | — | R |
| `login_attempts` | RW | — | — | — | RW |

R = Read, W = Write, RW = Both, — = No access

#### S3 Buckets

| Path Pattern | Writer | Readers |
|--------------|--------|---------|
| `forecasts/medium_range/{timestamp}/` | RunPod | Batcher (marker), Eval Worker (data) |
| `forecasts/nowcast/{timestamp}/` | RunPod | Batcher (marker), Eval Worker (data) |

#### SQS Queues

| Queue | Producers | Consumers |
|-------|-----------|-----------|
| `eval-queue-urgent` | Batcher | Eval Worker (urgent) |
| `eval-queue-standard` | Batcher | Eval Worker (standard) |
| `notification-queue` | Eval Worker | Email Worker, Webhook Worker |
| `*-dlq` | SQS (on failure) | Manual processing |

---

### 8.2 API Endpoint to Domain Mapping

| Endpoint | Method | Domain | Primary Flow |
|----------|--------|--------|--------------|
| `/v1/watchpoints` | POST | WPLC | WPLC-001/002 |
| `/v1/watchpoints` | GET | INFO | List WatchPoints |
| `/v1/watchpoints/{id}` | GET | INFO | INFO-008 |
| `/v1/watchpoints/{id}` | PATCH | WPLC | WPLC-003/004 |
| `/v1/watchpoints/{id}` | DELETE | WPLC | WPLC-008 |
| `/v1/watchpoints/{id}/pause` | POST | WPLC | WPLC-005 |
| `/v1/watchpoints/{id}/resume` | POST | WPLC | WPLC-006 |
| `/v1/watchpoints/{id}/clone` | POST | WPLC | WPLC-010 |
| `/v1/watchpoints/{id}/notifications` | GET | INFO | INFO-001 |
| `/v1/watchpoints/bulk` | POST | BULK | BULK-001 |
| `/v1/watchpoints/bulk/pause` | POST | BULK | BULK-002 |
| `/v1/watchpoints/bulk/resume` | POST | BULK | BULK-003 |
| `/v1/watchpoints/bulk/delete` | POST | BULK | BULK-004 |
| `/v1/watchpoints/bulk/tags` | PATCH | BULK | BULK-005 |
| `/v1/watchpoints/bulk/clone` | POST | BULK | BULK-006 |
| `/v1/forecasts/point` | GET | FQRY | FQRY-001 |
| `/v1/forecasts/points` | POST | FQRY | FQRY-002 |
| `/v1/forecasts/variables` | GET | FQRY | FQRY-003 |
| `/v1/forecasts/status` | GET | FQRY | FQRY-004 |
| `/v1/organization` | GET | INFO | Get Organization |
| `/v1/organization` | PATCH | USER | USER-002 |
| `/v1/organization/notification-preferences` | GET | INFO | Get Preferences |
| `/v1/organization/notification-preferences` | PATCH | USER | Update Preferences |
| `/v1/users` | GET | INFO | List Users |
| `/v1/users/invite` | POST | USER | USER-004 |
| `/v1/users/{id}` | PATCH | USER | USER-009 |
| `/v1/users/{id}` | DELETE | USER | USER-010 |
| `/v1/api-keys` | GET | INFO | List API Keys |
| `/v1/api-keys` | POST | USER | USER-011 |
| `/v1/api-keys/{id}` | DELETE | USER | USER-013 |
| `/v1/api-keys/{id}/rotate` | POST | USER | USER-012 |
| `/v1/usage` | GET | INFO | INFO-003 |
| `/v1/usage/history` | GET | INFO | INFO-004 |
| `/v1/billing/invoices` | GET | INFO | INFO-005 |
| `/v1/billing/subscription` | GET | INFO | INFO-006 |
| `/v1/billing/checkout-session` | POST | DASH | DASH-005 |
| `/v1/billing/portal-session` | POST | DASH | DASH-006 |
| `/v1/webhooks/stripe` | POST | BILL | BILL-007 |
| `/v1/health` | GET | API | API-003 |
| `/auth/login` | POST | DASH | DASH-001 |
| `/auth/logout` | POST | DASH | DASH-004 |
| `/auth/forgot-password` | POST | USER | USER-008 |
| `/auth/reset-password` | POST | USER | USER-008 |
| `/auth/oauth/{provider}/callback` | GET | DASH | DASH-002 |
| `/auth/accept-invite` | POST | USER | USER-005 |

---

## Document History

| Version | Date | Changes |
|---------|------|---------|
| v3.0 | 2026-02-01 | Initial version from flows analysis |

---

*Document Version: 3.0*
*Generated: 2026-02-01*
*Source: Flows Analysis Process*
*Companion to: watchpoint-platform-design-final.md, watchpoint-tech-stack-v3_3.md*
