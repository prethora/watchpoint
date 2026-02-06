# 08a - Notification Core

> **Purpose**: Defines the shared infrastructure, interfaces, and business logic for the Notification System. This package bridges the gap between the Evaluation Engine (which triggers alerts) and the specific Workers (which deliver them), handling state management, resilience, content generation, and policy enforcement.
> **Package**: `package core` (within `internal/notifications`)
> **Dependencies**: `01-foundation-types.md`, `02-foundation-db.md`
> **Dependents**: `08b-email-worker.md`, `08c-webhook-worker.md`

---

## Table of Contents

1. [Overview](#1-overview)
2. [Schema & Type Migrations](#2-schema--type-migrations)
3. [Transport Contracts (SQS)](#3-transport-contracts-sqs)
4. [Persisted Data Structures](#4-persisted-data-structures)
5. [Core Interfaces](#5-core-interfaces)
6. [Business Logic & Policy](#6-business-logic--policy)
7. [Resilience & Feedback](#7-resilience--feedback)
8. [Security & Signatures](#8-security--signatures)
9. [Flow Coverage](#9-flow-coverage)

---

## 1. Overview

The Notification Core acts as the kernel for all delivery workers. It enforces consistency across channels (Email, Webhook) by centralizing:
*   **State Transitions**: Atomic updates to delivery records.
*   **Policies**: Quiet Hours, Test Mode, and Rate Limiting.
*   **Content**: Digest generation and Template rendering.
*   **Security**: HMAC signing and secret rotation logic.

---

## 2. Schema & Type Migrations

This module requires extensions to the Foundation definitions to support ordering, health tracking, and new delivery states.

### 2.1 Database Migrations
**Target**: `02-foundation-db.md` (Table `notification_deliveries`)

We need to support the `skipped` state for Test Mode and Quiet Hours suppression.

```sql
-- Migration: Add 'skipped' to delivery status enum check
ALTER TABLE notification_deliveries 
DROP CONSTRAINT notification_deliveries_status_check;

ALTER TABLE notification_deliveries 
ADD CONSTRAINT notification_deliveries_status_check 
CHECK (status IN ('pending', 'sent', 'failed', 'bounced', 'retrying', 'skipped'));
```

### 2.2 Type Extensions
**Target**: `01-foundation-types.md` (Package `types`)

**Evaluation State (Ordering)**
Adds sequence tracking to enable client-side ordering of asynchronous webhooks.
```go
type EvaluationState struct {
    // ... existing fields ...
    EventSequence int64 `db:"event_sequence"` 
}
```

**Channel (Health Tracking)**
Adds fields to track channel health for circuit breaking (Flow `NOTIF-006`).
```go
type Channel struct {
    // ... existing fields ...
    FailureCount   int        `json:"failure_count"`
    DisabledReason string     `json:"disabled_reason,omitempty"`
    LastFailedAt   *time.Time `json:"last_failed_at,omitempty"`
}
```

---

## 3. Transport Contracts (SQS)

Defines the payload sent from the Python Evaluation Worker to the Go Notification Workers via SQS.

```go
// NotificationMessage represents the SQS payload.
// This struct is the transport envelope between Eval Worker and Notification Workers.
type NotificationMessage struct {
    // Core Identity
    NotificationID string `json:"notification_id"`
    WatchPointID   string `json:"watchpoint_id"`
    OrganizationID string `json:"organization_id"`

    // Routing & Logic
    EventType types.EventType    `json:"event_type"`
    Urgency   types.UrgencyLevel `json:"urgency"`
    TestMode  bool               `json:"test_mode"`

    // Ordering Metadata (Crucial for client-side sorting)
    Ordering OrderingMetadata `json:"ordering"`

    // Retry State: Carries retry count across the SQS Publish-Subscribe cycle.
    // Incremented by workers on transient failures before re-publishing.
    RetryCount int `json:"retry_count"`

    // Observability
    TraceID string `json:"trace_id"` // For X-Ray context propagation

    // Raw data snapshot for templating
    Payload map[string]interface{} `json:"payload"`
}

type OrderingMetadata struct {
    EventSequence     int64     `json:"event_sequence"`
    ForecastTimestamp time.Time `json:"forecast_timestamp"`
    EvalTimestamp     time.Time `json:"eval_timestamp"`
}
```

---

## 4. Persisted Data Structures

### 4.1 Strict Notification Payload

*The following types are consolidated in `01-foundation-types.md` (Section 10.1):*
- `types.NotificationPayload` - CRITICAL contract between Eval Worker and Notification Workers
- `types.OrderingMetadata` - Event sequence for client-side ordering
- `types.LocationSnapshot` - Location context for notifications
- `types.ConditionResult` - Evaluated condition results

This module uses these types directly from `package types`.

### 4.2 Digest Content
Defines the structure stored in `watchpoint_evaluation_state.last_digest_content`.

```go
type DigestContent struct {
    PeriodStart time.Time `json:"period_start"`
    PeriodEnd   time.Time `json:"period_end"`
    
    // Summary of the forecast window
    ForecastSummary ForecastSummary `json:"forecast_summary"`
    
    // Specific periods where conditions are met
    TriggeredPeriods []TimeRange `json:"triggered_periods"`
    
    // Comparison to the previous digest sent
    Comparison *DigestComparison `json:"comparison,omitempty"`
}

type ForecastSummary struct {
    MaxPrecipProb float64 `json:"max_precip_prob"`
    MaxPrecipMM   float64 `json:"max_precip_mm"`
    MinTempC      float64 `json:"min_temp_c"`
    MaxTempC      float64 `json:"max_temp_c"`
}

type TimeRange struct {
    Start time.Time `json:"start"`
    End   time.Time `json:"end"`
}

type DigestComparison struct {
    Direction string   `json:"direction"` // "worsening", "improving", "stable"
    Changes   []Change `json:"changes"`
}

type Change struct {
    Variable string  `json:"variable"`
    From     float64 `json:"from"`
    To       float64 `json:"to"`
}
```

---

## 5. Core Interfaces

### 5.1 Worker Contract
Implemented by `08b` (Email) and `08c` (Webhook).

```go
type NotificationChannel interface {
    // Type returns the channel type (e.g., "email", "webhook").
    Type() types.ChannelType

    // ValidateConfig checks if the channel config is valid (used in Test Mode/API).
    ValidateConfig(config map[string]any) error

    // Format transforms the generic Notification into a channel-specific payload.
    // For Webhooks, this handles platform detection (Slack vs Generic).
    Format(ctx context.Context, n *types.Notification, config map[string]any) ([]byte, error)

    // Deliver executes the transmission.
    Deliver(ctx context.Context, payload []byte, destination string) (*DeliveryResult, error)

    // ShouldRetry inspects an error to determine if it is transient.
    ShouldRetry(err error) bool
}

type DeliveryResult struct {
    ProviderMessageID string
    Status            DeliveryStatus // "sent", "failed", "bounced"
    FailureReason     string
    Retryable         bool
    Terminal          bool               // If true, disable channel immediately (e.g., HTTP 410)
    RetryAfter        *time.Duration     // For HTTP 429 support - when to retry
}
```

### 5.2 Persistence Manager
Abstracts database state transitions for deliveries.

```go
type DeliveryStatus string
const (
    DeliveryStatusPending   DeliveryStatus = "pending"
    DeliveryStatusSent      DeliveryStatus = "sent"
    DeliveryStatusFailed    DeliveryStatus = "failed"
    DeliveryStatusBounced   DeliveryStatus = "bounced"
    DeliveryStatusSkipped   DeliveryStatus = "skipped"
)

type DeliveryManager interface {
    // EnsureDeliveryExists is idempotent. Uses INSERT ... ON CONFLICT DO NOTHING.
    EnsureDeliveryExists(ctx context.Context, notifID string, chType types.ChannelType, idx int) (string, bool, error)

    // RecordAttempt logs that a worker is about to try sending.
    RecordAttempt(ctx context.Context, deliveryID string) error

    // MarkSuccess updates status to 'sent' and sets delivered_at.
    MarkSuccess(ctx context.Context, deliveryID string, providerMsgID string) error

    // MarkFailure updates status, increments attempts, calculates next_retry_at.
    MarkFailure(ctx context.Context, deliveryID string, reason string) (shouldRetry bool, err error)

    // MarkSkipped is used for TestMode or permanent policy suppression.
    MarkSkipped(ctx context.Context, deliveryID string, reason string) error

    // MarkDeferred sets status to 'deferred' and schedules resumption for Quiet Hours.
    // Used when PolicyEngine returns PolicyDefer. The notification will be re-queued
    // by the RequeueDeferredNotifications scheduled job once resumeAt time passes.
    MarkDeferred(ctx context.Context, deliveryID string, resumeAt time.Time) error

    // CheckAggregateFailure determines if all sibling deliveries for a notification have failed.
    // Logic: Returns true if count(deliveries where status != failed) == 0.
    // If so, emits 'NotificationFailed' metric and potentially alerts Ops.
    CheckAggregateFailure(ctx context.Context, notificationID string) (allFailed bool, err error)

    // ResetNotificationState performs a compensatory transaction when all delivery channels fail.
    // Sets last_notified_at = NULL and last_notified_state = NULL in watchpoint_evaluation_state.
    // This ensures the Evaluation Engine perceives the WatchPoint as "not notified" during
    // the next cycle, forcing a retry of the alert generation.
    ResetNotificationState(ctx context.Context, watchpointID string) error

    // CancelDeferred wraps Repo.CancelDeferredDeliveries. Purges any pending deliveries
    // with status='deferred' for this WatchPoint. Used when a WatchPoint is Resumed
    // to prevent flooding users with stale alerts parked during Quiet Hours.
    CancelDeferred(ctx context.Context, watchpointID string) error
}
```

### 5.3 Observability
Abstracts CloudWatch/Telemetry.

```go
type MetricResult string
const (
    MetricSuccess MetricResult = "success"
    MetricFailed  MetricResult = "failed"
    MetricSkipped MetricResult = "skipped"
)

type NotificationMetrics interface {
    RecordDelivery(ctx context.Context, channel types.ChannelType, result MetricResult)
    RecordLatency(ctx context.Context, channel types.ChannelType, duration time.Duration)
    RecordQueueLag(ctx context.Context, lag time.Duration)
}
```

---

## 6. Business Logic & Policy

### 6.1 Policy Engine (Quiet Hours)
Determines if a notification should be sent *now*.

```go
type PolicyDecision string
const (
    PolicyDeliverImmediately PolicyDecision = "deliver"
    PolicySuppress           PolicyDecision = "suppress"
    PolicyDefer              PolicyDecision = "defer"
)

type PolicyResult struct {
    Decision PolicyDecision
    Reason   string
    ResumeAt *time.Time // Set when Decision is PolicyDefer
}

type PolicyEngine interface {
    // Evaluate checks Quiet Hours, Frequency Caps, and Urgency overrides.
    //
    // IMPORTANT: Timezone Resolution
    // Quiet Hours evaluation requires the `Organization` entity to resolve the
    // authoritative Timezone for the user's local time, NOT the WatchPoint's
    // location timezone. This ensures Quiet Hours respect user preferences
    // regardless of where their monitored locations are geographically.
    //
    // Example: User in New York (org.Timezone="America/New_York") monitoring
    // a location in Los Angeles. Quiet Hours 10pm-7am should apply to NYC time.
    //
    // **Clearance Override**: If `EventType` is `threshold_cleared`, the Policy Engine
    // MUST bypass the standard `CooldownMinutes` check. Clearance notifications are
    // time-sensitive and must always be delivered if enabled. Users expect immediate
    // confirmation when conditions return to safe levels.
    Evaluate(ctx context.Context, n *types.Notification, org *types.Organization, prefs types.NotificationPreferences) (PolicyResult, error)
}
```

### 6.2 Content Generators

```go
// DigestGenerator encapsulates logic for Monitor Mode summaries.
//
// **Logic Updates**:
// 1. **Empty Check**: If the generated digest contains 0 triggered periods and 0 significant changes,
//    check `DigestConfig.SendEmpty`. If `false`, abort generation and return `ErrDigestEmpty` (do not send).
// 2. **Template Selection**: Use `DigestConfig.TemplateSet` if provided. Otherwise, default to `"digest_default"`.
// 3. **Truncation**: If `len(TriggeredPeriods) > 20`, truncate the list and set `RemainingCount` to prevent payload bloat.
type DigestGenerator interface {
    Generate(ctx context.Context,
             wp *types.WatchPoint,
             currentForecast *types.EvaluationResult,
             previousDigest *DigestContent,
             digestConfig *types.DigestConfig) (*DigestContent, error)
}

// ErrDigestEmpty is returned when SendEmpty=false and the digest has no content
var ErrDigestEmpty = errors.New("digest empty and SendEmpty=false")

// RenderedContent contains the string outputs of a template render.
type RenderedContent struct {
    Subject  string
    BodyText string
    BodyHTML string
    Markdown string
}

// TemplateEngine renders message content based on a named set (e.g., "wedding").
type TemplateEngine interface {
    // Render generates formatted content for the given template set and event type.
    //
    // Soft-Fail Fallback Logic:
    // If Render fails for a named template set (e.g., "wedding"), the engine MUST
    // automatically retry with templateSet="default". Only return an error if the
    // default template also fails. This ensures notifications are always delivered
    // even if custom templates have issues.
    //
    // Implementation:
    //   1. Attempt render with requested templateSet
    //   2. If error (template not found, parse error, etc.) AND templateSet != "default":
    //      - Log warning with original templateSet and error
    //      - Retry with templateSet="default"
    //   3. If default also fails, return error
    Render(ctx context.Context,
           templateSet string,
           eventType types.EventType,
           payload NotificationPayload) (*RenderedContent, error)
}
```

---

## 7. Resilience & Feedback

### 7.1 Retry Logic

```go
type RetryPolicy struct {
    MaxAttempts   int
    BaseDelay     time.Duration
    MaxDelay      time.Duration
    BackoffFactor float64
}

// Standard implementations
var WebhookRetryPolicy = RetryPolicy{MaxAttempts: 3, BaseDelay: 1 * time.Second, MaxDelay: 30 * time.Second, BackoffFactor: 5.0}
var EmailRetryPolicy = RetryPolicy{MaxAttempts: 3, BaseDelay: 1 * time.Second, MaxDelay: 10 * time.Second, BackoffFactor: 2.0}

func CalculateNextRetry(policy RetryPolicy, attempt int) time.Duration
```

**CRITICAL: Publish-Subscribe Retry Pattern**

Workers DO NOT block on retries. Long sleeps inside Lambda functions waste compute
resources and risk timeout failures. Instead, workers use SQS delayed message
re-publication to implement exponential backoff.

**Retry Flow:**
1. Worker attempts delivery, receives transient error (5xx, timeout, rate-limit)
2. Worker calculates backoff duration: `delay = CalculateNextRetry(policy, msg.RetryCount)`
3. Worker **publishes a NEW message** to SQS with:
   - `DelaySeconds` set to the calculated backoff (max 900 seconds for SQS)
   - `RetryCount` incremented from the original message
4. Worker **ACKs the original message** (deletes from queue)
5. Worker returns success (message processed, retry scheduled)

**Persistence Requirement**:
Before re-publishing to SQS, the worker MUST update the `notification_deliveries` record (increment attempts, set status='retrying', set next_retry_at). This ensures the "Last Attempted" timestamp in the dashboard is accurate even during backoff periods.

**Example Implementation:**
```go
func (w *Worker) handleTransientFailure(ctx context.Context, msg NotificationMessage, err error) error {
    if msg.RetryCount >= w.policy.MaxAttempts {
        // Max retries exceeded - mark as permanently failed
        return w.deliveryMgr.MarkFailure(ctx, msg.DeliveryID, err.Error())
    }

    delay := CalculateNextRetry(w.policy, msg.RetryCount)
    msg.RetryCount++

    // MUST update DB before re-publishing (Persistence Requirement)
    w.deliveryMgr.RecordAttempt(ctx, msg.DeliveryID)

    // Re-publish with delay (non-blocking retry)
    return w.sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
        QueueUrl:     &w.queueURL,
        MessageBody:  marshal(msg),
        DelaySeconds: int32(delay.Seconds()),
    })
}
```

This pattern ensures:
- No wasted Lambda compute time during backoff periods
- Automatic retry scheduling via SQS visibility timeout
- Clear retry state tracking via `RetryCount` field
- Graceful degradation under load (retries spread over time)

### 7.2 Rate Limiting (Outbound)

```go
// RateLimitHandler manages reactive backoff for downstream 429s.
type RateLimitHandler interface {
    ShouldThrottle(ctx context.Context, destination string) (bool, time.Duration)
    RecordResponse(ctx context.Context, destination string, statusCode int, retryAfter *time.Duration) error
}
```

### 7.3 Feedback Loop (Bounces)

```go
type FeedbackType string
const (
    FeedbackBounce    FeedbackType = "bounce"
    FeedbackComplaint FeedbackType = "complaint"
    FeedbackFailure   FeedbackType = "failure"
)

type FeedbackEvent struct {
    DeliveryID string
    Type       FeedbackType
    Reason     string
}

type FeedbackProcessor interface {
    // ProcessFeedback updates delivery status and increments channel failure counts.
    // Triggers channel disabling if thresholds are exceeded.
    ProcessFeedback(ctx context.Context, event FeedbackEvent) error
}

type ChannelHealthRepository interface {
    IncrementChannelFailure(ctx context.Context, wpID string, idx int) (int, error)
    DisableChannel(ctx context.Context, wpID string, idx int, reason string) error

    // ResetChannelFailureCount resets failure_count to 0 for a channel.
    // IMPORTANT: Implements "Lazy Reset Strategy" - see below.
    ResetChannelFailureCount(ctx context.Context, wpID string, channelID string) error
}

// --- Lazy Reset Strategy for Channel Health ---
//
// To reduce database write churn on successful deliveries, workers MUST implement
// a "Lazy Reset Strategy" for channel health tracking:
//
// **Rule**: Workers MUST only write to the database to reset `failure_count = 0`
// if the channel's in-memory state shows `failure_count > 0`. Successful deliveries
// to already-healthy channels (failure_count == 0) SHOULD NOT trigger a DB write.
//
// **Implementation Pattern**:
// ```go
// func (w *Worker) onDeliverySuccess(ctx context.Context, channel *types.Channel) error {
//     // Lazy Reset: Only write if channel was previously unhealthy
//     if channel.FailureCount > 0 {
//         return w.healthRepo.ResetChannelFailureCount(ctx, channel.WatchPointID, channel.ID)
//     }
//     // Channel already healthy - skip DB write
//     return nil
// }
// ```
//
// **Rationale**: In steady-state operation, most deliveries succeed to healthy channels.
// Writing a "reset" on every success would generate unnecessary database load. The lazy
// strategy only writes when transitioning from unhealthy -> healthy state.
//
// **Memory Requirement**: Workers must maintain in-memory channel health state (populated
// from the delivery record or channel config at message processing time) to make this
// determination without an additional DB read.
```

---

## 8. Security & Signatures

Shared logic for HMAC operations, enforcing the "Dual-Validity" grace period defined in the Design Addendum.

```go
type SignatureManager interface {
    // SignPayload generates "t=...,v1=...,v1_old=..." signature header.
    //
    // **Implementation Requirements**:
    // 1. Generate `t=<unix_timestamp>` using the provided `now` parameter.
    // 2. Generate `v1=<hmac_sha256>` using `secretConfig["secret"]` as the signing key.
    // 3. **Previous Secret Handling (Dual-Validity)**:
    //    a. Parse `previous_secret` from `secretConfig`. If not present, skip `v1_old`.
    //    b. Parse `previous_secret_expires_at` from `secretConfig` as RFC3339 timestamp.
    //    c. **CRITICAL**: If `now > previous_secret_expires_at`, the implementation MUST
    //       omit the `v1_old` signature from the generated header, even if `previous_secret`
    //       exists in the config. The expiration check enforces that the grace period has
    //       ended and clients should have migrated to the new secret.
    //    d. If `previous_secret` exists AND `now <= previous_secret_expires_at`, generate
    //       `v1_old=<hmac_sha256>` using `previous_secret` as the signing key.
    // 4. Return formatted header: "t=...,v1=..." or "t=...,v1=...,v1_old=..."
    //
    // **Example**:
    // ```go
    // expiresAt, _ := time.Parse(time.RFC3339, secretConfig["previous_secret_expires_at"].(string))
    // if prevSecret, ok := secretConfig["previous_secret"].(string); ok && now.Before(expiresAt) {
    //     // Include v1_old signature
    // }
    // ```
    SignPayload(payload []byte, secretConfig map[string]any, now time.Time) (string, error)

    // VerifySignature checks payload against signature.
    VerifySignature(payload []byte, header string, secrets map[string]string) bool
}
```

---

## 9. Flow Coverage

| Flow ID | Flow Name | Implementation |
|---|---|---|
| `NOTIF-001` | Immediate Delivery | `NotificationChannel.Deliver` + `DeliveryManager` |
| `NOTIF-002` | Quiet Hours | `PolicyEngine.Evaluate` |
| `NOTIF-003` | Digest Generation | `DigestGenerator.Generate` |
| `NOTIF-004` | Retry Logic | `RetryPolicy` + `DeliveryManager.MarkFailure` |
| `NOTIF-005` | Rate Limit Handling | `RateLimitHandler` |
| `NOTIF-006` | Bounce Handling | `FeedbackProcessor` + `ChannelHealthRepository` |
| `HOOK-001` | Secret Rotation | `SignatureManager.SignPayload` (Support for v1/v1_old) |
| `OBS-010` | Metrics | `NotificationMetrics` |
| `TEST-002` | Test Mode | `NotificationMessage.TestMode` check in Workers |