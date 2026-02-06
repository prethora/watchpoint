# 08c - Webhook Worker

> **Purpose**: Defines the Webhook Worker responsible for delivering notifications via HTTP POST to external systems. It handles platform auto-detection (Slack, Teams, Discord), payload formatting, HMAC signing for security, and strict SSRF protection.
> **Package**: `worker/webhook`
> **Dependencies**: `08a-notification-core.md` (Interfaces), `01-foundation-types.md` (Security/Types), `03-config.md`

---

## Table of Contents

1. [Overview](#1-overview)
2. [Worker Structure](#2-worker-structure)
3. [Platform Architecture](#3-platform-architecture)
4. [Payload Schemas](#4-payload-schemas)
5. [Security Implementation](#5-security-implementation)
6. [Delivery Logic](#6-delivery-logic)
7. [Configuration](#7-configuration)
8. [Migrations & Extensions](#8-migrations--extensions)
9. [Flow Coverage](#9-flow-coverage)

---

## 1. Overview

The Webhook Worker consumes messages from the Notification Queue, formats them into platform-specific JSON (e.g., Slack Block Kit, Adaptive Cards), secures them with HMAC signatures, and delivers them via HTTP.

### Responsibilities
*   **Polymorphic Formatting**: Automatically detects the target platform from the URL and shapes the JSON payload accordingly.
*   **Security**: Signs payloads using a rotating secret scheme (Dual-Validity) and prevents Server-Side Request Forgery (SSRF) via strict IP filtering.
*   **Resilience**: Respects downstream rate limits (`Retry-After` headers) and handles "soft failures" (HTTP 200 with error bodies).
*   **Ordering**: Propagates event sequence numbers to allow clients to reconstruct the timeline.

---

## 2. Worker Structure

The worker implements the `core.NotificationChannel` interface defined in `08a-notification-core.md`.

```go
// WebhookChannel implements core.NotificationChannel
type WebhookChannel struct {
    registry     *PlatformRegistry
    signer       core.SignatureManager
    httpClient   *http.Client // Configured with SSRF transport
    config       *config.WebhookConfig
    logger       *slog.Logger
}

// NewWebhookChannel is the factory used by the Lambda entrypoint
func NewWebhookChannel(
    cfg *config.WebhookConfig,
    signer core.SignatureManager,
    logger *slog.Logger,
) *WebhookChannel
```

### Factory Logic
The factory configures the `httpClient` with a custom `Transport` and `CheckRedirect` function to enforce SSRF protection on every network hop.

---

## 3. Platform Architecture

To support Flow `NOTIF-007` (Platform-Specific Formatting), the worker uses a registry pattern to select the correct formatter strategy based on the URL.

### 3.1 Interfaces & Enums

```go
type Platform string
const (
    PlatformGeneric    Platform = "generic"
    PlatformSlack      Platform = "slack"
    PlatformDiscord    Platform = "discord"
    PlatformTeams      Platform = "teams"
    PlatformGoogleChat Platform = "google_chat"
)

type PlatformFormatter interface {
    // Format transforms the core Notification into the platform-specific JSON payload.
    // The generic config map is passed to extract platform-specific overrides if needed.
    Format(ctx context.Context, n *types.Notification, config map[string]any) ([]byte, error)
    
    // Platform returns the enum identifier for metrics.
    Platform() Platform

    // ValidateResponse interprets the HTTP response body to catch "soft failures"
    // (e.g., Slack returning HTTP 200 with "ok": false).
    ValidateResponse(statusCode int, body []byte) error
}
```

### 3.2 Registry

```go
type PlatformRegistry struct {
    formatters map[Platform]PlatformFormatter
}

// Detect inspects the URL string to determine the target Platform.
//
// **Logic**:
// 1. Check `Channel.Config["platform_override"]`. If present and valid, use that platform.
// 2. Fallback: Inspect URL patterns (regex):
//    - "hooks.slack.com" -> PlatformSlack
//    - "discord.com/api/webhooks" -> PlatformDiscord
//    - ".webhook.office.com" OR ".logic.azure.com" -> PlatformTeams
//    - "chat.googleapis.com" -> PlatformGoogleChat
// 3. Fallback: Use `PlatformGeneric`.
func (r *PlatformRegistry) Detect(url string, config map[string]any) Platform

func (r *PlatformRegistry) Get(p Platform) PlatformFormatter

// CheckDeprecation examines a webhook URL for platform-specific deprecation patterns.
// Returns a warning message and deprecation status based on known platform lifecycle information.
//
// **Deprecation Patterns**:
// - Legacy Teams Connectors (*.webhook.office.com): "Teams Connectors are retiring
//   December 2025. Migrate to Power Automate Workflows (*.logic.azure.com)."
// - Other platforms may be added as deprecation announcements are made.
//
// **Usage**: Called by the API layer (GET /watchpoints/{id}) to populate response
// Meta.Warnings for proactive user notification. Also usable during Create/Update
// validation to warn about deprecated configurations.
//
// **Return Values**:
// - warning: Human-readable deprecation message (empty string if not deprecated)
// - isDeprecated: true if the URL matches a known deprecated pattern
func (r *PlatformRegistry) CheckDeprecation(url string) (warning string, isDeprecated bool)
```

**Deprecation Detection Implementation**:
```go
func (r *PlatformRegistry) CheckDeprecation(url string) (string, bool) {
    // Legacy Teams Office 365 Connectors (retiring December 2025)
    if strings.Contains(url, ".webhook.office.com") {
        return "Teams Connectors are retiring December 2025. Migrate to Power Automate Workflows.", true
    }
    // Add additional deprecation checks as platforms announce changes
    return "", false
}
```

---

## 4. Payload Schemas

These internal structs ensure type safety when marshalling JSON for specific platforms.

### 4.1 Slack (Block Kit)
```go
type SlackPayload struct {
    Text   string       `json:"text"`   // Fallback text for push notifications
    Blocks []SlackBlock `json:"blocks"` // Rich layout
}

type SlackBlock struct {
    Type     string         `json:"type"`             // "section", "header", "context"
    Text     *SlackText     `json:"text,omitempty"`
    Fields   []*SlackText   `json:"fields,omitempty"`
    Elements []*SlackText   `json:"elements,omitempty"`
}

type SlackText struct {
    Type string `json:"type"` // "plain_text", "mrkdwn"
    Text string `json:"text"`
}
```

### 4.2 Microsoft Teams (Adaptive Cards)
Targeting the Power Automate Workflow schema.
```go
type TeamsPayload struct {
    Type        string             `json:"type"` // "message"
    Attachments []TeamsAttachment  `json:"attachments"`
}

type TeamsAttachment struct {
    ContentType string       `json:"contentType"` // "application/vnd.microsoft.card.adaptive"
    Content     AdaptiveCard `json:"content"`
}

type AdaptiveCard struct {
    Type    string         `json:"type"`    // "AdaptiveCard"
    Version string         `json:"version"` // "1.4"
    Body    []AdaptiveItem `json:"body"`
}
```

### 4.3 Discord (Embeds)
```go
type DiscordPayload struct {
    Username  string         `json:"username"`
    AvatarURL string         `json:"avatar_url"`
    Content   string         `json:"content"` // Fallback/Ping text
    Embeds    []DiscordEmbed `json:"embeds"`
}

type DiscordEmbed struct {
    Title       string         `json:"title"`
    Description string         `json:"description"`
    Color       int            `json:"color"` // Decimal color code
    Fields      []DiscordField `json:"fields"`
    Footer      *DiscordFooter `json:"footer,omitempty"`
}
```

### 4.4 Google Chat (Cards v2)
```go
type GoogleChatPayload struct {
    Cards []GoogleCard `json:"cards"`
}

type GoogleCard struct {
    Header   GoogleHeader    `json:"header"`
    Sections []GoogleSection `json:"sections"`
}
```

### 4.5 Generic Payload
This formatter outputs the strict `core.NotificationPayload` structure defined in `08a-notification-core.md`, ensuring downstream consumers receive a stable contract.

---

## 5. Security Implementation

### 5.1 HMAC Signing (Dual-Validity)
Implements Flow `HOOK-001` and `SEC-006`.

The `Format` method extracts `secret` and `previous_secret` from the `types.Channel.Config` map. The `SignatureManager` (injected dependency) uses these to generate the signature header.

```go
// Header Format:
// X-Watchpoint-Signature: t=1706745600,v1=abc...,v1_old=def...
```

### 5.2 SSRF Protection (Transport Layer)
Implements Flow `API-002`. The worker must prevent calls to internal infrastructure (e.g., AWS Metadata Service `169.254.169.254`, localhost).

```go
// SafeTransport wraps http.Transport to enforce IP blocklists.
// It uses types.SSRFValidator from 01-foundation-types.
type SafeTransport struct {
    Validator types.SSRFValidator
    Base      *http.Transport
}

// CheckRedirect is used by http.Client to validate redirect targets.
func CheckRedirect(req *http.Request, via []*http.Request) error {
    // 1. Validate number of redirects against Config.MaxRedirects
    // 2. Resolve req.URL.Host to IP
    // 3. Call Validator(ip)
    return nil
}
```

---

## 6. Delivery Logic

### 6.1 Deliver Method
The `Deliver` method orchestrates the transmission and response interpretation.

```go
func (w *WebhookChannel) Deliver(ctx context.Context, payload []byte, destination string) (*core.DeliveryResult, error) {
    // 1. Test Mode Check (Flow TEST-002)
    // If n.TestMode: Log payload, return {Status: Skipped, ProviderMsgID: "test-mode"}
    
    // 2. Prepare Request
    req, _ := http.NewRequestWithContext(ctx, "POST", destination, bytes.NewReader(payload))
    
    // 3. Set Headers (Merge Logic)
    // - System: Content-Type, User-Agent
    // - Security: X-Watchpoint-Signature, X-Watchpoint-Event, X-Watchpoint-Sequence
    // - User Config: Merge 'headers' from config (System/Security take precedence)
    
    // 4. Execute
    resp, err := w.httpClient.Do(req)
    
    // 5. Handle Network/SSRF Errors
    if err != nil {
        // Return Retryable=true for timeouts, false for SSRF blocks
    }
    
    // 6. Handle HTTP Status
    // 429: Parse Retry-After -> Return Retryable=true, RetryAfter=duration
    // 2xx: Call formatter.ValidateResponse(resp) -> If error, treat as 500 (Retryable)
    // 4xx: Return Retryable=false (Permanent Failure)
    // 5xx: Return Retryable=true (Transient Failure)
}
```

### 6.2 Terminal Failure Handling (State Rollback)

**State Rollback**: If a delivery fails terminally (e.g., HTTP 401 Unauthorized, HTTP 410 Gone), the worker MUST call `deliveryManager.CheckAggregateFailure`. If this returns `true` (all sibling deliveries for this notification have failed), the worker MUST call `deliveryManager.ResetNotificationState`.

**Rationale**: This rollback ensures the Evaluation Engine perceives the WatchPoint as "not notified" during the next cycle, forcing a retry of the alert generation. Without this, a transient full-channel failure would result in permanently missed notifications.

```go
func (w *WebhookWorker) handleTerminalFailure(ctx context.Context, notifID, wpID string) error {
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

### 6.3 Retry Strategy
The worker does **not** sleep/block for long durations.
1.  If `Deliver` returns `Retryable: true`.
2.  The Lambda Handler calculates the backoff (1s, 5s, 30s) or uses `RetryAfter`.
3.  The Handler **ACKs** the current SQS message.
4.  The Handler **Publishes** a new SQS message with `DelaySeconds` set to the backoff.

**Long Rate Limits (>15 mins):**
If `Retry-After` exceeds SQS visibility limits (15m), the worker must **not** re-queue to SQS. Instead:
1. Update `notification_deliveries` status to `'deferred'`.
2. Set `next_retry_at` to the specific future time.
3. ACK the SQS message.
4. Reliance: The `RequeueDeferredNotifications` job (see `09-scheduled-jobs.md`) will pick this up.

### 6.4 Provider Message IDs
Since generic webhooks lack receipt IDs:
*   **Primary**: Look for headers like `X-Request-Id` or `X-Slack-Req-Id`.
*   **Fallback**: Generate `http-{status}-{timestamp}`.

**Generic Webhook Synthetic ID Format:**
For generic webhooks where no upstream ID is returned in response headers, the worker
MUST generate a synthetic ID to ensure the `notification_deliveries.provider_message_id`
column always has a traceable reference. This enables audit trails and debugging.

Format: `generic-{status}-{timestamp}-{uuid_short}`

Example: `generic-200-1706745600-a1b2c3d4`

```go
func generateSyntheticID(statusCode int) string {
    return fmt.Sprintf("generic-%d-%d-%s",
        statusCode,
        time.Now().Unix(),
        uuid.New().String()[:8],
    )
}
```

---

## 7. Configuration

Requires extension of `03-config.md`.

```go
type WebhookConfig struct {
    UserAgent      string        `envconfig:"WEBHOOK_USER_AGENT" default:"WatchPoint-Webhook/1.0"`
    DefaultTimeout time.Duration `envconfig:"WEBHOOK_TIMEOUT" default:"10s"`
    MaxRedirects   int           `envconfig:"WEBHOOK_MAX_REDIRECTS" default:"3"`
}
```

---

## 8. Migrations & Extensions

*All required extensions have been consolidated in the foundation documents.*

### 8.1 Configuration
The `WebhookConfig` struct is defined in `03-config.md`. This module uses `config.WebhookConfig` for `UserAgent`, `DefaultTimeout`, and `MaxRedirects` settings.

### 8.2 Core Interfaces
The `DeliveryResult.RetryAfter` and `DeliveryResult.Terminal` fields are defined in `08a-notification-core.md` (Section 5.1).

---

## 9. Flow Coverage

| Flow ID | Description | Implementation |
|---|---|---|
| `NOTIF-001` | Webhook Delivery | `WebhookChannel.Deliver` |
| `NOTIF-004` | Retry Logic | `Deliver` error mapping + SQS DelaySeconds |
| `NOTIF-005` | Rate Limit (429) | `Deliver` parses `Retry-After` header |
| `NOTIF-007` | Platform Formatting | `PlatformRegistry` + `PlatformFormatter` |
| `HOOK-001` | Secret Rotation | `SignatureManager` handles Dual-Validity |
| `HOOK-002` | Deprecation Warnings | `PlatformRegistry.CheckDeprecation` |
| `SEC-006` | Payload Signing | `SignatureManager` (HMAC-SHA256) |
| `API-002` | SSRF Protection | `SafeTransport` + `CheckRedirect` |
| `TEST-002` | Test Mode | `Deliver` shortcuts execution if TestMode=true |