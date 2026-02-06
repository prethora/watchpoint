# 05b - API WatchPoints

> **Purpose**: Defines the HTTP handlers, request/response structures, and business logic for managing WatchPoints. This includes CRUD operations, state transitions (pause/resume), cloning, and bulk management.
> **Package**: `package handlers`
> **Dependencies**: `05a-api-core.md`, `01-foundation-types.md`, `02-foundation-db.md`

---

## Table of Contents

1. [Overview](#1-overview)
2. [Handler Definition](#2-handler-definition)
3. [Domain Interfaces](#3-domain-interfaces)
4. [Request & Response Models](#4-request--response-models)
5. [Route Registration](#5-route-registration)
6. [Endpoint Logic](#6-endpoint-logic)
7. [Validation & Errors](#7-validation--errors)
8. [Flow Coverage](#8-flow-coverage)

---

## 1. Overview

The WatchPoint Handler manages the core resource of the platform. It handles:
*   **Polymorphic Creation**: Supporting both Event Mode (Time Window) and Monitor Mode (Rolling Window).
*   **Lifecycle Management**: CRUD, Pausing, Resuming, and Deleting.
*   **Composite Retrieval**: Aggregating configuration, runtime evaluation state, and live forecast snapshots.
*   **Bulk Operations**: Efficient batch processing for high-volume users.

It relies on `05a-api-core` for authentication (Actor extraction) and generic middleware, while defining domain-specific business logic here.

---

## 2. Handler Definition

The handler allows dependency injection to decouple the HTTP layer from specific database or forecast implementations.

```go
type WatchPointHandler struct {
    wpRepo        db.WatchPointRepository
    orgRepo       db.OrganizationRepository
    notifRepo     db.NotificationRepository // For notification history queries
    validator     *core.Validator
    logger        *slog.Logger

    // Domain Services
    forecastProvider ForecastProvider
    usageEnforcer    UsageEnforcer
    auditLogger      AuditLogger
    evalTrigger      EvaluationTrigger

    // Config
    constraints      config.WatchPointConstraints
}

// NewWatchPointHandler initializes the handler with all dependencies.
func NewWatchPointHandler(
    wpRepo db.WatchPointRepository,
    orgRepo db.OrganizationRepository,
    notifRepo db.NotificationRepository,
    validator *core.Validator,
    logger *slog.Logger,
    forecastProvider ForecastProvider,
    usageEnforcer UsageEnforcer,
    auditLogger AuditLogger,
    evalTrigger EvaluationTrigger,
    constraints config.WatchPointConstraints,
) *WatchPointHandler
```

---

## 3. Domain Interfaces

To ensure testability and loose coupling, the handler relies on the following interfaces rather than concrete structs.

### 3.1 Data Access (Repository Extensions)

Extends the standard CRUD defined in `02-foundation-db.md` with batch and state-specific methods.

```go
type WatchPointRepository interface {
    // Standard CRUD
    Create(ctx context.Context, wp *types.WatchPoint) error
    GetByID(ctx context.Context, id string, orgID string) (*types.WatchPoint, error)
    Update(ctx context.Context, wp *types.WatchPoint) error
    Delete(ctx context.Context, id string, orgID string) error
    List(ctx context.Context, orgID string, params ListWatchPointsParams) ([]*types.WatchPoint, types.PageInfo, error)

    // State Access
    GetEvaluationState(ctx context.Context, id string) (*types.EvaluationState, error)

    // Batch Operations
    CreateBatch(ctx context.Context, wps []*types.WatchPoint) (createdIndices []int, failedIndices map[int]error, err error)
    // UpdateStatusBatch signature defined in 02-foundation-db.md (Section 9.2).
    // Requires testMode parameter to enforce environment isolation.
    UpdateStatusBatch(ctx context.Context, ids []string, orgID string, status types.Status, testMode bool) (int64, error)
    // DeleteBatch signature defined in 02-foundation-db.md (Section 9.2).
    // Requires testMode parameter to enforce environment isolation.
    DeleteBatch(ctx context.Context, ids []string, orgID string, testMode bool) (int64, error)

    // NOTE: ListNotifications has been removed. Use NotificationRepository.List instead.
}
```

### 3.2 External Services

```go
// ForecastProvider fetches a lightweight snapshot of current conditions.
type ForecastProvider interface {
    // GetSnapshot returns read-only current variables. 
    // Returns nil error on failure (graceful degradation).
    GetSnapshot(ctx context.Context, lat, lon float64) (*types.ForecastSnapshot, error)
}

// UsageEnforcer checks plan limits before creation.
type UsageEnforcer interface {
    CheckLimit(ctx context.Context, orgID string, resource types.ResourceType, count int) error
}

// AuditLogger records business events.
type AuditLogger interface {
    Log(ctx context.Context, event types.AuditEvent) error
}

// EvaluationTrigger enqueues an immediate check (e.g., on Resume).
// TriggerEvaluation enqueues an `EvalMessage`. Implementations MUST map the
// `wpID` argument to the `SpecificWatchPointIDs` field in the message to
// ensure targeted execution.
type EvaluationTrigger interface {
    TriggerEvaluation(ctx context.Context, wpID string, reason string) error
}
```

---

## 4. Request & Response Models

### 4.1 Single Resource Operations

```go
// CreateWatchPointRequest (POST /)
type CreateWatchPointRequest struct {
    Name           string                  `json:"name" validate:"required,max=200"`
    Location       types.Location          `json:"location" validate:"required"`
    Timezone       string                  `json:"timezone" validate:"required,timezone"`
    
    // Mode Exclusivity enforced via validation tags
    TimeWindow     *types.TimeWindow       `json:"time_window,omitempty" validate:"required_without=MonitorConfig,excluded_with=MonitorConfig"`
    MonitorConfig  *types.MonitorConfig    `json:"monitor_config,omitempty" validate:"required_without=TimeWindow,excluded_with=TimeWindow"`
    
    Conditions     []types.Condition       `json:"conditions" validate:"required,min=1,max=10,dive"`
    ConditionLogic types.ConditionLogic    `json:"condition_logic" validate:"required,oneof=ANY ALL"`
    Channels       []types.Channel         `json:"channels" validate:"required,min=1,dive"`
    
    Preferences    *types.Preferences      `json:"preferences,omitempty"`
    TemplateSet    string                  `json:"template_set,omitempty" validate:"omitempty,max=50"`
    Tags           []string                `json:"tags,omitempty" validate:"max=10,dive,max=50"`
    Metadata       map[string]any          `json:"metadata,omitempty"`
}

// UpdateWatchPointRequest (PATCH /{id})
type UpdateWatchPointRequest struct {
    Name           *string             `json:"name,omitempty" validate:"omitempty,max=200"`
    Conditions     *[]types.Condition  `json:"conditions,omitempty" validate:"omitempty,min=1,max=10,dive"`
    Channels       *[]types.Channel    `json:"channels,omitempty" validate:"omitempty,min=1,dive"`
    Status         *types.Status       `json:"status,omitempty" validate:"omitempty,oneof=active paused archived"`
    Preferences    *types.Preferences  `json:"preferences,omitempty"`
    // TimeWindow and MonitorConfig updates are restricted to prevent mode-switching
}

// WatchPointDetail (GET /{id})
// Aggregates config, runtime state, and forecast snapshot.
type WatchPointDetail struct {
    *types.WatchPoint
    EvaluationState *types.EvaluationState  `json:"evaluation_state,omitempty"`
    CurrentForecast *types.ForecastSnapshot `json:"current_forecast,omitempty"`
}
```

### 4.2 Bulk Operations

```go
// BulkFilter is defined in types package (01-foundation-types.md Section 9.2).
// Reference: types.BulkFilter

// BulkActionRequest (Pause, Resume, Delete)
type BulkActionRequest struct {
    IDs     []string          `json:"ids,omitempty" validate:"required_without=Filter,max=500"`
    Filter  *types.BulkFilter `json:"filter,omitempty" validate:"required_without=IDs"`
    Confirm bool              `json:"confirm,omitempty"` // Required for Delete
}

// BulkResponse for status changes/deletes
type BulkResponse struct {
    SuccessCount int `json:"success_count"`
    FailureCount int `json:"failure_count"`
}

// BulkCreateResponse for imports (POST /bulk)
type BulkCreateResponse struct {
    Successes []BulkCreateSuccess `json:"successes"`
    Failures  []BulkCreateFailure `json:"failures"`
}

type BulkCreateSuccess struct {
    Index      int               `json:"index"`
    ID         string            `json:"id"`
    WatchPoint *types.WatchPoint `json:"watchpoint"`
}

type BulkCreateFailure struct {
    Index   int             `json:"index"`
    Code    types.ErrorCode `json:"code"`
    Message string          `json:"message"`
}

// BulkTagUpdateRequest (PATCH /bulk/tags)
type BulkTagUpdateRequest struct {
    Filter     types.BulkFilter `json:"filter" validate:"required"`
    AddTags    []string         `json:"add_tags,omitempty" validate:"dive,max=50"`
    RemoveTags []string         `json:"remove_tags,omitempty" validate:"dive,max=50"`
}
```

### 4.3 Cloning

```go
// TimeShift allows relative rescheduling (e.g., "+1 Year").
type TimeShift struct {
    Years   int `json:"years,omitempty"`
    Months  int `json:"months,omitempty"`
    Days    int `json:"days,omitempty"`
    Hours   int `json:"hours,omitempty"`
}

// CloneWatchPointRequest (POST /{id}/clone)
type CloneWatchPointRequest struct {
    Name       *string           `json:"name,omitempty"`
    TimeWindow *types.TimeWindow `json:"time_window,omitempty"` // Explicit new window
    TimeShift  *TimeShift        `json:"time_shift,omitempty"`  // Relative shift
    Status     types.Status      `json:"status,omitempty" validate:"omitempty,oneof=active paused"`
}

// BulkCloneRequest (POST /bulk/clone)
type BulkCloneRequest struct {
    SourceIDs []string  `json:"source_ids" validate:"required,min=1,max=100"`
    TimeShift TimeShift `json:"time_shift" validate:"required"`
    Override  struct {
        Status types.Status `json:"status"`
        Tags   []string     `json:"tags"`
    } `json:"override,omitempty"`
}

// BulkCloneResponse
type BulkCloneResponse struct {
    CreatedCount int               `json:"created_count"`
    Mapping      map[string]string `json:"mapping"` // SourceID -> NewID
}
```

### 4.4 History & Listing

```go
// ListWatchPointsParams binding struct
type ListWatchPointsParams struct {
    Status []types.Status `json:"status"`
    Tags   []string       `json:"tags"`
    Limit  int            `json:"limit"`
    Cursor string         `json:"cursor"`
}

// NOTE: NotificationHistoryItem and NotificationHistoryResponse have been removed.
// Use types.NotificationHistoryItem and types.ListResponse[types.NotificationHistoryItem]
// from 01-foundation-types.md (Section 10.6) instead.
```

---

## 5. Route Registration

The `Routes()` method creates a `chi.Mux` and maps the following endpoints. Authentication middleware is applied globally to this sub-router.

| Method | Path | Handler | Flow Ref |
|---|---|---|---|
| POST | `/` | `Create` | WPLC-001, WPLC-002 |
| GET | `/` | `List` | INFO-008, DASH-008 |
| GET | `/stats` | `GetStats` | DASH-008 |
| POST | `/bulk` | `BulkCreate` | WPLC-011 |
| POST | `/bulk/pause` | `BulkPause` | BULK-002 |
| POST | `/bulk/resume` | `BulkResume` | BULK-003 |
| POST | `/bulk/delete` | `BulkDelete` | BULK-004 |
| POST | `/bulk/clone` | `BulkClone` | BULK-006 |
| PATCH | `/bulk/tags` | `BulkTagUpdate` | BULK-005 |
| GET | `/{id}` | `Get` | INFO-008 |
| PATCH | `/{id}` | `Update` | WPLC-003, WPLC-004 |
| DELETE | `/{id}` | `Delete` | WPLC-008 |
| POST | `/{id}/pause` | `Pause` | WPLC-005 |
| POST | `/{id}/resume` | `Resume` | WPLC-006 |
| POST | `/{id}/clone` | `Clone` | WPLC-010 |
| POST | `/{id}/channels/{channel_id}/rotate-secret` | `RotateChannelSecret` | HOOK-001 |
| GET | `/{id}/notifications` | `GetNotificationHistory` | INFO-001 |

---

## 6. Endpoint Logic

### 6.1 Create (`POST /`)
1.  **Decode & Validate**: Parse JSON, apply struct validation (polymorphic mode check). Validate conditions against `types.StandardVariables`.
2.  **Enforce Limits**: Call `UsageEnforcer.CheckLimit(WatchPoints, 1)`. Return 403 if exceeded.
    *   **Strict Consistency**: The limit check MUST use a direct `SELECT COUNT(*) FROM watchpoints WHERE organization_id = $1 AND status != 'archived' AND deleted_at IS NULL` query rather than relying on denormalized counters in the `organizations` or `rate_limits` tables. This ensures limit enforcement is always consistent with the actual number of active resources.
    *   **Soft Consistency Note**: While the direct count strategy minimizes drift, this check is subject to race conditions under high concurrency (e.g., two simultaneous create requests may both pass the limit check). The system explicitly accepts 'Soft Consistency' here; any overages created during race conditions are detected and paused by the daily `BILL-011` (Downgrade Enforcement) scheduled job, which reconciles actual counts against plan limits.
3.  **CONUS Check**: `types.IsCONUS`. If false and mode implies Nowcast, append Warning to response metadata.
4.  **Defaults**: Apply defaults (Status="active", TemplateSet="default").
5.  **Source Injection (VERT-002)**: The handler MUST inject `Actor.Source` into the `WatchPoint.Source` field upon creation. Do NOT trust the `source` field from the JSON request body—always use the Actor's Source to ensure immutable attribution to the creating vertical app. If `Actor.Source` is empty, default to "default".
6.  **Template Set Validation (VERT-004)**: The `template_set` field is validated only for string format (max 50 chars, alphanumeric with underscores). Validation of the template's *existence* is deferred to the Email Worker layer (Loose Coupling). This allows WatchPoints to be created before templates are provisioned.
7.  **Persist**: `Repo.Create`. *(Note: State creation is handled lazily by the Eval Worker, not the API.)*
8.  **Audit**: Emit `watchpoint.created` event.
9.  **Hydrate (Soft Dependency)**: Call `ForecastProvider.GetSnapshot`. If this call fails (error or timeout), log a warning but **DO NOT** fail the request. Proceed with creation and set `current_forecast: null` in the response.
10. **Evaluation Trigger**: For **Monitor Mode** WatchPoints, the API must **NOT** trigger an immediate evaluation. Unlike Event Mode (which may return a snapshot immediately) or Resume (which triggers immediate checks), Monitor Mode relies on the next scheduled Batcher cycle (up to 15m latency) to establish a baseline. This 'Quiet Create' policy prevents 'Alert Shock' on setup.
11. **Return**: 201 Created with `WatchPointDetail`.

### 6.2 Get (`GET /{id}`)
1.  **Extract**: `id` from URL, `orgID` from context.
2.  **Fetch Config**: `Repo.GetByID`. Return 404 if not found.
3.  **Fetch State**: `Repo.GetEvaluationState`. (Parallel execution encouraged).
4.  **Fetch Forecast**: `ForecastProvider.GetSnapshot`. (Graceful failure: on error, set field to null).
5.  **Deprecation Check**: For each webhook channel, call `PlatformRegistry.CheckDeprecation(url)`.
    Collect any warnings and include them in the response `Meta.Warnings` field.
    Example warning: "Teams Connectors are retiring December 2025. Migrate to Power Automate Workflows."
6.  **Return**: 200 OK with `WatchPointDetail` and populated `Meta.Warnings` if any deprecations detected.

### 6.3 Update (`PATCH /{id}`)
1.  **Decode & Validate**: Pointer fields allow partial updates.
2.  **Source Immutability (VERT-003)**: The `WatchPoint.Source` field is **immutable**. If a user updates a WatchPoint via the Dashboard (where `Actor.Source` might be "dashboard" or empty), the original `WatchPoint.Source` MUST be preserved. The handler should explicitly exclude the `source` field from the UPDATE statement or fetch-and-restore it during the update operation.
3.  **Persist**: `Repo.Update`.
4.  **Array Handling**: Conditions/Channels updates are **Replacements**, not Merges.
5.  **Audit**: Emit `watchpoint.updated`.
6.  **Return**: 200 OK.

### 6.4 Pause / Resume (`POST /{id}/pause|resume`)
1.  **State Check**: Verify current status (e.g., cannot pause if already paused).
2.  **Persist**: Update status.
3.  **Side Effect (Pause)**:
    1.  Call `DeliveryManager.CancelDeferred(id)` (or `notifRepo.CancelDeferredDeliveries`) to prevent silenced alerts from resurfacing after Quiet Hours end.
4.  **Side Effect (Resume)**:
    1.  Call `DeliveryManager.CancelDeferred(id)` to clear stale backlog.
    2.  Call `evalTrigger.TriggerEvaluation` passing the specific WatchPoint ID to trigger a targeted evaluation job.
5.  **Audit**: Emit `watchpoint.paused` or `watchpoint.resumed`.

### 6.5 Bulk Create (`POST /bulk`)
**Quiet Create Policy**: To optimize performance, this endpoint DOES NOT fetch forecast snapshots (returns `current_forecast: null`) and DOES NOT trigger immediate evaluation. WatchPoints will be picked up by the next scheduled Batcher run. State creation is handled lazily by the Eval Worker.

1.  **Validate**: Check array size (max 100).
2.  **Parallel Validation**: Implement parallel validation using an error group (e.g., `errgroup`) to validate batch items concurrently. This is crucial for batches containing multiple webhooks to prevent serial DNS resolution latencies from exceeding API timeouts.
3.  **Limit Check**: Check if `current + len(batch) > limit`.
4.  **Persist**: `Repo.CreateBatch`. *(Note: State creation is handled lazily by the Eval Worker, not the API.)*
5.  **Map Results**: Iterate response to build `BulkCreateSuccess` and `BulkCreateFailure` lists based on indices.
6.  **Audit**: Emit aggregated `watchpoint.bulk_created` event containing `count` and summary in metadata (not individual events per item).
7.  **Return**: 200 OK (even if some failed).

### 6.6 Bulk Pause (`POST /bulk/pause`)
1.  **Validate**: Validate `BulkActionRequest` (either IDs or Filter must be provided).
2.  **Resolve IDs**: If `Filter` is provided, resolve to concrete WatchPoint IDs.
3.  **Side Effect**: Call `DeliveryManager.CancelDeferred` (or repository equivalent) for the target IDs to prevent silenced alerts from resurfacing after Quiet Hours.
4.  **Persist**: Call `Repo.UpdateStatusBatch(ctx, ids, orgID, StatusPaused, Actor.IsTestMode)`. The `testMode` parameter ensures strict environment isolation.
5.  **Audit**: Emit **aggregated** `watchpoint.bulk_paused` event containing `count` and `filter/ids` in metadata. Do NOT emit individual events per item.
6.  **Return**: 200 OK with `BulkResponse`.

### 6.7 Bulk Resume (`POST /bulk/resume`)
1.  **Validate**: Validate `BulkActionRequest` (either IDs or Filter must be provided).
2.  **Resolve IDs**: If `Filter` is provided, resolve to concrete WatchPoint IDs.
3.  **Persist**: Call `Repo.UpdateStatusBatch(ctx, ids, orgID, StatusActive, Actor.IsTestMode)`. The `testMode` parameter ensures strict environment isolation.
4.  **Side Effect**: For each resumed WatchPoint, call `evalTrigger.TriggerEvaluation` to trigger immediate re-evaluation.
5.  **Audit**: Emit **aggregated** `watchpoint.bulk_resumed` event containing `count` and `filter/ids` in metadata. Do NOT emit individual events per item.
6.  **Return**: 200 OK with `BulkResponse`.

### 6.8 Bulk Delete (`POST /bulk/delete`)
1.  **Validate**: Validate `BulkActionRequest` (either IDs or Filter must be provided). Require `Confirm: true`.
2.  **Resolve IDs**: If `Filter` is provided, resolve to concrete WatchPoint IDs.
3.  **Persist**: Call `Repo.DeleteBatch(ctx, ids, orgID, Actor.IsTestMode)`. The `testMode` parameter ensures strict environment isolation.
4.  **Audit**: Emit **aggregated** `watchpoint.bulk_deleted` event containing `count` and `filter/ids` in metadata. Do NOT emit individual events per item.
5.  **Return**: 200 OK with `BulkResponse`.

### 6.9 Bulk Clone (`POST /bulk/clone`)
1.  **Fetch**: Call `Repo.GetBatch(sourceIDs, orgID)` to fetch all source WatchPoints in a single vectorized query rather than N individual fetches.
2.  **Transform**:
    *   Start Transaction.
    *   Iterate items: apply `TimeShift` (see below), reset Status/State, generate new ID.
    *   **TimeShift Mode Handling**: If the source `WatchPoint.MonitorConfig` is present (Monitor Mode), **ignore** the `TimeShift` parameter entirely. Time shifting only applies to Event Mode items with explicit `TimeWindow` values. Monitor Mode WatchPoints operate on rolling windows and have no fixed time reference to shift.
3.  **Persist**: `Repo.CreateBatch`.
4.  **Commit**.
5.  **Audit**: Emit **aggregated** `watchpoint.bulk_cloned` event containing `count` and source-to-new ID mapping in metadata. Do NOT emit individual events per item.
6.  **Return**: Mapping of SourceID -> NewID.

### 6.10 Bulk Tag Update (`PATCH /bulk/tags`)
Atomically updates tags for WatchPoints matching a filter without fetch-modify-save loops.

1.  **Validate**: Validate `BulkTagUpdateRequest`. At least one of `add_tags` or `remove_tags` must be provided.
2.  **Atomic Update**: Call `Repo.UpdateTagsBatch(ctx, orgID, filter, addTags, removeTags)`.
    *   This delegates to the repository method which generates atomic SQL:
        ```sql
        UPDATE watchpoints
        SET tags = (tags || $addTags) - $removeTags, updated_at = NOW()
        WHERE organization_id = $orgID AND [filter conditions]
        ```
    *   PostgreSQL array operators ensure atomicity and prevent race conditions.
3.  **Audit**: Emit **aggregated** `watchpoint.bulk_tag_updated` event containing `count`, `filter`, `add_tags`, and `remove_tags` in metadata. Do NOT emit individual events per item.
4.  **Return**: 200 OK with `BulkResponse` containing the count of updated WatchPoints.

### 6.11 Get Notification History (`GET /{id}/notifications`)
1.  **Extract**: `id` (WatchPoint ID) from URL, `orgID` from context.
2.  **Build Filter**: Create `types.NotificationFilter` with `WatchPointID` set.
3.  **Query**: Call `notifRepo.List(ctx, filter)`.
4.  **Return**: 200 OK with `types.ListResponse[types.NotificationHistoryItem]`.

### 6.12 Get Stats (`GET /stats`)
Returns aggregated counts for dashboard summary cards (Active, Error, Triggered, Paused, Total).

1.  **Extract**: `orgID` from context.
2.  **Query**: Call `Repo.GetStats(ctx, orgID)`.
3.  **Return**: 200 OK with `types.WatchPointStats`.

**Response Example**:
```json
{
  "data": {
    "active": 42,
    "error": 3,
    "triggered": 7,
    "paused": 5,
    "total": 50
  }
}
```

### 6.13 List with Summary Mode (`GET /`)
The List endpoint supports an optimized path for dashboard performance via query parameters.

**Query Parameters**:
*   `mode=summary`: Returns `types.WatchPointSummary` objects via `Repo.ListSummaries`.
    Excludes heavy JSONB fields (conditions, channels) and includes joined runtime state.
*   `mode=full` (default): Returns full `types.WatchPoint` objects via existing `Repo.List`.
*   `test_mode` (boolean): Filter by Test Mode status. Behavior depends on Actor type (see below).

**Actor-Specific Test Mode Data Isolation**:

The List endpoint enforces strict data isolation based on the Actor type to ensure API Keys
cannot access data outside their environment scope:

1.  **API Key Authentication** (`Actor.Type == ActorTypeAPIKey`):
    *   The handler **MUST** overwrite `filter.TestMode` with `Actor.IsTestMode`.
    *   API Keys are strictly scoped to their environment (Live or Test).
    *   The `test_mode` query parameter is **ignored** for API Key requests.
    *   A Live API Key can only see Live WatchPoints; a Test API Key can only see Test WatchPoints.

2.  **Dashboard Session Authentication** (`Actor.Type == ActorTypeUser`):
    *   The handler **MAY** accept the `test_mode` query parameter (boolean) to populate `filter.TestMode`.
    *   If `test_mode=true`: Filter to Test Mode WatchPoints only.
    *   If `test_mode=false` or omitted: Filter to Live Mode WatchPoints (default).
    *   This allows Dashboard users to toggle views between Live and Test environments.

**Logic**:
1.  **Parse Mode**: Check `?mode=` query parameter. Default to `"full"` if not specified.
2.  **Apply Test Mode Filter**:
    *   If `Actor.Type == ActorTypeAPIKey`: Set `filter.TestMode = Actor.IsTestMode` (ignore query param).
    *   If `Actor.Type == ActorTypeUser`: Parse `?test_mode=` query parameter, default to `false`.
3.  **Branch**:
    *   If `mode=summary`: Call `Repo.ListSummaries(ctx, orgID, params)`.
    *   If `mode=full`: Call `Repo.List(ctx, orgID, params)`.
4.  **Return**: 200 OK with appropriate list response type.

### 6.14 Rotate Channel Secret (`POST /{id}/channels/{channel_id}/rotate-secret`)
Enables zero-downtime secret rotation for webhook channels using the Dual-Validity pattern.

**Request**: Empty body or optional `{ "secret": "custom_secret" }` (if omitted, auto-generate).

**Logic**:
1.  **Extract**: `id` (WatchPoint ID), `channel_id` (Channel UUID) from URL, `orgID` from context.
2.  **Validate**: Ensure channel exists and is type `webhook`.
3.  **Atomic Update**: Call `Repo.UpdateChannelConfig(ctx, wpID, channelID, func(cfg) {
        cfg["previous_secret"] = cfg["secret"]
        cfg["secret"] = generateOrUseProvided()
        cfg["secret_rotated_at"] = time.Now()
        return cfg
    })`.
4.  **Audit**: Emit `watchpoint.channel.secret_rotated`.
5.  **Return**: 200 OK with redacted channel config (secret shown as `[REDACTED]`).

**Dual-Validity Behavior**:
The `previous_secret` field remains valid for a grace period (default 24h) to allow
clients to update their verification logic. The Webhook Worker signs payloads with both
`v1=` (current) and `v1_old=` (previous) signatures during this period.

---

## 7. Validation & Errors

The handler maps internal errors to standard API error codes (see `01-foundation-types.md`).

### Domain Errors

| Error | Code String | Condition |
|---|---|---|
| `ErrInvalidTimeWindow` | `validation_time_window_invalid` | Start > End, or historic without flag |
| `ErrLimitReached` | `limit_watchpoints_exceeded` | Plan limit hit |
| `ErrBulkPartial` | `bulk_partial_failure` | Some items in batch failed |
| `ErrAlreadyPaused` | `conflict_already_paused` | Pause called on paused item |
| `ErrCONUSWarning` | N/A (Metadata Warning) | Nowcast requested outside CONUS |

### Validation Rules
*   **Location**: Lat/Lon bounds checks.
*   **Conditions**: Max 10 per WatchPoint. Must be validated against `types.StandardVariables`. The handler checks that the `Variable` exists in the map and the `Threshold` falls within `Range`.
*   **Modes**: Mutually exclusive `TimeWindow` and `MonitorConfig`.

---

## 8. Flow Coverage

| Flow ID | Description | Implementation Method |
|---|---|---|
| `WPLC-001` | Create WatchPoint (Event) | `Create` |
| `WPLC-002` | Create WatchPoint (Monitor) | `Create` |
| `WPLC-003` | Update WatchPoint | `Update` |
| `WPLC-005` | Pause WatchPoint | `Pause` |
| `WPLC-006` | Resume WatchPoint | `Resume` (triggers eval) |
| `WPLC-008` | Delete WatchPoint | `Delete` |
| `WPLC-010` | Clone WatchPoint | `Clone` |
| `WPLC-011` | Bulk Import | `BulkCreate` |
| `BULK-002` | Bulk Pause | `BulkPause` |
| `BULK-003` | Bulk Resume | `BulkResume` |
| `BULK-004` | Bulk Delete | `BulkDelete` |
| `BULK-005` | Bulk Tag Update | `BulkTagUpdate` |
| `BULK-006` | Bulk Clone | `BulkClone` |
| `INFO-001` | Notification History | `GetNotificationHistory` |
| `INFO-008` | Get WatchPoint Detail | `Get` |
| `DASH-008` | Dashboard Optimization | `GetStats`, `List` (summary mode) |
| `HOOK-001` | Secret Rotation | `RotateChannelSecret` |
| `HOOK-002` | Deprecation Warnings | `Get` (deprecation check) |
| `BILL-008` | Limit Check | `UsageEnforcer.CheckLimit` |
| `OBS-011` | Audit Logging | `AuditLogger.Log` |