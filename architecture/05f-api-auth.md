# 05f - API Authentication & Authorization

> **Purpose**: Defines the HTTP handlers, service interfaces, data models, and security logic for user authentication, session management, OAuth integration, and credential lifecycle (password reset, invites).
> **Package**: `package handlers`
> **Dependencies**: `05a-api-core.md`, `01-foundation-types.md`, `02-foundation-db.md`, `03-config.md`, `05d-api-organization.md` (for Invite concepts)

---

## Table of Contents

1. [Overview](#1-overview)
2. [Database Extensions](#2-database-extensions)
3. [Domain Models & DTOs](#3-domain-models--dtos)
4. [Core Interfaces](#4-core-interfaces)
5. [Handler Definition](#5-handler-definition)
6. [Route Registration](#6-route-registration)
7. [Service Logic & Security](#7-service-logic--security)
8. [OAuth Implementation](#8-oauth-implementation)
9. [Flow Coverage](#9-flow-coverage)

---

## 1. Overview

The Authentication module serves as the gatekeeper for the platform. It manages identity verification via Password or OAuth (Google/GitHub), maintains state via secure Sessions (for Dashboard access), and handles credential lifecycle events.

### Key Responsibilities
*   **Identity Verification**: Authenticating users against stored hashes or external providers.
*   **Session Management**: Issuing and validating secure, HTTP-only cookies with CSRF protection.
*   **Security Enforcement**: Implementing brute force protection (lockouts), strict rate limiting, and input canonicalization.
*   **Credential Lifecycle**: Managing password resets and invite acceptance.

---

## 2. Database Extensions

*Schema defined in `02-foundation-db.md`.*

The auth-related tables (`password_resets`) and columns (`auth_provider`, `auth_provider_id`, `invite_token_hash`, `invite_expires_at`) are consolidated in the Foundation Database document (Section 6.5). This module uses those definitions.

---

## 3. Domain Models & DTOs

These structures define the contract for API requests and responses. Validation tags enforce security rules at the gateway.

### 3.1 Authentication

```go
// POST /auth/login
type LoginRequest struct {
    Email    string `json:"email" validate:"required,email"`
    Password string `json:"password" validate:"required"`
}

// Unified response for Login, OAuth Callback, and Accept Invite
// Session ID is returned ONLY via HttpOnly/Secure Set-Cookie header
type AuthResponse struct {
    CSRFToken string      `json:"csrf_token"`           // For X-CSRF-Token header
    User      *types.User `json:"user"`
    ExpiresAt time.Time   `json:"expires_at"`
}
```

### 3.2 Credential Lifecycle

```go
// POST /auth/forgot-password
type ForgotPasswordRequest struct {
    Email string `json:"email" validate:"required,email"`
}

// POST /auth/reset-password
type ResetPasswordRequest struct {
    Token       string `json:"token" validate:"required"`
    NewPassword string `json:"new_password" validate:"required,min=8,max=72"`
}

// POST /auth/accept-invite
type AcceptInviteRequest struct {
    Token       string `json:"token" validate:"required"`
    Name        string `json:"name" validate:"required,max=100"`
    Password    string `json:"password" validate:"required,min=8,max=72"`
}
```

### 3.3 Internal Types

*`OAuthProfile` is consolidated in `01-foundation-types.md` (Section 10.3).*

This module uses `types.OAuthProfile` for normalized OAuth user data.

---

## 4. Core Interfaces

The Auth module relies on dependency injection for data access, cryptography, and external integrations to ensure testability.

### 4.1 Data Access (Repositories)

These interfaces extend or complement the foundation repositories.

```go
// UserRepository extension for Auth
type UserRepository interface {
    // GetByEmail retrieves a user.
    // Returns ErrNotFoundUser if missing.
    // Returns ErrOrgDeleted if the associated organization is soft-deleted.
    GetByEmail(ctx context.Context, email string) (*types.User, error)

    // UpdatePassword updates the hash and clears reset tokens.
    UpdatePassword(ctx context.Context, userID string, newHash string) error

    // CreateWithProvider creates a user via OAuth (password_hash is nil).
    CreateWithProvider(ctx context.Context, user *types.User) error
}

// APIKeyRepository extension for Auth (Source Propagation - VERT-001)
type APIKeyRepository interface {
    // GetByHash retrieves an API key by its bcrypt hash.
    // Returns the full APIKey struct including the `source` field for Actor population.
    // Returns ErrNotFoundAPIKey if missing or revoked.
    GetByHash(ctx context.Context, keyHash string) (*types.APIKey, error)
}

// PasswordResetRepository manages reset tokens.
type PasswordResetRepository interface {
    Create(ctx context.Context, userID string, tokenHash string, expiresAt time.Time) error
    GetActiveByUserID(ctx context.Context, userID string) (*PasswordResetToken, error)
    MarkUsed(ctx context.Context, id int64) error
}

// Internal model for PasswordResetRepository
type PasswordResetToken struct {
    ID        int64
    UserID    string
    TokenHash string
    ExpiresAt time.Time
    Used      bool
}

// NOTE: LoginAttemptRepository has been replaced by types.SecurityService.
// Security event tracking is now centralized in the SecurityService interface
// (defined in 01-foundation-types.md Section 5.3) which operates on the unified
// 'security_events' table (defined in 02-foundation-db.md Section 5.2).
```

### 4.2 Services & Logic

```go
// AuthService orchestrates credential validation and lifecycle flows.
//
// **Dependencies**: AuthService requires the following repositories/services:
// - UserRepository: For user lookup and creation
// - SessionRepository: For session management
// - OrganizationRepository: For JIT organization provisioning (DASH-002)
// - PlanRegistry: For default plan limits during org creation
// - PasswordResetRepository: For password reset token management
// - types.SecurityService: For unified security event tracking and brute force protection
type AuthService interface {
    // Login verifies credentials and checks lockout. Returns User or error.
    //
    // **Security Event Recording**: On both success and failure, calls
    // SecurityService.RecordAttempt(ctx, "login", email, ip, success, reason).
    // This enables unified tracking for IP-based blocking and identifier lockouts.
    //
    // **Lazy Session Cleanup**: During the login transaction, the service MUST execute
    // a delete for expired sessions (`DELETE FROM sessions WHERE user_id = $1 AND expires_at < NOW()`)
    // to prevent session table bloat. This cleanup runs opportunistically within the
    // existing transaction to minimize overhead.
    Login(ctx context.Context, email, password, ip string) (*types.User, error)

    // Lifecycle methods
    RequestPasswordReset(ctx context.Context, email string) error
    CompletePasswordReset(ctx context.Context, token, newPassword string) error

    // AcceptInvite validates the invite token, sets user password, and activates the account.
    // CRITICAL: Must execute User Status Update (invited -> active) and Session Creation
    // within a single database transaction. Rollback if session creation fails to keep
    // the invite token valid for retry.
    AcceptInvite(ctx context.Context, token, name, password string) (*types.User, error)

    // LoginWithOAuth authenticates via OAuth provider.
    // Must reject with Conflict if user exists with different auth_provider.
    //
    // **JIT Organization Provisioning (DASH-002)**: If a new user is created via OAuth
    // (no existing account found), the service MUST execute the following within a
    // single transaction:
    // 1. Create a new Organization with Default/Free Tier plan via OrganizationRepository.
    //    Use PlanRegistry.GetLimits(PlanFree) for default limits.
    // 2. Create the User linked to that Organization with role='owner'.
    // 3. Link the OAuth provider (set auth_provider and auth_provider_id).
    // 4. Create the initial session.
    // Rollback entire transaction if any step fails.
    //
    // **Lazy Session Cleanup**: Same as Login - delete expired sessions for the user
    // during the login transaction to prevent table bloat.
    LoginWithOAuth(ctx context.Context, profile OAuthProfile) (*types.User, error)
}

// SessionService manages session tokens, cookies, and CSRF.
type SessionService interface {
    CreateSession(ctx context.Context, userID, orgID, ip, userAgent string) (*types.Session, string, error)
    ValidateSession(ctx context.Context, sessionID string) (*types.Session, error)
    ValidateCSRF(session *types.Session, token string) error
    InvalidateSession(ctx context.Context, sessionID string) error
    InvalidateAllUserSessions(ctx context.Context, userID string) error
}

// BruteForceProtector encapsulates lockout logic.
type BruteForceProtector interface {
    CheckLoginAllowed(ctx context.Context, email, ip string) (allowed bool, err error)
    RecordAttempt(ctx context.Context, email, ip string, success bool) error
}

// TokenGenerator abstracts entropy sources for testability.
type TokenGenerator interface {
    GenerateSessionID() (string, error)
    GenerateCSRF() (string, error)
    GenerateSecureToken() (string, error) // For Resets/Invites
    GenerateOAuthState() (string, error)  // Signed state token
}
```

---

## 5. Handler Definition

The `AuthHandler` maps HTTP requests to the Service layer and handles cookie/header management.

```go
type AuthHandler struct {
    authService    AuthService
    sessionService SessionService
    oauthManager   OAuthManager
    cookieConfig   CookieConfig
    logger         *slog.Logger
    validator      *core.Validator
}

func NewAuthHandler(
    auth AuthService,
    sess SessionService,
    oauth OAuthManager,
    cfg CookieConfig,
    l *slog.Logger,
    v *core.Validator,
) *AuthHandler

// CookieConfig defines security attributes for session cookies.
type CookieConfig struct {
    Name     string `envconfig:"SESSION_COOKIE_NAME" default:"session_id"`
    Domain   string `envconfig:"SESSION_COOKIE_DOMAIN"`
    Secure   bool   `envconfig:"SESSION_COOKIE_SECURE" default:"true"`
    HttpOnly bool   `envconfig:"SESSION_COOKIE_HTTP_ONLY" default:"true"`
    SameSite string `envconfig:"SESSION_COOKIE_SAME_SITE" default:"Lax"`
    MaxAge   int    `envconfig:"SESSION_COOKIE_MAX_AGE" default:"604800"`
}
```

---

## 6. Route Registration

The Auth API exposes both public endpoints (strict IP rate limits) and protected endpoints (standard session checks).

```go
func (h *AuthHandler) RegisterRoutes(r chi.Router) {
    // Public Routes - Apply Strict Rate Limiting (e.g., 5-10 req/min per IP)
    r.Group(func(r chi.Router) {
        // Credential Flows
        r.Post("/auth/login", h.HandleLogin)
        r.Post("/auth/forgot-password", h.HandleForgotPassword)
        r.Post("/auth/reset-password", h.HandleResetPassword)
        r.Post("/auth/accept-invite", h.HandleAcceptInvite)
        
        // OAuth Flows
        r.Get("/auth/oauth/{provider}/login", h.HandleOAuthLogin)
        r.Get("/auth/oauth/{provider}/callback", h.HandleOAuthCallback)
    })

    // Protected Routes - Requires Valid Session
    r.Group(func(r chi.Router) {
        r.Use(core.AuthMiddleware) 
        r.Post("/auth/logout", h.HandleLogout)
    })
}
```

---

## 7. Service Logic & Security

### 7.1 Security Standards

*   **Password Hashing**: `bcrypt` with cost **12**.
*   **Session Signing**: `HMAC-SHA256`.
*   **Rate Limiting**: Distinct bucket `auth_login_ip:{ip}` used for public auth endpoints. **Password Reset**: Must utilize `RateLimiter` with an email-specific key (`email_reset_limit:{email}`) to prevent inbox flooding/DoS (e.g., 3 requests/hour), independent of IP limits.
*   **Enumeration Protection**: `Login` and `ForgotPassword` always return generic success/error messages (e.g., "Invalid email or password") even if the user does not exist.

### 7.2 Session Lifecycle

*   **Creation**: On successful login, a session record is created in the DB. Session ID is returned **only** via `Set-Cookie` header (HttpOnly, Secure, SameSite=Lax). It is **not** included in the JSON response body.
*   **Storage**: The session ID is sent as a `HttpOnly`, `Secure` cookie.
*   **Validation**: Each request validates the session ID against the DB.
*   **Logout**: Invokes `DELETE FROM sessions WHERE id = $1` (Hard Delete) to ensure immediate invalidation. **Client-Side Cookie Cleanup**: The `HandleLogout` handler MUST write a `Set-Cookie` header with the session cookie name, `Max-Age=0`, and `Expires` set to the Unix epoch (`Thu, 01 Jan 1970 00:00:00 GMT`) to force immediate browser deletion of the cookie.
*   **Password Reset**: `CompletePasswordReset` must perform a hard delete of **all** active sessions for the user to revoke access immediately.
*   **AcceptInvite Atomicity**: The `AcceptInvite` flow MUST execute User Status Update (`status='invited'` → `status='active'`, clear `invite_token_hash`) and Session Creation within a single database transaction. If session creation fails, rollback the entire transaction to keep the invite token valid for retry. This prevents partial state where the user is activated but has no valid session.

### 7.3 Email Canonicalization

All email inputs are passed through a normalizer before DB lookup:
```go
func CanonicalizeEmail(email string) string {
    return strings.ToLower(strings.TrimSpace(email))
}
```

### 7.4 Error Mapping

Internal errors are mapped to specific HTTP codes:

| Internal Error | HTTP Status | Client Message |
|---|---|---|
| `ErrAuthLocked` | 429 | "Too many failed attempts. Try again later." |
| `ErrAuthInvalidCreds` | 401 | "Invalid email or password." |
| `ErrAuthUserNotFound` | 401 | "Invalid email or password." (Masked) |
| `ErrAuthAccountNotActive` | 403 | "Account not active. Please check your invite." |
| `ErrAuthEmailNotVerified` | 403 | "Social login email not verified." |

---

## 8. OAuth Implementation

### 8.1 Interfaces

To allow testing without external calls, the OAuth layer is abstracted.

```go
type OAuthProvider interface {
    Name() string
    GetLoginURL(state string) string
    Exchange(ctx context.Context, code string) (*OAuthProfile, error)
}

type OAuthManager interface {
    GetProvider(name string) (OAuthProvider, error)
}
```

### 8.2 CSRF Protection (State)

Because the API is stateless (Lambda), the `state` parameter for OAuth is handled via a **Signed Cookie**.

1.  **Generate**: Random string + HMAC signature.
2.  **Cookie**: Set short-lived `oauth_state` cookie.
3.  **Redirect**: Send user to provider with raw state.
4.  **Verify**: On callback, check `state` param matches `oauth_state` cookie value.

### 8.3 Account Linking Logic

When `LoginWithOAuth` is called:
1.  **Lookup**: Find user by email.
2.  **Verify**: Check `profile.EmailVerified`. If false, reject with `ErrAuthEmailNotVerified`.
3.  **Link**:
    *   If user exists with `status='active'`: Update `auth_provider` and `auth_provider_id` if not already set. **Strict Rejection Policy**: If user exists (verified email) but `auth_provider` does not match the current provider, return `409 Conflict`. Do not overwrite or merge.
    *   If user missing: Create new user with `status = 'active'`, `auth_provider`, and `auth_provider_id`.
    *   **Implicit Acceptance**: If user exists with `status='invited'`, successful OAuth login MUST execute in a single transaction:
        1.  Set `status='active'`.
        2.  Clear `invite_token_hash` and `invite_expires_at` (the invite is now consumed).
        3.  Link Provider: Set `auth_provider` and `auth_provider_id` from the OAuth profile.
        4.  Create Session.
        Rollback if any step fails to keep the invite token valid for retry. This allows invited users to skip the explicit accept-invite flow by signing in with their OAuth provider.

---

## 9. Flow Coverage

| Flow ID | Description | Component | Method |
|---|---|---|---|
| `USER-006` | User Login | `AuthHandler` | `HandleLogin` |
| `USER-007` | OAuth Login | `AuthHandler` | `HandleOAuthCallback` |
| `USER-005` | Accept Invite | `AuthHandler` | `HandleAcceptInvite` |
| `USER-008` | Password Reset | `AuthHandler` | `HandleForgotPassword` / `HandleResetPassword` |
| `DASH-001` | Session Creation + Lazy Cleanup | `SessionService` | `CreateSession` |
| `DASH-002` | JIT Org Provisioning (OAuth) | `AuthService` | `LoginWithOAuth` |
| `DASH-004` | Logout | `SessionService` | `InvalidateSession` |
| `SEC-001` | Brute Force Lockout | `BruteForceProtector` | `CheckLoginAllowed` |
| `SEC-005` | Input Sanitization | `AuthHandler` | `CanonicalizeEmail` |