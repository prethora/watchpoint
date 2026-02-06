# 05e - API Billing & Usage

> **Purpose**: Defines the HTTP handlers, service interfaces, data models, and business logic for subscription management, usage tracking, and payment provider integration (Stripe).
> **Package**: `package handlers`
> **Dependencies**: `05a-api-core.md`, `01-foundation-types.md`, `02-foundation-db.md`, `10-external-integrations.md`

---

## Table of Contents

1. [Overview](#1-overview)
2. [Database Extensions](#2-database-extensions)
3. [Type Definitions (Package types)](#3-type-definitions-package-types)
4. [Domain Interfaces](#4-domain-interfaces)
5. [Billing Handler](#5-billing-handler)
6. [Usage Handler](#6-usage-handler)
7. [Webhook Handler](#7-webhook-handler)
8. [Route Registration](#8-route-registration)
9. [Business Logic & Edge Cases](#9-business-logic--edge-cases)
10. [Flow Coverage](#10-flow-coverage)

---

## 1. Overview

This module manages the financial lifecycle of an Organization. It decouples the API layer from the specific payment provider (Stripe) using service interfaces.

### Key Responsibilities
*   **Subscription Management**: generating Checkout sessions for upgrades and Portal sessions for self-serve management.
*   **Usage Reporting**: Aggregating real-time state (active WatchPoints) and transient counters (API calls) into a unified usage report.
*   **Synchronization**: Processing asynchronous webhooks to keep local subscription state in sync with the provider.
*   **Invoicing**: Proxying invoice history access.

---

## 2. Database Extensions

*Schema defined in `02-foundation-db.md`.*

The billing-related columns (`subscription_status`, `last_subscription_event_at`, `payment_failed_at`) are consolidated in the Foundation Database document (Section 6.5). This module uses those definitions.

---

## 3. Type Definitions (Package types)

*All billing types are consolidated in `01-foundation-types.md` (Section 10.2).*

This module uses the following types from `package types`:
- `types.SubscriptionDetails`
- `types.PaymentMethodInfo`
- `types.Invoice`
- `types.ListInvoicesParams`
- `types.UsageSnapshot`
- `types.LimitDetail`
- `types.UsageDataPoint`
- `types.DailyUsageStat`
- `types.SubscriptionStatus` (Enum)
- `types.ResourceType` (Enum)
- `types.ResetFrequency` (Enum)
- `types.TimeGranularity` (Enum)

### Local Request/Response Types

```go
// RedirectURLs is used by the BillingService to guide the user after Stripe checkout.
type RedirectURLs struct {
    Success string
    Cancel  string
}
```

---

## 4. Domain Interfaces

These interfaces isolate the HTTP handlers from external integrations and database complexity.

### 4.1 Services

```go
// BillingService abstracts interactions with the payment provider (Stripe).
// Implemented in 10-external-integrations.md.
type BillingService interface {
    // EnsureCustomer checks if the org has a Stripe Customer ID.
    // If not, it creates one idempotent. Required before Checkout.
    EnsureCustomer(ctx context.Context, orgID string, email string) (customerID string, err error)

    // CreateCheckoutSession generates a URL for the user to enter payment info.
    CreateCheckoutSession(
        ctx context.Context, 
        orgID string, 
        targetPlan types.PlanTier, 
        redirectURLs RedirectURLs,
    ) (checkoutURL string, sessionID string, err error)

    // CreatePortalSession generates a URL for self-serve billing management.
    CreatePortalSession(
        ctx context.Context, 
        orgID string, 
        returnURL string,
    ) (portalURL string, err error)

    // GetInvoices retrieves billing history directly from the provider.
    GetInvoices(
        ctx context.Context, 
        orgID string, 
        params ListInvoicesParams,
    ) ([]*Invoice, types.PageInfo, error)

    // GetSubscription returns the current raw subscription state from the provider.
    GetSubscription(ctx context.Context, orgID string) (*SubscriptionDetails, error)
}

// PlanRegistry defines the authoritative limits for each tier.
type PlanRegistry interface {
    // GetLimits returns the limits (WatchPoints, API calls) for a given tier.
    GetLimits(tier types.PlanTier) types.PlanLimits
}

// AuditLogger defines the contract for recording business events.
// This interface allows dependency injection into the handler.
type AuditLogger interface {
    Log(ctx context.Context, event types.AuditEvent) error
}
```

### 4.2 Usage Reporting

```go
// UsageReporter aggregates usage metrics for a specific period.
type UsageReporter interface {
    // GetCurrentUsage returns a snapshot of usage against limits for the current period.
    //
    // **Aggregation Strategy (VERT-001)**: The implementation MUST aggregate across all
    // source rows for the organization:
    //   `SELECT SUM(api_calls_count) FROM rate_limits WHERE organization_id = $1`
    // This returns the total usage against the plan limit, combining all vertical apps.
    GetCurrentUsage(ctx context.Context, orgID string) (*UsageSnapshot, error)

    // GetUsageHistory returns timeseries data for charting.
    //
    // **Composite Key Handling (VERT-001)**: The implementation MUST handle the new
    // `usage_history` composite key `(organization_id, date, source)`. When presenting
    // the organization-wide view, aggregate by date across all sources:
    //   `SELECT date, SUM(api_calls), SUM(watchpoints), SUM(notifications)
    //    FROM usage_history WHERE organization_id = $1 GROUP BY date`
    GetUsageHistory(
        ctx context.Context,
        orgID string,
        rangeStart, rangeEnd time.Time,
        granularity TimeGranularity,
    ) ([]*UsageDataPoint, error)
}
```

### 4.3 Webhook Processing

```go
// WebhookEventHandler processes verified events from the payment provider.
type WebhookEventHandler interface {
    // HandleEvent dispatches the event based on its type.
    HandleEvent(ctx context.Context, payload []byte, signature string) error
}
```

### 4.4 Extended Repositories

```go
// SubscriptionStateRepository manages local billing state synchronization.
type SubscriptionStateRepository interface {
    // UpdateSubscriptionStatus atomically updates the plan and status.
    // MUST fail if eventTimestamp < stored last_subscription_event_at (Optimistic Locking).
    // MUST fail if Organization.deleted_at IS NOT NULL (Zombie check).
    UpdateSubscriptionStatus(
        ctx context.Context,
        orgID string,
        newPlan types.PlanTier,
        status SubscriptionStatus, 
        eventTimestamp time.Time,
    ) error
    
    // UpdatePaymentFailure records dunning state.
    UpdatePaymentFailure(ctx context.Context, orgID string, failedAt time.Time) error
}

// UsageHistoryRepository accesses pre-aggregated daily stats (from SCHED-002).
type UsageHistoryRepository interface {
    // Query retrieves usage statistics for the given time range.
    // The granularity parameter controls aggregation level (daily, monthly).
    // Implementation must handle aggregation (e.g., date_trunc) based on the granularity.
    Query(
        ctx context.Context,
        orgID string,
        start, end time.Time,
        granularity types.TimeGranularity,
    ) ([]DailyUsageStat, error)
}
```

---

## 5. Billing Handler

Handles synchronous billing actions initiated by the user.

### 5.1 Structure

```go
type BillingHandler struct {
    service      BillingService
    orgRepo      db.OrganizationRepository
    usageHandler *UsageHandler // Composition for grouped route registration
    audit        AuditLogger   // To log billing events
    validator    *core.Validator
    logger       *slog.Logger
}

func NewBillingHandler(
    svc BillingService,
    repo db.OrganizationRepository,
    usage *UsageHandler,
    audit AuditLogger,
    v *core.Validator,
    l *slog.Logger,
) *BillingHandler
```

### 5.2 Request/Response Models

```go
// POST /v1/billing/checkout-session
// Note: SuccessURL and CancelURL are intentionally omitted from the request struct.
// These URLs are constructed server-side using DashboardURL from configuration
// to prevent Open Redirect vulnerabilities.
type CreateCheckoutRequest struct {
    Plan types.PlanTier `json:"plan" validate:"required,oneof=starter pro business"`
}

type CheckoutResponse struct {
    CheckoutURL string    `json:"checkout_url"`
    SessionID   string    `json:"session_id"`
    ExpiresAt   time.Time `json:"expires_at"`
}

// POST /v1/billing/portal-session
// Note: ReturnURL is intentionally omitted from the request struct.
// The return URL is constructed server-side using DashboardURL from configuration
// to prevent Open Redirect vulnerabilities.
type CreatePortalRequest struct {
    // Empty struct - all redirect URLs are server-controlled
}

type PortalResponse struct {
    PortalURL string `json:"portal_url"`
}
```

### 5.3 Methods

```go
// CreateCheckoutSession (POST /v1/billing/checkout-session)
// 1. Validates org exists.
// 2. **Plan Validation**: Reject requests where `plan='free'` with a 400 Bad Request error.
//    Downgrades to free tier must be handled via the Stripe Portal, not checkout.
// 3. **Self-Healing Customer ID**: Calls `service.EnsureCustomer` to guarantee a Stripe
//    Customer ID exists. This handles edge cases where initial signup failed or data
//    was migrated without a customer ID.
// 4. **Server-Controlled Redirect URLs**: Constructs SuccessURL and CancelURL server-side
//    using `cfg.DashboardURL` (e.g., `cfg.DashboardURL + "/billing?success=true"` and
//    `cfg.DashboardURL + "/billing?canceled=true"`). This prevents Open Redirect
//    vulnerabilities by never accepting redirect URLs from client input.
// 5. Calls service.CreateCheckoutSession with the constructed URLs.
// 6. Logs "billing.checkout.created" audit event.
// 7. Returns 200 with URL.
func (h *BillingHandler) CreateCheckoutSession(w http.ResponseWriter, r *http.Request)

// CreatePortalSession (POST /v1/billing/portal-session)
// 1. **Self-Healing Customer ID**: Calls `service.EnsureCustomer` to guarantee a Stripe
//    Customer ID exists. This handles edge cases where the customer ID is missing due
//    to failed signup, data migration, or other edge cases.
// 2. **Server-Controlled Return URL**: Constructs the return URL server-side using
//    `cfg.DashboardURL` (e.g., `cfg.DashboardURL + "/billing"`). This prevents Open
//    Redirect vulnerabilities by never accepting the return URL from client input.
// 3. Calls service.CreatePortalSession with the constructed return URL.
// 4. Returns 200 with URL.
func (h *BillingHandler) CreatePortalSession(w http.ResponseWriter, r *http.Request)

// GetInvoices (GET /v1/billing/invoices)
// 1. Proxies request to service.GetInvoices.
// 2. Returns 200 with list.
func (h *BillingHandler) GetInvoices(w http.ResponseWriter, r *http.Request)

// GetSubscription (GET /v1/billing/subscription)
// 1. Calls service.GetSubscription to fetch latest state from Stripe.
// 2. **Opportunistic Sync (INFO-006)**: The handler MUST compare the returned Stripe
//    subscription status and plan against the local database record. If mismatched
//    (e.g., user just completed payment and was redirected before webhook arrived),
//    the handler issues a synchronous UPDATE to the organizations table to align
//    local state with Stripe before returning the response. This handles race conditions
//    where the webhook lags behind the user's redirect from Stripe Checkout.
//    - Compare: stripe.Status vs org.subscription_status, stripe.Plan vs org.plan
//    - If different: UPDATE organizations SET subscription_status=$1, plan=$2,
//      last_subscription_event_at=NOW() WHERE id=$3
// 3. Returns current status from provider (reflecting the synchronized state).
func (h *BillingHandler) GetSubscription(w http.ResponseWriter, r *http.Request)
```

---

## 6. Usage Handler

Provides visibility into consumption and limits.

### 6.1 Structure

```go
type UsageHandler struct {
    reporter   UsageReporter
    validator  *core.Validator
}
```

### 6.2 Models

```go
// GET /v1/usage
type UsageResponse struct {
    Period         DateRange      `json:"period"`
    Plan           types.PlanTier `json:"plan"`
    Snapshot       *UsageSnapshot `json:"snapshot"`
}

type DateRange struct {
    Start time.Time `json:"start"`
    End   time.Time `json:"end"`
}
```

### 6.3 Methods

```go
// GetCurrent (GET /v1/usage)
// 1. Calls reporter.GetCurrentUsage(orgID).
// 2. Returns usage snapshot with limit metadata.
// **Real-Time Accuracy**: The handler MUST use a **Direct Count** strategy
// (`SELECT COUNT(*) FROM watchpoints WHERE organization_id = $1 AND status != 'archived' AND deleted_at IS NULL`)
// against the `watchpoints` table (filtered by status) to ensure real-time accuracy
// for the dashboard, rather than relying on the potentially stale `watchpoints_count`
// in the `rate_limits` table.
func (h *UsageHandler) GetCurrent(w http.ResponseWriter, r *http.Request)

// GetHistory (GET /v1/usage/history)
// 1. Parses query params (start, end, granularity).
// 2. Calls reporter.GetUsageHistory, passing the granularity param to the repository.
// 3. Returns timeseries data.
// **Granularity Handling**: The handler passes the `granularity` parameter directly
// to the `UsageHistoryRepository.Query` method, which handles the aggregation of
// historical rows using `date_trunc` based on the specified granularity.
// **Data Continuity**: The handler MUST perform a **Union** operation. It queries
// `usage_history` for closed/past days and overlays the current day's partial stats
// from `rate_limits` (API calls) and the live `watchpoints` count. This ensures the
// dashboard shows up-to-the-minute data without waiting for the daily aggregation job.
func (h *UsageHandler) GetHistory(w http.ResponseWriter, r *http.Request)
```

---

## 7. Webhook Handler

Handles asynchronous events from Stripe. This handler is **unauthenticated** (no JWT) but verifies the provider signature.

### 7.1 Structure

```go
type StripeWebhookHandler struct {
    eventHandler WebhookEventHandler
    secret       string // Injected from config
    logger       *slog.Logger
}
```

### 7.2 Methods

```go
// Handle (POST /v1/webhooks/stripe)
// 1. Reads body and "Stripe-Signature" header.
// 2. Verifies signature using h.secret.
// 3. Delegates to h.eventHandler.HandleEvent.
// 4. Returns 200 OK immediately (processing is delegated/async logic).
func (h *StripeWebhookHandler) Handle(w http.ResponseWriter, r *http.Request)
```

---

## 8. Route Registration

The `BillingHandler` acts as the entry point for registering the billing/usage route group to keep `05a-api-core` clean.

```go
// RegisterRoutes mounts all billing and usage endpoints.
func (h *BillingHandler) RegisterRoutes(r chi.Router) {
    // Authenticated Routes (Requires RoleAdmin unless otherwise noted)
    r.Group(func(r chi.Router) {
        // Core middleware applied (Auth, RateLimit) via parent router

        // Billing
        r.Post("/billing/checkout-session", h.CreateCheckoutSession)
        r.Post("/billing/portal-session", h.CreatePortalSession)
        // GET /billing/invoices requires Admin or Owner due to financial sensitivity
        r.With(h.RequireRole(RoleAdmin, RoleOwner)).Get("/billing/invoices", h.GetInvoices)
        r.Get("/billing/subscription", h.GetSubscription)

        // Usage (Composed handler)
        r.Get("/usage", h.usageHandler.GetCurrent)
        r.Get("/usage/history", h.usageHandler.GetHistory)
    })
}

// RegisterRoutes for Webhooks is separate (Public/No Auth Middleware)
func (h *StripeWebhookHandler) RegisterRoutes(r chi.Router) {
    r.Post("/webhooks/stripe", h.Handle)
}
```

---

## 9. Business Logic & Edge Cases

### 9.1 Zombie Billing Prevention
When `SubscriptionStateRepository.UpdateSubscriptionStatus` is called via webhook:
1.  It checks if `Organization.deleted_at` is NOT NULL.
2.  If set, it **must not** update the database.
3.  It logs a `ZC_BILLING_ALERT` (Zombie Content) error, signaling Ops to manually cancel the subscription in Stripe, ensuring we don't bill deleted orgs.

### 9.2 Webhook Concurrency & Ordering
Stripe webhooks may arrive out of order (e.g., `updated` arrives before `created`).
1.  The repository uses **Optimistic Locking**: 
    ```sql
    UPDATE organizations ... 
    WHERE id = $1 
    AND (last_subscription_event_at IS NULL OR last_subscription_event_at < $2)
    ```
2.  Old events are ignored (idempotent no-op).

### 9.3 Downgrade Enforcement
1.  When a plan is downgraded via webhook, limits are updated immediately in the DB.
2.  No immediate action is taken on overage (active WatchPoints > new limit).
3.  The scheduled job `BILL-011` (daily) detects the overage and initiates the grace period/pause logic (see `09-scheduled-jobs.md`).

### 9.4 Event Propagation
On critical billing events (e.g., `invoice.payment_succeeded`), the Webhook Handler does **not** send emails directly to avoid latency.
1.  It updates the database.
2.  It uses `AuditLogger` to emit a `billing.invoice.paid` event.
3.  The **Email Worker** (`08b`) consumes this event to send the receipt asynchronously.

### 9.5 Auto-Resume on Payment Success
Upon receiving `invoice.paid` event:
1.  The Webhook Handler must call `WatchPointRepo.ResumeAllByOrgID(..., PausedReasonBillingDelinquency)` to restore service without user intervention.
2.  Only WatchPoints paused specifically for billing delinquency are resumed; user-paused WatchPoints remain paused.
3.  **Receipt Delivery**: In addition to auto-resume, the handler MUST insert a notification with `event_type='billing_receipt'` and payload containing invoice details. This triggers the Email Worker to send a transactional receipt.

### 9.6 Payment Failure Notification
Upon receiving `invoice.payment_failed` event:
1.  **Explicit Notification**: The handler must insert a `billing_alert` notification into the `notifications` table to trigger the Email Worker.
2.  Do not rely solely on audit logs for user-facing alerts—explicit notification ensures the owner is informed of the payment issue.

---

## 10. Flow Coverage

| Flow ID | Description | Component | Method |
|---|---|---|---|
| `BILL-001` | Create Stripe Customer | `BillingService` | `EnsureCustomer` |
| `BILL-002` | Checkout Session | `BillingHandler` | `CreateCheckoutSession` |
| `BILL-003` | Plan Upgrade | `WebhookHandler` | `Handle` |
| `BILL-004` | Plan Downgrade | `WebhookHandler` | `Handle` |
| `BILL-005` | Payment Failure | `WebhookHandler` | `Handle` |
| `BILL-007` | Stripe Webhook | `WebhookHandler` | `Handle` |
| `INFO-003` | Current Usage | `UsageHandler` | `GetCurrent` |
| `INFO-004` | Usage History | `UsageHandler` | `GetHistory` |
| `INFO-005` | Get Invoices | `BillingHandler` | `GetInvoices` |
| `INFO-006` | Get Subscription | `BillingHandler` | `GetSubscription` |
| `DASH-005` | Dashboard Checkout | `BillingHandler` | `CreateCheckoutSession` |
| `DASH-006` | Dashboard Portal | `BillingHandler` | `CreatePortalSession` |