# 10 - External Integrations

> **Purpose**: Defines the abstraction layer for third-party services (Stripe, SendGrid, OAuth). This package ensures the core domain logic remains decoupled from specific vendor implementations, handles network resilience (retries, circuit breaking), and manages vendor-specific security patterns (webhook signature verification).
> **Package**: `package external`
> **Dependencies**: `01-foundation-types.md`, `03-config.md`
> **Dependents**: `05e-api-billing`, `05f-api-auth`, `08b-email-worker`

---

## Table of Contents

1. [Overview](#1-overview)
2. [Type Migrations](#2-type-migrations)
3. [Common Infrastructure](#3-common-infrastructure)
4. [Billing Integration (Stripe)](#4-billing-integration-stripe)
5. [Email Integration (SendGrid)](#5-email-integration-sendgrid)
6. [Identity Integration (OAuth)](#6-identity-integration-oauth)
7. [Client Registry & Configuration](#7-client-registry--configuration)
8. [Testing & Mocks](#8-testing--mocks)
9. [Flow Coverage](#9-flow-coverage)

---

## 1. Overview

This package acts as the **Anti-Corruption Layer** between the WatchPoint domain and external vendors.

### Key Responsibilities
*   **Translation**: converting vendor-specific objects (e.g., `stripe.Session`) into domain entities (`types.SubscriptionDetails`).
*   **Resilience**: Enforcing timeouts, retries, and circuit breaking on all outbound HTTP calls.
*   **Security**: Verifying incoming webhook signatures using vendor-specific cryptography (HMAC, ECDSA).
*   **Testability**: Providing mock implementations so the API and Workers can run in isolation.

---

## 2. Type Dependencies

*All shared types are consolidated in `01-foundation-types.md` (Section 10).*

This module uses the following types from `package types`:

1.  **Billing Types** (Section 10.2):
    *   `types.Invoice`
    *   `types.SubscriptionDetails`
    *   `types.PaymentMethodInfo`
    *   `types.SubscriptionStatus` (Enum)
    *   `types.ListInvoicesParams`

2.  **Identity Types** (Section 10.3):
    *   `types.OAuthProfile`

3.  **Email Types** (Section 10.4):
    *   `types.SendInput`
    *   `types.SenderIdentity`

---

## 3. Common Infrastructure

### 3.1 Base HTTP Client

All provider clients embed a private `BaseClient` to enforce consistent resilience patterns.

```go
type BaseClient struct {
    client      *http.Client
    breaker     *gobreaker.CircuitBreaker
    retryPolicy RetryPolicy
    userAgent   string
}

type RetryPolicy struct {
    MaxRetries int
    MinWait    time.Duration
    MaxWait    time.Duration
}

// Do executes the request with:
// 1. Trace ID injection (X-B3-TraceId)
// 2. User-Agent injection
// 3. Circuit breaker wrapping
// 4. Retry on 429/5xx (respecting Retry-After headers)
// 5. Metrics emission (ExternalCallLatency, ExternalCallErrors)
func (c *BaseClient) Do(req *http.Request) (*http.Response, error)
```

### 3.2 Error Mapping

Vendor-specific errors are translated into `types.AppError` codes to keep the domain layer agnostic.

| Vendor Error | WatchPoint Error |
|---|---|
| Stripe `card_declined` | `types.ErrPaymentDeclined` |
| SendGrid `403 Forbidden` | `types.ErrEmailBlocked` |
| HTTP 429 | `types.ErrUpstreamRateLimited` |
| HTTP 500+ | `types.ErrUpstreamUnavailable` |

---

## 4. Billing Integration (Stripe)

### 4.1 Interface Definition

Matches `BillingService` defined in `05e-api-billing`.

```go
type BillingService interface {
    EnsureCustomer(ctx context.Context, orgID string, email string) (string, error)
    
    // CreateCheckoutSession includes orgID to set client_reference_id for webhook correlation
    CreateCheckoutSession(ctx context.Context, orgID string, plan types.PlanTier, urls types.RedirectURLs) (checkoutURL string, sessionID string, err error)
    
    CreatePortalSession(ctx context.Context, orgID string, returnURL string) (portalURL string, err error)
    
    GetInvoices(ctx context.Context, orgID string, params types.ListInvoicesParams) ([]*types.Invoice, types.PageInfo, error)
    
    GetSubscription(ctx context.Context, orgID string) (*types.SubscriptionDetails, error)
}
```

### 4.2 Webhook Verification

Exposed for use by `05e-api-billing`'s `StripeWebhookHandler`.

```go
// WebhookVerifier abstracts signature checking
type WebhookVerifier interface {
    Verify(payload []byte, header string, secret string) error
}

type StripeVerifier struct{}

// Verify uses stripe-go/webhook to check timestamp and v1 signature
func (v *StripeVerifier) Verify(payload []byte, header string, secret string) error
```

### 4.3 Event Constants

Exported to prevent magic strings in handlers.

```go
const (
    EventStripeCheckoutCompleted = "checkout.session.completed"
    EventStripeInvoicePaid       = "invoice.paid"
    EventStripePaymentFailed     = "invoice.payment_failed"
    EventStripeSubUpdated        = "customer.subscription.updated"
    EventStripeSubDeleted        = "customer.subscription.deleted"
)
```

### 4.4 Implementation Details (`StripeClient`)

*   **Config**: Initialized with `StripeSecretKey`.
*   **Timeout**: 20 seconds.
*   **Metadata**: `EnsureCustomer` searches by `metadata["org_id"]` to ensure 1:1 mapping.
*   **Concurrency**: To prevent duplicate customers during race conditions (e.g., concurrent signup requests), the `EnsureCustomer` implementation MUST first query the Stripe Search API for a customer with `metadata['org_id'] == inputID`. If a matching customer exists, use the existing Stripe customer ID instead of creating a new one. This ensures idempotency even under concurrent access.
*   **Pagination Mapping** (`GetInvoices`):
    *   The implementation MUST map the domain `types.ListInvoicesParams.Cursor` to Stripe's `starting_after` parameter when making API calls.
    *   The response `PageInfo.NextCursor` is derived from the ID of the last item in the Stripe result list.
    *   If the Stripe response contains more items (`has_more: true`), set `PageInfo.HasMore = true` and populate `NextCursor` with the last invoice ID.

---

## 5. Email Integration (SendGrid)

### 5.1 Interface Definition

Matches `EmailProvider` defined in `08b-email-worker`.

```go
type EmailProvider interface {
    // Send maps domain inputs to provider-specific template calls
    Send(ctx context.Context, input types.SendInput) (providerMsgID string, err error)
}
```

### 5.2 Webhook Verification (ECDSA)

Exposed for use by `08b`'s bounce handler.

```go
type EmailVerifier interface {
    Verify(payload []byte, signature string, timestamp string, publicKey string) (bool, error)
}

type SendGridVerifier struct{}

// Verify checks the ECDSA signature from X-Twilio-Email-Event-Webhook-Signature
func (v *SendGridVerifier) Verify(payload []byte, signature string, timestamp string, publicKey string) (bool, error)
```

### 5.3 Event Constants

```go
const (
    EventSendGridBounce    = "bounce"
    EventSendGridComplaint = "spamreport"
)
```

### 5.4 Implementation Details (`SendGridClient`)

*   **Config**: Initialized with `SendGridAPIKey`.
*   **Timeout**: 10 seconds.
*   **Template Logic**: Maps `types.SendInput.TemplateData` to SendGrid dynamic template variables.

---

## 6. Identity Integration (OAuth)

### 6.1 Interface Definition

Matches requirements for `05f-api-auth`.

```go
type OAuthProvider interface {
    Name() string
    GetLoginURL(state string) string
    
    // Exchange trades code for profile. Does NOT return access/refresh tokens (scope is auth only).
    Exchange(ctx context.Context, code string) (*types.OAuthProfile, error)
}

type OAuthManager interface {
    GetProvider(name string) (OAuthProvider, error)
}
```

### 6.2 Provider Implementations

#### GoogleProvider
*   **Scopes**: `.../auth/userinfo.email`, `.../auth/userinfo.profile`
*   **Endpoint**: `https://www.googleapis.com/oauth2/v2/userinfo`
*   **Normalization**: Maps `verified_email` to `types.OAuthProfile.EmailVerified`.

#### GithubProvider
*   **Scopes**: `read:user`, `user:email`
*   **Endpoints**: `https://api.github.com/user` + `https://api.github.com/user/emails`
*   **Normalization**: Iterates email list to find `{primary: true, verified: true}`.

---

## 7. Client Registry & Configuration

Central factory that instantiates all clients based on configuration.

```go
type ClientRegistry struct {
    Billing BillingService
    Email   EmailProvider
    OAuth   OAuthManager
    
    // Verifiers
    StripeVerifier WebhookVerifier
    EmailVerifier  EmailVerifier
}

// NewClientRegistry initializes clients.
// If config.IsTestMode is true, returns Mock implementations.
// Sets strict timeouts per provider.
func NewClientRegistry(cfg *config.Config, logger *slog.Logger) (*ClientRegistry, error)
```

---

## 8. Testing & Mocks

Exported mocks allow other packages to test without external dependencies.

```go
type MockBillingService struct {
    CreateCheckoutSessionFunc func(ctx context.Context, orgID string, plan types.PlanTier, urls types.RedirectURLs) (string, string, error)
    // ... other methods
}

type MockEmailProvider struct {
    SendFunc func(ctx context.Context, input types.SendInput) (string, error)
}

type MockOAuthProvider struct {
    ExchangeFunc func(ctx context.Context, code string) (*types.OAuthProfile, error)
}
```

---

## 9. Flow Coverage

| Flow ID | Description | Component | Method |
|---|---|---|---|
| `BILL-001` | Create Customer | `StripeClient` | `EnsureCustomer` |
| `BILL-002` | Checkout Session | `StripeClient` | `CreateCheckoutSession` |
| `BILL-007` | Stripe Webhook | `StripeVerifier` | `Verify` |
| `NOTIF-001` | Email Send | `SendGridClient` | `Send` |
| `NOTIF-006` | Bounce Webhook | `SendGridVerifier` | `Verify` |
| `DASH-002` | OAuth Callback | `Google/GithubProvider` | `Exchange` |
| `TEST-004` | Mock Mode | `ClientRegistry` | `NewClientRegistry` |