# 05d - API Organization & Users

> **Purpose**: Defines the HTTP handlers, request/response structures, and business logic for Organization settings, User management (RBAC), and API Key lifecycle.
> **Package**: `package handlers`
> **Dependencies**: `05a-api-core.md`, `01-foundation-types.md`, `02-foundation-db.md`

---

## Table of Contents

1. [Overview](#1-overview)
2. [Domain Interfaces](#2-domain-interfaces)
3. [Organization Handler](#3-organization-handler)
4. [User Handler](#4-user-handler)
5. [API Key Handler](#5-api-key-handler)
6. [Route Registration](#6-route-registration)
7. [Validation Rules & Scopes](#7-validation-rules--scopes)
8. [Flow Coverage](#8-flow-coverage)

---

## 1. Overview

This module manages the administrative aspects of the WatchPoint platform. It enforces a Multi-Owner RBAC model and handles the secure lifecycle of credentials (API Keys).

### Key Responsibilities
*   **Organization**: Managing profile, billing contact, and global notification preferences (Quiet Hours).
*   **Users**: Inviting members, enforcing role transition rules (e.g., preventing removal of the last Owner), and handling self-removal ("Leave Organization").
*   **API Keys**: issuing secure keys, listing keys (masked), rotating keys (dual-validity), and revocation.

---

## 2. Domain Interfaces

To support business logic without coupling to specific implementations, the handlers rely on these service interfaces in addition to the repositories.

### 2.1 Services

```go
// TokenService generates cryptographically secure tokens for invites.
type TokenService interface {
    // Generate returns a random token and its bcrypt hash.
    Generate() (token string, tokenHash string, err error)
}

// EmailService abstracts the transactional email provider (AWS SES).
type EmailService interface {
    SendInvite(ctx context.Context, toEmail string, inviteURL string, role types.UserRole) error
}

// BillingService handles synchronous billing actions required during admin tasks.
type BillingService interface {
    // CancelSubscription stops billing immediately. Used during Org Soft Delete.
    CancelSubscription(ctx context.Context, orgID string) error
}
```

### 2.2 Extended Repositories

These interfaces extend the base CRUD defined in `02-foundation-db.md` with domain-specific atomic operations.

```go
type UserRepository interface {
    // Standard
    GetByID(ctx context.Context, id string, orgID string) (*types.User, error)
    Update(ctx context.Context, user *types.User) error
    Delete(ctx context.Context, id string, orgID string) error // Soft delete
    
    // Logic Support
    GetByEmail(ctx context.Context, email string) (*types.User, error)
    CountOwners(ctx context.Context, orgID string) (int, error)
    
    // Invite Flow
    // Creates a user in 'invited' state with a hashed token
    CreateInvited(ctx context.Context, user *types.User, tokenHash string, expiresAt time.Time) error
    
    // Re-Invite Flow
    // Updates only the token and expiry for an existing invited user
    UpdateInviteToken(ctx context.Context, userID string, tokenHash string, expiresAt time.Time) error
}

type APIKeyRepository interface {
    // List retrieves API keys for an organization with optional filtering.
    // Supports Prefix filtering for compromise recovery (e.g., finding keys by
    // leaked prefix "sk_live_abc..."). When Prefix is non-empty, filters by
    // `key_prefix LIKE $prefix%`.
    List(ctx context.Context, orgID string, params ListAPIKeysParams) ([]*types.APIKey, error)
    Create(ctx context.Context, key *types.APIKey) error
    Delete(ctx context.Context, id string, orgID string) error // Revoke

    // Rotate updates the old key's expiry (grace period) and inserts the new key hash
    Rotate(ctx context.Context, id string, orgID string, newHash string, graceEnd time.Time) error

    // RevokeByUser bulk revokes keys when a user is deleted
    RevokeByUser(ctx context.Context, userID string, orgID string) error

    // TouchLastUsed updates the timestamp (fire-and-forget optimization)
    TouchLastUsed(ctx context.Context, id string) error

    // CountRecentByUser returns the count of API keys created by a user in the given time window.
    // Used for proactive rate limiting to prevent rapid key cycling during compromise.
    CountRecentByUser(ctx context.Context, userID string, since time.Time) (int, error)
}

// SessionRepository is required by UserHandler to invalidate sessions on user deletion/block
type SessionRepository interface {
    // DeleteByUser removes all active sessions for a specific user ID
    DeleteByUser(ctx context.Context, userID string) error
}
```

---

## 3. Organization Handler

Manages organization-wide settings. Note that dynamic usage data is delegated to the Billing API.

### 3.1 Handler Definition

```go
type OrganizationHandler struct {
    repo           db.OrganizationRepository
    wpRepo         db.WatchPointRepository  // Required for cascading pause on delete
    notifRepo      db.NotificationRepository // Required for organization-wide notification history
    auditRepo      db.AuditRepository       // Required for audit log queries
    planRegistry   PlanRegistry             // Defined in 05e-api-billing.md
    billingService BillingService
    audit          AuditLogger
    validator      *core.Validator
}

func NewOrganizationHandler(
    repo db.OrganizationRepository,
    wpRepo db.WatchPointRepository,
    notifRepo db.NotificationRepository,
    auditRepo db.AuditRepository,
    planRegistry PlanRegistry,
    billing BillingService,
    audit AuditLogger,
    v *core.Validator,
) *OrganizationHandler
```

### 3.2 Request/Response Models

```go
// Response includes static config only. Dynamic usage is in /v1/usage.
type OrganizationResponse struct {
    ID                      string                         `json:"id"`
    Name                    string                         `json:"name"`
    BillingEmail            string                         `json:"billing_email"`
    Plan                    types.PlanTier                 `json:"plan"`
    PlanLimits              types.PlanLimits               `json:"plan_limits"`
    NotificationPreferences types.NotificationPreferences  `json:"notification_preferences"`
    CreatedAt               time.Time                      `json:"created_at"`
}

// PATCH /v1/organization
type UpdateOrganizationRequest struct {
    Name         *string `json:"name,omitempty" validate:"omitempty,max=200"`
    BillingEmail *string `json:"billing_email,omitempty" validate:"omitempty,email"`
}

// PATCH /v1/organization/notification-preferences
type UpdatePreferencesRequest struct {
    QuietHours *QuietHoursUpdate `json:"quiet_hours,omitempty"`
    Digest     *DigestUpdate     `json:"digest,omitempty"`
}

type QuietHoursUpdate struct {
    Enabled  *bool                `json:"enabled,omitempty"`
    // "quiet_schedule" is a custom validation tag
    Schedule *[]types.QuietPeriod `json:"schedule,omitempty" validate:"omitempty,quiet_schedule"`
    Timezone *string              `json:"timezone,omitempty" validate:"omitempty,timezone"`
}

type DigestUpdate struct {
    Enabled      *bool   `json:"enabled,omitempty"`
    Frequency    *string `json:"frequency,omitempty" validate:"omitempty,oneof=daily weekly"`
    DeliveryTime *string `json:"delivery_time,omitempty" validate:"omitempty,time_24h"` // "HH:MM"
}
```

### 3.3 Endpoints

| Method | Path | Action | Permissions | Flow ID |
|---|---|---|---|---|
| POST | `/` | `Create` | (Signup Flow) | `USER-001` |
| GET | `/` | `Get` | Member+ | `INFO-002` |
| PATCH | `/` | `Update` | Admin+ | `USER-002` |
| DELETE | `/` | `Delete` | Owner Only | `USER-003` |
| GET | `/notification-preferences` | `GetPreferences` | Member+ | `INFO-002` |
| PATCH | `/notification-preferences` | `UpdatePreferences` | Admin+ | `USER-002` |
| GET | `/notifications` | `GetNotificationHistory` | Member+ | `INFO-002` |
| GET | `/audit-logs` | `ListAuditLogs` | Admin+, Owner | `OBS-010` |

**UpdatePreferences Side Effect**:
When `digest` preferences are updated, the handler MUST recalculate `next_digest_at` based on the new schedule and current time, updating the column atomically with the JSONB change.

### 3.4 Audit Log Query

```go
// GET /v1/organization/audit-logs
// ListAuditLogs returns paginated audit events for the organization.
// Requires RoleAdmin or RoleOwner permissions.
func (h *OrganizationHandler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
    // 1. Parse query params into types.AuditQueryFilters
    // 2. Call h.auditRepo.List(ctx, filters)
    // 3. Return AuditLogResponse
}

// AuditLogResponse wraps audit events with pagination info.
type AuditLogResponse struct {
    Data     []*types.AuditEvent `json:"data"`
    PageInfo types.PageInfo      `json:"pagination"`
}
```

### 3.5 Organization Notification History

```go
// GET /v1/organization/notifications
// GetNotificationHistory returns paginated notification history for the organization.
// Provides transparency to all members by showing notifications across all WatchPoints.
// Requires RoleMember or higher permissions.
func (h *OrganizationHandler) GetNotificationHistory(w http.ResponseWriter, r *http.Request) {
    // 1. Extract orgID from context
    // 2. Parse query params (event_type, urgency) into types.NotificationFilter
    // 3. Set filter.OrganizationID = orgID (WatchPointID remains empty for org-wide)
    // 4. Call h.notifRepo.List(ctx, filter)
    // 5. Return types.ListResponse[types.NotificationHistoryItem]
}
```

**Logic Highlights**:

*   **Create (`POST /`)** - Signup Flow:
    1.  Start DB Transaction.
    2.  Fetch plan limits from `PlanRegistry.GetLimits(planTier)`.
    3.  Insert Organization record with limits.
    4.  Insert User record (owner role, status='active').
    5.  Commit transaction.
    6.  Call `BillingService.EnsureCustomer(orgID, billingEmail)` (**Best Effort**). Log error if Stripe fails but do not fail the request—the customer record will be created on first billing interaction.

*   **Update (`PATCH /`)**: If `billing_email` changes, trigger asynchronous `BillingService.UpdateCustomerEmail(orgID, newEmail)`. Log warning on failure but do not fail the API request (**Best Effort**).

*   **Delete (`DELETE /`)** - Soft Delete:
    1.  Validate `Owner` role.
    2.  Call `BillingService.CancelSubscription(orgID)`. **Fail Fast (500)** if this returns an error to prevent zombie billing—the subscription must be cancelled before proceeding.
    3.  Call `wpRepo.PauseAllByOrgID(orgID)` to immediately stop evaluation overhead for all WatchPoints.
    4.  Set `deleted_at = NOW()` (soft delete).
    5.  Does *not* purge data immediately (background job `MAINT-005` handles hard delete).

---

## 4. User Handler

Manages user lifecycle, invites, and RBAC enforcement.

### 4.1 Handler Definition

```go
type UserHandler struct {
    userRepo     UserRepository
    apiKeyRepo   APIKeyRepository // For cascading revocation (defined above)
    sessionRepo  SessionRepository // For session invalidation (defined above)
    txManager    db.TransactionManager
    tokenService TokenService
    emailService EmailService
    audit        AuditLogger
}
```

### 4.2 Request/Response Models

```go
// GET /v1/users
type ListUsersParams struct {
    Role   *types.UserRole `json:"role,omitempty"`
    Limit  int             `json:"limit"`
    Cursor string          `json:"cursor,omitempty"`
}

type ListUsersResponse struct {
    Data     []UserDTO      `json:"data"`
    PageInfo types.PageInfo `json:"pagination"`
}

type UserDTO struct {
    ID              string         `json:"id"`
    Email           string         `json:"email"`
    Role            types.UserRole `json:"role"`
    Status          string         `json:"status"` // "active" | "invited"
    InviteExpiresAt *time.Time     `json:"invite_expires_at,omitempty"`
    LastLoginAt     *time.Time     `json:"last_login_at,omitempty"`
}

// POST /v1/users/invite
type InviteUserRequest struct {
    Email string         `json:"email" validate:"required,email"`
    Role  types.UserRole `json:"role" validate:"required,oneof=owner admin member"`
}

// PATCH /v1/users/{id}
type UpdateUserRequest struct {
    Role *types.UserRole `json:"role" validate:"required,oneof=owner admin member"`
}
```

### 4.3 Endpoints

| Method | Path | Action | Permissions | Flow ID |
|---|---|---|---|---|
| GET | `/` | `List` | Member+ | `INFO-002` |
| POST | `/invite` | `Invite` | Admin+ | `USER-004` |
| POST | `/{id}/resend-invite` | `ResendInvite` | Admin+ | `USER-004` |
| PATCH | `/{id}` | `UpdateRole` | Owner (Any), Admin (Members) | `USER-009` |
| DELETE | `/{id}` | `Delete` | Owner (Any), Admin (Members), Self | `USER-010` |

### 4.4 Business Logic

1.  **Invite Flow (`POST /invite`)**:
    *   Generates secure token. Hashes token for DB. Sends raw token in email link.
    *   Creates user with `status='invited'`.
    *   **Duplicate Handling**: Catch unique constraint violation on email. Return **409 Conflict** with message `{"error": "User already exists"}`.
    *   **Email Delivery**: Send invite email synchronously via `EmailService.SendInvite`. Return **500 Internal Server Error** if SES fails to ensure the admin knows the invite did not go out.
2.  **Resend Invite**:
    *   Regenerates token for existing `invited` user. Updates expiry. Resends email.
3.  **Role Transition Matrix**:
    *   Admin cannot promote to Owner.
    *   Admin cannot demote Owner.
    *   Owner can perform any transition.
4.  **Deletion Constraints**:
    *   **Self-Deletion**: Allowed ("Leave Org").
    *   **Last Owner**: `CountOwners` check prevents removing/demoting the last Owner. **Concurrency Safety**: Must use `SELECT ... FOR UPDATE` or serializable isolation when counting owners to prevent concurrent race conditions from deleting the final owner.
    *   **Cascading**: Transactionally invalidates sessions. **API Key Persistence**: API Keys created by the user are organizational assets and **remain active**. The `created_by_user_id` field is set to NULL (handled via DB `ON DELETE SET NULL`).

---

## 5. API Key Handler

Manages programmatic access credentials.

### 5.1 Handler Definition

```go
type APIKeyHandler struct {
    repo   APIKeyRepository
    audit  AuditLogger
}
```

### 5.2 Request/Response Models

```go
// POST /v1/api-keys
type CreateAPIKeyRequest struct {
    Name          string   `json:"name" validate:"required,max=100"`
    Scopes        []string `json:"scopes" validate:"required,min=1,scopes"` // Validated against AllScopes
    ExpiresInDays int      `json:"expires_in_days" validate:"min=1,max=365"`
    Source        string   `json:"source,omitempty" validate:"omitempty,max=50,alphanum"` // Vertical app identity (VERT-001). Restricted to Admin/Owner roles.
}

// Unsafe response (returned ONCE)
type APIKeySecretResponse struct {
    APIKeyResponse
    NewKey       string    `json:"new_key"` // Plaintext (sk_live_...)
    NewKeyPrefix string    `json:"new_key_prefix"`
    
    // For Rotation
    PreviousKeyValidUntil *time.Time `json:"previous_key_valid_until,omitempty"`
}

// Safe response (for List/Get)
type APIKeyResponse struct {
    ID         string     `json:"id"`
    Name       string     `json:"name"`
    KeyPrefix  string     `json:"key_prefix"`
    Scopes     []string   `json:"scopes"`
    LastUsedAt *time.Time `json:"last_used_at,omitempty"`
    ExpiresAt  *time.Time `json:"expires_at,omitempty"`
    CreatedAt  time.Time  `json:"created_at"`
}

type ListAPIKeysParams struct {
    ActiveOnly bool   `json:"active"`
    Prefix     string `json:"prefix,omitempty"` // Filter by key prefix (e.g., "sk_live_abc...") for compromise recovery
    Limit      int    `json:"limit"`
    Cursor     string `json:"cursor"`
}
```

### 5.3 Endpoints

| Method | Path | Action | Permissions | Flow ID |
|---|---|---|---|---|
| GET | `/` | `List` | Admin+ | `INFO-002` |
| POST | `/` | `Create` | Admin+ | `USER-011` |
| POST | `/{id}/rotate` | `Rotate` | Admin+ | `USER-012` |
| DELETE | `/{id}` | `Revoke` | Admin+ | `USER-013` |

### 5.4 Business Logic

1.  **Actor Validation**: Only human Users can create API keys. API Keys cannot create other keys.
2.  **Scope Validation**: Requested scopes must exist in `types.AllScopes`.
3.  **Proactive Rate Limit**: Before creating a new key, query `APIKeyRepository.CountRecentByUser`
    with a 1-hour window. If count > 5, return **429 Too Many Requests** with message
    `{"error": {"code": "rate_limit_exceeded", "message": "Too many API keys created recently. Try again later."}}`.
    This prevents rapid key cycling during account compromise scenarios.
4.  **Source Provisioning (VERT-001)**: If `Source` is provided in the request:
    *   Validate that the Actor has Admin or Owner role. Members cannot provision source-tagged keys.
    *   Persist the `Source` value into the `api_keys.source` column.
    *   If `Source` is empty or omitted, the column defaults to NULL (treated as "default" at runtime).
5.  **Secure Key Generation**:
    *   The Handler generates the plaintext secret (e.g., `sk_live_...`) in memory using a cryptographically secure random generator.
    *   The Handler computes the bcrypt hash of the secret.
    *   The Handler populates the `types.APIKey` struct with the **Hash only** (not the plaintext).
    *   The plaintext secret is injected into the `APIKeySecretResponse` object for the one-time response.
    *   The plaintext secret is **never** passed to the repository or persisted—only the hash is stored.
6.  **Rotation (Dual Validity)**:
    *   Generates new key.
    *   Updates old key record: `expires_at = NOW() + 24h`.
    *   Inserts new key record.
    *   Returns new secret.
7.  **Leakage Prevention**: `List` endpoint *never* returns the full secret, only the prefix.

---

## 6. Route Registration

Authentication Middleware is applied globally to this router.

```go
func (h *OrganizationHandler) RegisterRoutes(r chi.Router) {
    // Organization
    r.Route("/organization", func(r chi.Router) {
        r.Get("/", h.Get)
        r.Patch("/", h.RequireRole(RoleAdmin, h.Update))
        r.Delete("/", h.RequireRole(RoleOwner, h.Delete))

        r.Get("/notification-preferences", h.GetPreferences)
        r.Patch("/notification-preferences", h.RequireRole(RoleAdmin, h.UpdatePreferences))

        // Organization-wide notification history - transparency for all members
        r.With(h.RequireRole(RoleMember)).Get("/notifications", h.GetNotificationHistory)

        // Audit Log Query - requires Admin or Owner role
        r.With(h.RequireRole(RoleAdmin, RoleOwner)).Get("/audit-logs", h.ListAuditLogs)
    })

    // Users
    r.Route("/users", func(r chi.Router) {
        r.Get("/", h.userHandler.List)
        r.Post("/invite", h.RequireRole(RoleAdmin, h.userHandler.Invite))
        
        r.Route("/{id}", func(r chi.Router) {
            r.Patch("/", h.userHandler.UpdateRole) // Logic handles role matrix
            r.Delete("/", h.userHandler.Delete)    // Logic handles self-deletion
            r.Post("/resend-invite", h.RequireRole(RoleAdmin, h.userHandler.ResendInvite))
        })
    })

    // API Keys
    r.Route("/api-keys", func(r chi.Router) {
        r.Use(h.RequireRole(RoleAdmin)) // All key ops require Admin
        r.Get("/", h.apiKeyHandler.List)
        r.Post("/", h.apiKeyHandler.Create)
        r.Post("/{id}/rotate", h.apiKeyHandler.Rotate)
        r.Delete("/{id}", h.apiKeyHandler.Revoke)
    })
}
```

---

## 7. Validation Rules & Scopes

Specific validators registered in `05a-api-core` for this module:

*   `quiet_schedule`: Validates `[]QuietPeriod`. Checks start < end, overlapping windows.
*   `timezone`: Validates string against IANA database.
*   `scopes`: Validates that each string in the list exists in `types.AllScopes` (defined in `01-foundation-types.md` Section 4.5).

---

## 8. Flow Coverage

| Flow ID | Description | Component | Method |
|---|---|---|---|
| `USER-002` | Organization Update | `OrganizationHandler` | `Update` |
| `USER-003` | Organization Soft Delete | `OrganizationHandler` | `Delete` |
| `USER-004` | Invite User | `UserHandler` | `Invite` |
| `USER-009` | User Role Change | `UserHandler` | `UpdateRole` |
| `USER-010` | User Removal | `UserHandler` | `Delete` |
| `USER-011` | API Key Creation | `APIKeyHandler` | `Create` |
| `USER-012` | API Key Rotation | `APIKeyHandler` | `Rotate` |
| `USER-013` | API Key Revocation | `APIKeyHandler` | `Revoke` |
| `INFO-002` | Org/User/Key Info | All Handlers | `Get`/`List` |
| `SEC-002` | Brute Force (Key) | `01-foundation` | Implicit (Rate Limiting) |
| `BILL-006` | Billing Cancel (Side Effect) | `OrganizationHandler` | `Delete` |