# 05a - API Core & Router

> **Purpose**: Defines the HTTP server structure, router configuration, middleware stack, authentication logic, and standard request/response handling patterns. This package serves as the entry point for the Lambda function.
> **Package**: `package core`
> **Dependencies**: `01-foundation-types.md`, `02-foundation-db.md`, `03-config.md`
> **Dependents**: All Domain Handler Documents (`05b` - `05f`)

---

## Table of Contents

1. [Overview](#1-overview)
2. [Dependencies](#2-dependencies)
3. [Server Structure & Lifecycle](#3-server-structure--lifecycle)
4. [Router & Versioning](#4-router--versioning)
5. [Middleware Stack](#5-middleware-stack)
6. [Authentication & Authorization](#6-authentication--authorization)
7. [Request & Response Utilities](#7-request--response-utilities)
8. [Validation](#8-validation)
9. [Health Check System](#9-health-check-system)
10. [Context Management](#10-context-management)
11. [Testing Strategy](#11-testing-strategy)
12. [Flow Coverage](#12-flow-coverage)

---

## 1. Overview

The `core` package provides the chassis for the API. It creates a `chi` router compatible with both standard HTTP (for local dev) and AWS Lambda Proxy Integration (via `chiadapter`). It enforces cross-cutting concerns—security, logging, observability, and error handling—before requests reach domain-specific handlers.

### Key Responsibilities
*   **Routing**: versioned mounting of domain handlers.
*   **Safety**: Panic recovery and soft timeouts before Lambda hard limits.
*   **Identity**: Resolving API Keys and Sessions into a unified Actor.
*   **Standardization**: Enforcing strict JSON contracts and error formats.

---

## 2. Dependencies

### 2.1 External Libraries
*   `github.com/go-chi/chi/v5`: Routing and middleware primitives.
*   `github.com/awslabs/aws-lambda-go-api-proxy/chi`: Lambda adapter.
*   `github.com/go-playground/validator/v10`: Struct validation.
*   `log/slog`: Structured logging.

### 2.2 Internal Imports
*   `watchpoint/internal/types`: Domain entities and Error codes.
*   `watchpoint/internal/db`: Repository interfaces.
*   `watchpoint/internal/config`: Configuration structs.

---

## 3. Server Structure & Lifecycle

The `Server` struct encapsulates all dependencies, allowing for easy injection during testing and distinct configuration for different environments.

### 3.1 Server Definition

```go
type Server struct {
    Config          *config.Config
    Repos           db.RepositoryRegistry
    Logger          *slog.Logger
    Validator       *Validator
    Metrics         MetricsCollector
    SecurityService types.SecurityService // Unified security event tracking and IP blocking

    // Internal router
    router    *chi.Mux
}

// MetricsCollector defines the interface for recording API telemetry.
type MetricsCollector interface {
    // RecordRequest records API request metrics including latency and count.
    // Uses metric constants MetricAPILatency and MetricAPIRequestCount from 01-foundation-types.md.
    RecordRequest(method, endpoint, status string, duration time.Duration)
}
```

### 3.2 Constructors

```go
// NewServer initializes dependencies, sets up the router, and mounts routes.
// It performs a "fail-fast" check on critical infrastructure (e.g., DB connection).
// Initializes SecurityService using the SecurityRepository from repos.
func NewServer(
    cfg *config.Config,
    repos db.RepositoryRegistry,
    logger *slog.Logger,
) (*Server, error)

// Handler returns the http.Handler interface for the router.
// Used by http.ListenAndServe (local) and chiadapter.New (Lambda).
func (s *Server) Handler() http.Handler
```

### 3.3 Graceful Shutdown

```go
// Shutdown performs a graceful termination.
// 1. Closes Database connection pools.
// 2. Flushes any buffered logs.
// 3. Cancels any background contexts derived from Server.
func (s *Server) Shutdown(ctx context.Context) error
```

---

## 4. Router & Versioning

To prevent circular dependencies and enforce the Principle of Least Privilege, the Server does **not** pass the global `Config` to handlers. It injects specific interfaces or config subsets.

### 4.1 Mounting Pattern

```go
// MountRoutes defines the top-level routing hierarchy.
func (s *Server) MountRoutes() {
    // Global Middleware Registration (includes MetricsMiddleware)
    s.registerGlobalMiddleware()

    // API Version Groups
    s.router.Route("/v1", s.mountV1)

    // Top-Level Routes
    s.router.Get("/health", s.HandleHealth)
    s.router.Get("/openapi.json", s.ServeOpenAPISpec)
}

// registerGlobalMiddleware applies middleware in order: Recoverer, ContextTimeout,
// RequestID, SecurityHeadersMiddleware, XRayMiddleware, RequestLogger, CORSMiddleware,
// MetricsMiddleware, IPSecurityMiddleware, AuthMiddleware, CSRFMiddleware, RateLimit,
// IdempotencyMiddleware.
//
// **Ordering Rationale**: SecurityHeadersMiddleware executes early (after RequestID)
// to ensure all responses include security headers regardless of downstream processing.
// IPSecurityMiddleware MUST execute BEFORE AuthMiddleware to reject blocked IPs before
// expensive authentication checks (DB queries, bcrypt). This provides proactive abuse
// prevention at the network layer. Authentication MUST execute before RateLimit and
// Idempotency because both depend on the OrganizationID injected into context by
// AuthMiddleware. Rate limiting uses OrganizationID for per-tenant buckets, and
// idempotency keys are scoped to organizations for isolation. CSRFMiddleware executes
// after AuthMiddleware because it depends on the Actor type to determine validation
// requirements.
func (s *Server) registerGlobalMiddleware() {
    s.router.Use(s.Recoverer)
    s.router.Use(ContextTimeoutMiddleware(s.Config.RequestTimeout))
    s.router.Use(middleware.RequestID)
    s.router.Use(s.SecurityHeadersMiddleware)
    s.router.Use(s.XRayMiddleware)
    s.router.Use(RequestLogger(s.Logger, s.Config.RedactedHeaders))
    s.router.Use(NewCORSMiddleware(s.Config.CorsAllowedOrigins))
    s.router.Use(s.MetricsMiddleware)
    s.router.Use(s.IPSecurityMiddleware)
    s.router.Use(s.AuthMiddleware)
    s.router.Use(s.CSRFMiddleware)
    s.router.Use(s.RateLimit)
    s.router.Use(s.IdempotencyMiddleware)
}

// mountV1 registers all v1 endpoints.
func (s *Server) mountV1(r chi.Router) {
    // Handlers are initialized here to ensure they receive
    // only the config/repos they explicitly need.
    
    // Example: WatchPoints
    wpHandler := handlers.NewWatchPointHandler(
        s.Repos.WatchPoints(),
        s.Config.WatchPointSettings, // Pass specific config struct
        s.Logger,
        s.Validator,
    )
    r.Mount("/watchpoints", wpHandler.Routes())

    // Example: Auth
    authHandler := handlers.NewAuthHandler(
        s.Repos.Users(),
        s.Repos.Sessions(),
        s.Config.Auth, // Pass specific config struct
        s.Logger,
    )
    r.Mount("/auth", authHandler.Routes())
}
```

---

## 5. Middleware Stack

Middleware is applied in a specific order to ensure safety, observability, and security.

### 5.1 Execution Order

1.  **Panic Recovery**: Catches crashes, returns 500 JSON.
2.  **Soft Timeout**: Cancels context before Lambda hard timeout.
3.  **Request ID**: Generates or propagates correlation ID.
4.  **Security Headers**: Sets X-Content-Type-Options, X-Frame-Options, X-XSS-Protection.
5.  **X-Ray Tracing**: AWS X-Ray segment generation.
6.  **Logger**: Structured logging (redacted).
7.  **CORS**: Browser security headers.
8.  **Metrics**: Request latency and count recording.
9.  **IP Security**: Blocks IPs with excessive failed attempts (proactive abuse prevention).
10. **Authentication**: Resolves Actor (User/API Key), injects OrganizationID into context.
11. **CSRF**: Validates CSRF tokens for session-based requests.
12. **Rate Limiting**: Per-organization token buckets (requires OrganizationID from Auth).
13. **Idempotency**: Locks for POST requests (requires OrganizationID from Auth).

### 5.2 Middleware Definitions

```go
// Recoverer catches panics, logs the stack trace internally, and writes
// a standardized types.APIErrorResponse (500) to the client.
func (s *Server) Recoverer(next http.Handler) http.Handler

// ContextTimeoutMiddleware sets a deadline on the request context.
// duration: (Lambda Timeout - 1 second).
// Returns 504 Gateway Timeout if exceeded.
func ContextTimeoutMiddleware(duration time.Duration) func(http.Handler) http.Handler

// XRayMiddleware creates segments and injects trace_id into context/logger.
func (s *Server) XRayMiddleware(next http.Handler) http.Handler

// RequestLogger logs request metadata (method, path, status, duration).
// It explicitly redacts headers defined in Config (Authorization, etc.).
func RequestLogger(logger *slog.Logger, redactedHeaders []string) func(http.Handler) http.Handler

// NewCORSMiddleware configures CORS based on environment variables.
func NewCORSMiddleware(allowedOrigins []string) func(http.Handler) http.Handler

// RateLimit uses a backing store to enforce plan limits.
//
// **Direct Database Strategy**: For v1, the middleware performs atomic updates on the
// `rate_limits` table (`UPDATE ... SET api_calls_count = api_calls_count + 1`) directly.
// This avoids the operational complexity of Redis while utilizing PostgreSQL's row-level
// locking capabilities.
//
// **Source-Based Attribution (VERT-001)**: The middleware uses `Actor.Source` to identify
// the specific row in the `rate_limits` table to increment. The UPDATE targets the row
// matching `(organization_id, source)`. If no row exists for the source, the middleware
// performs an UPSERT to create one with the organization's default period settings.
//
// **Aggregate Limit Enforcement**: While counters are tracked per-source, limit enforcement
// (checking against Plan Max) is done against the SUM of all source rows for that
// organization: `SELECT SUM(api_calls_count) FROM rate_limits WHERE organization_id = $1`.
// This ensures vertical apps share the organization's quota.
//
// **80% Threshold Check (Overage Warning Logic)**: The middleware monitors the
// aggregated `api_calls_count` against `plan_limits`. If usage > 80% AND `warning_sent_at`
// is NULL (or from a previous cycle), it atomically updates `warning_sent_at` and inserts
// a `billing_warning` notification into the `notifications` table to trigger the Email
// Worker. This ensures users are alerted before hitting hard limits.
func (s *Server) RateLimit(next http.Handler) http.Handler

// CSRFMiddleware enforces CSRF protection for session-based authentication.
// This middleware checks the Actor.Type from context (injected by AuthMiddleware).
//
// **Validation Logic**:
// - If `ActorType == ActorTypeAPIKey`: Skip validation (API keys are bearer tokens,
//   not vulnerable to CSRF attacks). Allow request to proceed.
// - If `ActorType == ActorTypeUser` (Session): Enforce that the `X-CSRF-Token` header
//   matches the token stored in the session. The CSRF token is issued during login
//   and stored in the Session record. Reject with 403 Forbidden if mismatch.
// - If `ActorType == ActorTypeSystem`: Skip validation (internal system calls).
//
// **Safe Methods**: GET, HEAD, OPTIONS requests are exempt from CSRF validation
// as they should not cause state changes.
func (s *Server) CSRFMiddleware(next http.Handler) http.Handler

// IdempotencyMiddleware ensures POST requests with "Idempotency-Key" header
// are processed exactly once. Uses db.IdempotencyRepository.
//
// **Response Capturer Pattern**: The middleware wraps the `http.ResponseWriter` with
// a custom capturer that buffers the response status code, headers, and body during
// handler execution. Upon successful completion, the captured response is persisted
// to the `idempotency_keys.response_body` JSONB column along with `response_status`
// and `response_headers`. For subsequent requests with the same idempotency key,
// the middleware replays the stored response directly without re-executing the handler.
//
// **Storage Schema** (persisted to idempotency_keys table):
// - response_status: HTTP status code (e.g., 201)
// - response_headers: JSONB map of header key-value pairs
// - response_body: JSONB containing the complete response body
func (s *Server) IdempotencyMiddleware(next http.Handler) http.Handler

// MetricsMiddleware records request latency and count metrics for observability.
// Wraps handlers to capture start time, call the next handler, capture response
// status code, and call s.Metrics.RecordRequest.
// Uses metric constants MetricAPILatency and MetricAPIRequestCount from 01-foundation-types.md.
func (s *Server) MetricsMiddleware(next http.Handler) http.Handler

// SecurityHeadersMiddleware sets standard security response headers on all API responses.
// Executes early in the middleware chain (after RequestID) to ensure headers are present
// regardless of downstream processing or errors.
//
// **Headers Set**:
// - `X-Content-Type-Options: nosniff` - Prevents MIME type sniffing attacks
// - `X-Frame-Options: DENY` - Prevents clickjacking by disallowing iframe embedding
// - `X-XSS-Protection: 1; mode=block` - Enables browser XSS filtering (legacy browsers)
//
// **Implementation**:
// ```go
// func (s *Server) SecurityHeadersMiddleware(next http.Handler) http.Handler {
//     return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//         w.Header().Set("X-Content-Type-Options", "nosniff")
//         w.Header().Set("X-Frame-Options", "DENY")
//         w.Header().Set("X-XSS-Protection", "1; mode=block")
//         next.ServeHTTP(w, r)
//     })
// }
// ```
func (s *Server) SecurityHeadersMiddleware(next http.Handler) http.Handler

// IPSecurityMiddleware provides proactive IP-based blocking before authentication.
// Executes BEFORE AuthMiddleware to reject known-bad IPs without expensive auth checks.
//
// **Logic**:
// 1. Extract client IP from X-Forwarded-For header (first entry) or RemoteAddr.
// 2. Call SecurityService.IsIPBlocked(ctx, ip).
// 3. If blocked: Return 403 Forbidden with JSON error body:
//    {"error": {"code": "ip_blocked", "message": "Access denied"}}
// 4. If not blocked: Call next handler.
//
// **Rationale**: Prevents resource exhaustion from credential stuffing attacks
// by rejecting IPs with excessive failures (e.g., >100 failures in 15 min)
// before performing database queries or bcrypt comparisons.
func (s *Server) IPSecurityMiddleware(next http.Handler) http.Handler
```

### 5.3 Rate Limiting Interface

```go
// RateLimitStore abstracts Redis (Prod) vs In-Memory (Dev/Test).
type RateLimitStore interface {
    IncrementAndCheck(ctx context.Context, key string, limit int, window time.Duration) (RateLimitResult, error)
}

type RateLimitResult struct {
    Allowed   bool
    Remaining int
    ResetAt   time.Time
}
```

---

## 6. Authentication & Authorization

### 6.1 Unification Strategy

The `Authenticator` interface decouples the HTTP layer from specific auth mechanisms (DB lookups), allowing for easy mocking.

```go
type Authenticator interface {
    // ResolveToken inspects prefix ("sk_", "sess_") and returns the Actor.
    //
    // **Resolution Strategy**:
    // 1. Query the key record with WHERE clause: `revoked_at IS NULL`. This ensures
    //    revoked keys are immediately invalid regardless of expiration timestamp.
    // 2. If the record is found (not revoked), the *application logic* (not SQL)
    //    compares `expires_at` vs `NOW()` to determine expiration status.
    // 3. **Source Propagation (VERT-001)**: For API Key authentication, fetch the
    //    `source` column from the `api_keys` table and populate `Actor.Source`.
    //    If the key has no source (NULL or empty), default to "default".
    //
    // **Distinct Error Codes**:
    // - Return `ErrAuthTokenInvalid` if the token is malformed, not found, or revoked.
    // - Return `ErrAuthTokenExpired` if the token exists and is not revoked, but
    //   `expires_at < NOW()`. This distinction allows clients to differentiate
    //   between invalid credentials and expired credentials for UX purposes
    //   (e.g., prompting token refresh vs re-authentication).
    ResolveToken(ctx context.Context, token string) (*types.Actor, error)
}
```

### 6.2 Auth Middleware

```go
// AuthMiddleware wraps handlers requiring authentication.
// 1. Extracts Bearer token.
// 2. Calls Authenticator.ResolveToken.
// 3. Injects Actor into Context.
// 4. Returns 401 Unauthorized on failure.
//
// **Live Role Check**: The middleware must execute a `JOIN` (or efficient lookup)
// to retrieve the user's **current** Role and OrganizationID from the database
// on every request. Do not rely solely on stale session data. This ensures role
// demotions or bans take effect immediately.
//
// **Sliding Window Update Strategy (Session Extension)**:
// To prevent database write thrashing on every request, the middleware MUST implement
// a sliding window for session activity updates:
// 1. After resolving the session, check the `LastActivityAt` timestamp.
// 2. Only execute the database update if `now() - LastActivityAt > 1 hour`.
// 3. The SQL update statement MUST include the condition:
//    `AND last_activity_at < NOW() - INTERVAL '1 hour'`
//    This uses database-level filtering to handle race conditions and ensure
//    the update only occurs when truly needed.
// 4. **Cookie Synchronization**: If the session is extended (database write occurs),
//    the middleware MUST re-issue the `Set-Cookie` header with the new expiration
//    timestamp to keep the browser cookie state synchronized with the server session.
func (s *Server) AuthMiddleware(next http.Handler) http.Handler
```

### 6.3 Authorization Helpers

```go
// RequireRole checks if the Actor has the specified Role (Owner, Admin, Member).
// Returns 403 Forbidden if check fails.
func (s *Server) RequireRole(role types.UserRole) func(http.Handler) http.Handler

// RequireScope checks if API Key Actors possess the specific scope.
// Users implicitly have scopes based on their Role.
func (s *Server) RequireScope(scope string) func(http.Handler) http.Handler
```

---

## 7. Request & Response Utilities

Enforces the JSON contract defined in the Design Addendum.

### 7.1 Response Structures

```go
// APIResponse is the standard envelope for all successful API responses.
// Uses types.ResponseMeta to convey non-blocking warnings (e.g., deprecation notices).
type APIResponse struct {
    Data interface{}          `json:"data,omitempty"`
    Meta *types.ResponseMeta  `json:"meta,omitempty"` // Warnings, pagination, etc.
}

type APIErrorResponse struct {
    Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
    Code      string         `json:"code"`
    Message   string         `json:"message"`
    Details   map[string]any `json:"details,omitempty"`
    RequestID string         `json:"request_id"`
}
```

### 7.2 Helper Functions

```go
// JSON writes status and data to the response writer.
func JSON(w http.ResponseWriter, r *http.Request, status int, data interface{})

// Error unwraps the error chain.
// - If types.AppError: maps Code to HTTP Status.
// - If generic error: maps to "internal_server_error" (500).
func Error(w http.ResponseWriter, r *http.Request, err error)

// DecodeJSON reads body into dst.
// Enforces: MaxBytes (1MB), DisallowUnknownFields.
// Returns "validation_invalid_json" (400) on syntax errors.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) error
```

---

## 8. Validation

Wraps `go-playground/validator` to register domain-specific rules.

### 8.1 Validator Structure

```go
type Validator struct {
    validate *validator.Validate
    logger   *slog.Logger
}

// ValidationResult contains both blocking errors and non-blocking warnings.
// Allows the API to accept valid but deprecated inputs while signaling migration needs.
type ValidationResult struct {
    // Errors are blocking validation failures that prevent request processing.
    Errors []ValidationError `json:"errors,omitempty"`

    // Warnings are non-blocking advisory messages (e.g., deprecated webhook URLs).
    // The request proceeds but warnings should be surfaced to the client.
    Warnings []string `json:"warnings,omitempty"`
}

type ValidationError struct {
    Field   string `json:"field"`
    Code    string `json:"code"`
    Message string `json:"message"`
}

// IsValid returns true if there are no blocking errors.
func (v ValidationResult) IsValid() bool {
    return len(v.Errors) == 0
}
```

### 8.2 Methods & Custom Rules

```go
// NewValidator creates the validator and registers custom tags.
func NewValidator(logger *slog.Logger) *Validator

// ValidateStruct returns a structured types.AppError on failure.
// DEPRECATED: Use ValidateStructWithWarnings for new code paths.
func (v *Validator) ValidateStruct(i interface{}) error

// ValidateStructWithWarnings performs validation and returns both errors and warnings.
// This method supports the new ValidationResult pattern that separates blocking errors
// from non-blocking warnings (e.g., deprecated webhook platforms).
//
// Usage:
//   result := v.ValidateStructWithWarnings(req)
//   if !result.IsValid() {
//       return types.NewValidationError(result.Errors)
//   }
//   // Proceed with request, append result.Warnings to response meta
func (v *Validator) ValidateStructWithWarnings(i interface{}) ValidationResult

// Custom Tags:
// "is_conus": Validates Lat/Lon is within US Bounds.
// "ssrf_url": Validates Webhook URL against private IP blocklists using SSRF protection.
//   - Invokes types.ValidateWebhookURL (from 01-foundation-types.md) for HTTPS scheme check.
//   - Performs DNS resolution within a context with a **strict 500ms timeout**.
//   - Checks resolved IPs against SSRFBlockedCIDRs (from 01-foundation-types.md).
//   - **Fail Closed**: If DNS resolution times out, the validation MUST fail with
//     ErrCodeValidationInvalidWebhook. Do not allow URLs that cannot be verified.
// "is_timezone": Validates IANA timezone string.
```

---

## 9. Health Check System

Performs deep probing of subsystems rather than a simple "ping".

### 9.1 Interfaces

```go
type HealthProbe interface {
    Name() string
    Check(ctx context.Context) error
}
```

### 9.2 Handler Logic

```go
// HandleHealth executes all probes concurrently with a short timeout.
// Returns 200 if all healthy, 503 if any critical subsystem fails.
// Returns JSON detailing status of Database, S3, and SQS.
//
// **Concurrent Execution**: The handler spawns goroutines to probe Database, S3, and
// Queue subsystems in parallel using an errgroup. Each probe executes independently
// to minimize total health check latency.
//
// **Global Timeout**: A context with a **2-second deadline** is derived from the request
// context. If any component fails to respond within this timeout, the health check
// returns 503 Service Unavailable with details indicating which components timed out.
func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request)
```

---

## 10. Context Management

Provides type-safe accessors for context keys (which are unexported types to prevent collisions).

```go
// WithActor injects the authenticated Actor.
func WithActor(ctx context.Context, actor types.Actor) context.Context

// GetActor retrieves the Actor.
func GetActor(ctx context.Context) (types.Actor, bool)

// GetOrgID retrieves the Organization ID (derived from Actor).
func GetOrgID(ctx context.Context) (string, bool)

// GetTraceID retrieves the X-Ray/Request ID for correlation.
func GetTraceID(ctx context.Context) string
```

---

## 11. Testing Strategy

Provides helpers to spin up isolated test instances of the Server without external dependencies.

### 11.1 Test Helpers

```go
// NewTestServer initializes a Server with:
// - InMemory repositories
// - No-op logger
// - Mock Authenticator (bypasses DB/Bcrypt)
// - InMemory RateLimiter
func NewTestServer() (*Server, *mocks.MockRepositoryRegistry)
```

### 11.2 Mocks needed
*   `MockAuthenticator`: Allows injecting a predefined Actor for a given token.
*   `MockRateLimitStore`: Allows testing rate limit headers without Redis.

---

## 12. Flow Coverage

| Flow ID | Flow Name | Implementation |
|---|---|---|
| `API-001` | Request Auth | `AuthMiddleware` + `Authenticator` |
| `API-002` | Webhook Validation | `Validator` ("ssrf_safe_url") |
| `API-003` | Health Check | `HandleHealth` + `HealthProbe` |
| `API-004` | Idempotency | `IdempotencyMiddleware` |
| `SEC-005` | Input Sanitization | `DecodeJSON` (Strict decoding) |
| `INFO-008` | CSRF Protection | `CSRFMiddleware` |
