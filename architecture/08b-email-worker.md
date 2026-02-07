# 08b - Email Worker

> **Purpose**: Defines the Email Worker responsible for delivering notifications via AWS SES. It handles client-side template rendering (Go `html/template` + `text/template` with `go:embed`), rate limiting, and asynchronous feedback processing (bounces/complaints via SNS).
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

The Email Worker consumes messages from the Notification Queue (`SQS`), processes them using the core notification logic, and dispatches them to AWS SES for delivery. Template rendering is performed client-side using Go `html/template` and `text/template` with embedded templates via `go:embed`. Asynchronous status tracking (bounces/complaints) is handled via SNS notifications from SES.

### Responsibilities
*   **Adapter Pattern**: Implements the `core.NotificationChannel` interface.
*   **Template Rendering**: Client-side rendering using a `Renderer` struct with embedded Go templates. Converts domain objects (WatchPoints, Forecasts) into rendered HTML/text email bodies, handling timezone conversions.
*   **Provider Integration**: Sends pre-rendered emails via AWS SES (v2 SDK). Auth is via IAM roles (no API key needed).
*   **Feedback Loop**: Processes bounce/complaint events from SES via SNS (`ParseSNSBounceEvent`) to disable bad channels (hard bounces).
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
    renderer        *Renderer
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

We utilize **Client-Side Rendering** using Go `html/template` and `text/template` with templates embedded via `//go:embed`. This approach keeps templates version-controlled alongside the code, eliminates external template management dependencies, and enables type-safe template data.

### 3.1 Renderer

The `Renderer` struct handles template loading and rendering. Templates are organized by template set and event type.

```go
// RenderedEmail holds the output of template rendering.
type RenderedEmail struct {
    Subject  string
    BodyHTML string
    BodyText string
}

// Renderer loads and renders embedded email templates.
type Renderer struct {
    // Embedded templates loaded from go:embed filesystem
    templates map[string]map[types.EventType]*template.Template
}

// Render produces a RenderedEmail for the given template set, event type, and notification.
//
// **Soft-Fail Fallback Logic (VERT-004)**: If the named template set (e.g., "wedding")
// cannot be resolved, the implementation MUST automatically retry using the "default"
// template set before returning an error.
//
// Resolution order:
//   1. Attempt: Render(requestedSet, eventType, notification)
//   2. Fallback: Render("default", eventType, notification) if step 1 fails
//   3. Error: Return error only if both attempts fail
func (r *Renderer) Render(set string, eventType types.EventType, n *types.Notification) (*RenderedEmail, types.SenderIdentity, error)
```

*`SenderIdentity` is consolidated in `01-foundation-types.md` (Section 10.4).*

This module uses `types.SenderIdentity` for email sender information.

**Timezone Handling**:
The `Render` method relies on the `timezone` key being present in the `Notification.Payload` map (populated by the Eval Worker). If invalid or missing, it defaults to UTC. It uses standard layouts (e.g., "Mon, Jan 2 at 3:04 PM") for formatting time strings in the rendered output.

---

## 4. Provider Interface

This interface decouples the worker from the specific email provider. The SES implementation resides in `10-external-integrations.md`.

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

    // 4. Render Template (with Soft-Fail Fallback - VERT-004)
    // The Render method implements automatic fallback to "default" template set
    // if the requested template set cannot be resolved.
    rendered, sender, err := e.renderer.Render(n.TemplateSet, n.EventType, &n)
    if err != nil {
        // Both requested set and default fallback failed - permanent failure
        e.logger.Error("template rendering failed for all sets",
            "requested_set", n.TemplateSet, "event_type", n.EventType, "error", err)
        return nil, err
    }

    // 5. Send via Provider (SES)
    msgID, err := e.provider.Send(ctx, SendInput{
        To:          destination,
        From:        sender,
        Subject:     rendered.Subject,
        BodyHTML:    rendered.BodyHTML,
        BodyText:    rendered.BodyText,
        ReferenceID: n.ID,
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

Emails often fail asynchronously. SES publishes bounce and complaint notifications to an SNS topic. The `BounceProcessor` logic handles these SNS events, parsed via `ParseSNSBounceEvent()`.

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

*Note: Bounce/complaint events arrive via SNS notifications from SES. The SNS message is parsed using `ParseSNSBounceEvent()` which normalizes the SES-specific payload into the `BounceEvent` struct.*

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
Uses `gobreaker` to detect SES outages.
*   **Trigger**: Consecutive errors from `EmailProvider` (SES).
*   **Action**: Nack messages immediately (preserve in SQS).
*   **Recovery**: Half-open state tests single delivery before resuming full throughput.

### 7.3 Rate Limiting
Uses `core.RateLimitHandler`.
*   If `provider.Send` returns a Rate Limit error (429), the worker records the `Retry-After` header value and returns a specific error to the SQS handler to delay the message visibility.

---

## 8. Configuration

This worker requires extensions to the configuration defined in `03-config.md`.

The `EmailConfig` struct in `03-config.md` defines the email provider settings. Template rendering is handled client-side via `go:embed` -- no external template IDs are needed.

```go
type EmailConfig struct {
    FromAddress string `envconfig:"EMAIL_FROM_ADDRESS" default:"alerts@watchpoint.io"`
    FromName    string `envconfig:"EMAIL_FROM_NAME" default:"WatchPoint Alerts"`
    SESRegion   string `envconfig:"SES_REGION" default:"us-east-1"`
    Provider    string `envconfig:"EMAIL_PROVIDER" default:"ses"`
}
```

**IAM Permissions**: The Email Worker Lambda requires `ses:SendEmail` and `ses:SendRawEmail` permissions (defined in `04-sam-template.md`). No API key or SSM secret is needed for SES authentication.

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