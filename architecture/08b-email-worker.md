# 08b - Email Worker

> **Purpose**: Defines the Email Worker responsible for delivering notifications via transactional email providers (e.g., SendGrid, SES). It handles template data preparation, provider-side rendering, rate limiting, and asynchronous feedback processing (bounces/complaints).
> **Package**: `worker/email`
> **Dependencies**: `08a-notification-core.md` (Interfaces), `10-external-integrations.md` (Provider Impl), `01-foundation-types.md`, `02-foundation-db.md` (UserRepository)

---

## Table of Contents

1. [Overview](#1-overview)
2. [Worker Structure](#2-worker-structure)
3. [Template Architecture](#3-template-architecture)
4. [Provider Interface](#4-provider-interface)
5. [Delivery Logic](#5-delivery-logic)
6. [Feedback Processing (Bounces)](#6-feedback-processing-bounces)
7. [Resilience & Safety](#7-resilience--safety)
8. [Configuration](#8-configuration)
9. [Flow Coverage](#9-flow-coverage)

---

## 1. Overview

The Email Worker consumes messages from the Notification Queue (`SQS`), processes them using the core notification logic, and dispatches them to an external email provider. Unlike generic webhooks, email delivery requires complex template rendering (handled provider-side) and asynchronous status tracking (handling bounces/spam reports).

### Responsibilities
*   **Adapter Pattern**: Implements the `core.NotificationChannel` interface.
*   **Data Preparation**: Converts domain objects (WatchPoints, Forecasts) into flattened template variables, handling timezone conversions.
*   **Provider Integration**: Abstracts the specific email service (SendGrid/SES).
*   **Feedback Loop**: Processes webhooks from providers to disable bad channels (hard bounces).
*   **Safety**: Enforces PII redaction in logs and respects global test mode.

---

## 2. Worker Structure

The worker is composed of the SQS handler, the channel implementation, and a rate limiter.

```go
type EmailWorker struct {
    channel        *EmailChannel
    rateLimiter    core.RateLimitHandler
    circuitBreaker *gobreaker.CircuitBreaker
    logger         *slog.Logger
}

// EmailChannel implements core.NotificationChannel
type EmailChannel struct {
    provider        EmailProvider
    templates       TemplateService
    deliveryRepo    core.DeliveryManager
    defaultFromAddr string
}
```

### Handler Entrypoint

The Lambda handler performs the following high-level orchestration:
1.  **Circuit Breaker Check**: If the provider is globally down (high error rate), Nack the message immediately to leave it in the queue.
2.  **Deduplication**: Calls `deliveryRepo.EnsureDeliveryExists`. If the record exists and is already `sent` or `delivered`, acknowledge and skip.
3.  **Delivery**: Invokes `channel.Deliver`.
4.  **Result Handling**: Updates the delivery status in the DB based on the result (Success, Retry, Failed).

---

## 3. Template Architecture

We utilize **Provider-Side Rendering** (e.g., SendGrid Dynamic Templates) to allow non-engineers to manage layout and copy. The worker's job is to map internal states to external Template IDs and prepare the data payload.

### 3.1 Template Configuration

```go
// TemplateConfig maps internal logic to provider IDs
type TemplateConfig struct {
    // Key: TemplateSet Name (e.g., "default", "wedding")
    // Value: Map of EventType -> Provider Template ID (e.g., "d-12345...")
    Sets map[string]map[types.EventType]string `json:"sets"`
}

func (c *TemplateConfig) Validate() error {
    // Ensures "default" set exists and covers all critical EventTypes
    return nil
}
```

### 3.2 Template Service

Responsible for data transformation and timezone localization.

```go
type TemplateData map[string]interface{}

*`SenderIdentity` is consolidated in `01-foundation-types.md` (Section 10.4).*

This module uses `types.SenderIdentity` for email sender information.

type TemplateService interface {
    // Resolve returns the provider template ID.
    //
    // **Soft-Fail Fallback Logic (VERT-004)**: If the named template set (e.g., "wedding")
    // cannot be resolved (not found in TemplateConfig or provider returns error), the
    // implementation MUST automatically retry resolution using the "default" template set
    // before returning an error. This ensures notifications are delivered even if specific
    // vertical templates are misconfigured or not yet provisioned.
    //
    // Resolution order:
    //   1. Attempt: Resolve(requestedSet, eventType)
    //   2. Fallback: Resolve("default", eventType) if step 1 fails
    //   3. Error: Return error only if both attempts fail
    Resolve(set string, eventType types.EventType) (string, error)

    // Prepare converts the notification into flat variables.
    // - Extracts 'timezone' from n.Payload (defaulting to UTC if missing).
    // - Converts UTC timestamps to human-readable strings in that timezone.
    // - Formats numbers (e.g., "0.55" -> "55%").
    // - Determines Sender Name based on TemplateSet.
    Prepare(n *types.Notification) (TemplateData, SenderIdentity, error)
}
```

**Timezone Handling**:
The `Prepare` method relies on the `timezone` key being present in the `Notification.Payload` map (populated by the Eval Worker). If invalid or missing, it defaults to UTC. It uses standard layouts (e.g., "Mon, Jan 2 at 3:04 PM") for formatting time strings passed to the template.

---

## 4. Provider Interface

This interface decouples the worker from the specific API (SendGrid, SES, Postmark). The implementation resides in `10-external-integrations.md`.

```go
// Errors
var ErrRecipientBlocked = errors.New("recipient blocked by provider")

// Helper to identify blocklist errors across implementations
func IsBlocklistError(err error) bool {
    return errors.Is(err, ErrRecipientBlocked)
}

// EmailProvider defines the contract for transmission
type EmailProvider interface {
    // Send transmits the email. 
    // Returns provider_message_id on success.
    // Returns ErrRecipientBlocked for suppression lists/hard bounces.
    Send(ctx context.Context, input SendInput) (string, error)
}

*`SendInput` is consolidated in `01-foundation-types.md` (Section 10.4).*

This module uses `types.SendInput` for email transmission payloads.
```

**Attachments**: Not supported in v1. Invoices and reports are handled via links to hosted files.

---

## 5. Delivery Logic

The `Deliver` method implements the `core.NotificationChannel` interface.

```go
func (e *EmailChannel) Deliver(ctx context.Context, payload []byte, destination string) (*core.DeliveryResult, error) {
    // 1. Redact destination for logging
    e.logger.Info("attempting delivery", "dest", RedactEmail(destination))

    // 2. Parse Notification Payload
    var n types.Notification
    if err := json.Unmarshal(payload, &n); err != nil {
        return nil, err // Permanent failure
    }

    // 3. Test Mode Bypass
    if n.TestMode {
        e.logger.Info("test mode: suppressing email", "notification_id", n.ID)
        return &core.DeliveryResult{
            Status: core.DeliveryStatusSkipped,
            ProviderMessageID: "test-simulated",
        }, nil
    }

    // 4. Prepare Template Data (with Soft-Fail Fallback - VERT-004)
    // The Resolve method implements automatic fallback to "default" template set
    // if the requested template set cannot be resolved.
    tmplID, err := e.templates.Resolve(n.TemplateSet, n.EventType)
    if err != nil {
        // Both requested set and default fallback failed - permanent failure
        e.logger.Error("template resolution failed for all sets",
            "requested_set", n.TemplateSet, "event_type", n.EventType, "error", err)
        return nil, err
    }
    
    data, sender, err := e.templates.Prepare(&n)
    if err != nil {
        return nil, err
    }

    // 5. Send via Provider
    msgID, err := e.provider.Send(ctx, SendInput{
        To:           destination,
        From:         sender,
        TemplateID:   tmplID,
        TemplateData: data,
        ReferenceID:  n.ID,
    })

    // 6. Handle Synchronous Failures
    if err != nil {
        if IsBlocklistError(err) {
            // Treat as Hard Bounce immediately (Permanent Failure)
            return &core.DeliveryResult{
                Status:        core.DeliveryStatusBounced,
                FailureReason: "address_blocked",
                Retryable:     false,
            }, nil
        }
        // Other errors trigger retry logic in core
        return nil, err 
    }

    return &core.DeliveryResult{
        ProviderMessageID: msgID,
        Status:            core.DeliveryStatusSent,
    }, nil
}
```

### Terminal Failure Handling (State Rollback)

**State Rollback**: If a delivery fails terminally (e.g., 401 Unauthorized, recipient blocked), the worker MUST call `deliveryManager.CheckAggregateFailure`. If this returns `true` (all sibling deliveries for this notification have failed), the worker MUST call `deliveryManager.ResetNotificationState`.

**Rationale**: This rollback ensures the Evaluation Engine perceives the WatchPoint as "not notified" during the next cycle, forcing a retry of the alert generation. Without this, a transient full-channel failure would result in permanently missed notifications.

```go
func (w *EmailWorker) handleTerminalFailure(ctx context.Context, notifID, wpID string) error {
    allFailed, err := w.deliveryMgr.CheckAggregateFailure(ctx, notifID)
    if err != nil {
        return err
    }
    if allFailed {
        w.logger.Warn("all delivery channels failed, resetting notification state",
            "notification_id", notifID, "watchpoint_id", wpID)
        return w.deliveryMgr.ResetNotificationState(ctx, wpID)
    }
    return nil
}
```

---

## 6. Feedback Processing (Bounces)

Emails often fail asynchronously. The `BounceProcessor` logic handles webhooks from the provider.

### 6.1 Structures

```go
// BounceEvent normalizes provider-specific payloads
type BounceEvent struct {
    ProviderMessageID string
    EmailAddress      string
    Reason            string
    Type              FeedbackType // Bounce, Complaint
    Timestamp         time.Time
}

type FeedbackType string
const (
    FeedbackBounce    FeedbackType = "bounce"
    FeedbackComplaint FeedbackType = "complaint"
)
```

### 6.2 Processor Logic

```go
type BounceProcessor struct {
    deliveryRepo  core.DeliveryManager
    healthRepo    core.ChannelHealthRepository
    userRepo      db.UserRepository  // For owner notification on channel failure
    emailProvider EmailProvider      // For sending system alerts
    templates     TemplateService    // For resolving system alert template
}

func (b *BounceProcessor) Process(ctx context.Context, event BounceEvent) error {
    // 1. Update Delivery Status
    // Find delivery by ProviderMessageID
    // Set Status = 'bounced', FailureReason = event.Reason

    // 2. Manage Channel Health
    // If Type == Complaint (Spam Report):
    //    IMMEDIATELY Disable channel via healthRepo.DisableChannel
    //
    // If Type == Hard Bounce:
    //    Increment failure count via healthRepo.IncrementChannelFailure
    //    If count >= 3, Disable channel

    // 3. Notify Owner
    // Use userRepo.GetOwnerEmail(orgID) to resolve the Owner's email address.
    // Resolve the template ID for EventSystemAlert using templates.Resolve.
    // Call emailProvider.Send to alert the owner of the channel failure.
    // This ensures organization owners are aware when email delivery fails.

    return nil
}

// **Concurrency Strategy (Optimistic Locking)**:
// To disable a channel inside the `channels` JSONB array:
// 1. Fetch WatchPoint with `config_version`.
// 2. Locate and modify the channel in memory.
// 3. Update with `WHERE id=$id AND config_version=$version`.
// 4. If 0 rows updated (race condition), refetch and retry.
```

*Note: The HTTP handler receiving the webhook resides in the API layer (`10-external-integrations`), which calls this processor.*

---

## 7. Resilience & Safety

### 7.1 PII Redaction
Logs must never contain raw email addresses.
```go
func RedactEmail(email string) string {
    // Returns "j***@gmail.com"
}
```

### 7.2 Global Circuit Breaker
Uses `gobreaker` to detect provider outages.
*   **Trigger**: Consecutive 5xx errors from `EmailProvider`.
*   **Action**: Nack messages immediately (preserve in SQS).
*   **Recovery**: Half-open state tests single delivery before resuming full throughput.

### 7.3 Rate Limiting
Uses `core.RateLimitHandler`.
*   If `provider.Send` returns a Rate Limit error (429), the worker records the `Retry-After` header value and returns a specific error to the SQS handler to delay the message visibility.

---

## 8. Configuration

This worker requires extensions to the configuration defined in `03-config.md`.

**Required Extension**: The `EmailConfig` struct in `03-config.md` must include a `Templates` field to map template IDs.

```go
type EmailConfig struct {
    // JSON string mapping "template_set" -> "event_type" -> "provider_id"
    // Example: {"default": {"threshold_crossed": "d-123..."}}
    Templates       string `envconfig:"EMAIL_TEMPLATES_JSON" validate:"required,json"`
    DefaultFromAddr string `envconfig:"EMAIL_FROM_ADDRESS"`
    Provider        string `envconfig:"EMAIL_PROVIDER" default:"sendgrid"`
}
```

---

## 9. Flow Coverage

| Flow ID | Flow Name | Implementation |
|---|---|---|
| `NOTIF-001` | Email Delivery | `EmailChannel.Deliver` → `Provider.Send` |
| `NOTIF-004` | Retry Logic | Handled via SQS visibility + `core.DeliveryManager` |
| `NOTIF-005` | Rate Limiting | `core.RateLimitHandler` integration |
| `NOTIF-006` | Bounce Handling | `BounceProcessor.Process` |
| `TEST-002` | Test Mode | `Notification.TestMode` check in `Deliver` |
| `VERT-003` | Template Selection | `TemplateService.Resolve` using `TemplateConfig` |