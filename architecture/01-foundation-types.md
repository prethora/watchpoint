# 01 - Foundation Types

> **Purpose**: Defines the core domain entities, interfaces, error handling patterns, and cross-cutting concerns (logging, validation, context) used throughout the WatchPoint platform.
> **Package**: `package types`
> **Dependencies**: None (Root Document)

---

## Table of Contents

1. [Overview & ID Conventions](#1-overview--id-conventions)
2. [Domain Entities](#2-domain-entities)
3. [Value Objects & Configuration](#3-value-objects--configuration)
4. [Enums & Constants](#4-enums--constants)
5. [Validation & Security](#5-validation--security)
6. [Core Interfaces](#6-core-interfaces)
7. [Error Handling](#7-error-handling)
8. [Context & Actor Pattern](#8-context--actor-pattern)
9. [Utilities & Patterns](#9-utilities--patterns)

---

## 1. Overview & ID Conventions

This document defines the Go structs mapping to the database schema. It uses pointer fields for optional/mode-specific configurations and strict typing for JSONB columns.

### ID Generation Strategy
IDs are 32-character strings consisting of a **prefix** and a **UUID/KSUID**.

| Entity | Prefix | Example |
|---|---|---|
| Organization | `org_` | `org_8x7k2mNqPl` |
| User | `user_` | `user_9y8j3kLrMz` |
| WatchPoint | `wp_` | `wp_1a2b3c4d5e` |
| Notification | `notif_` | `notif_5f6g7h8i9j` |
| API Key | `key_` | `key_3d4e5f6g7h` |
| Session | `sess_` | `sess_a8f3k2m9p1` |

---

## 2. Domain Entities

### 2.1 WatchPoint

```go
type WatchPoint struct {
    ID                  string         `json:"id" db:"id"`
    OrganizationID      string         `json:"organization_id" db:"organization_id"`
    
    // Core Identity
    Name                string         `json:"name" db:"name"`
    Location            Location       `json:"location" db:"-"` // Flattens to location_lat/_lon
    Timezone            string         `json:"timezone" db:"timezone"`
    TileID              string         `json:"tile_id" db:"tile_id"` // Generated column, read-only
    
    // Modes (Mutually Exclusive)
    TimeWindow          *TimeWindow    `json:"time_window,omitempty" db:"time_window_start,time_window_end"` 
    MonitorConfig       *MonitorConfig `json:"monitor_config,omitempty" db:"monitor_config"`
    
    // Logic
    Conditions          Conditions     `json:"conditions" db:"conditions"`
    ConditionLogic      ConditionLogic `json:"condition_logic" db:"condition_logic"`
    
    // Delivery
    Channels            ChannelList    `json:"channels" db:"channels"`
    TemplateSet         string         `json:"template_set" db:"template_set"`
    NotificationPrefs   *Preferences   `json:"preferences,omitempty" db:"preferences"`
    
    // Meta
    Status              Status         `json:"status" db:"status"`
    TestMode            bool           `json:"test_mode" db:"test_mode"`
    Tags                []string       `json:"tags" db:"tags"`
    ConfigVersion       int            `json:"-" db:"config_version"`
    Source              string         `json:"source,omitempty" db:"source"`
    
    CreatedAt           time.Time      `json:"created_at" db:"created_at"`
    UpdatedAt           time.Time      `json:"updated_at" db:"updated_at"`
    ArchivedAt          *time.Time     `json:"archived_at,omitempty" db:"archived_at"`
    ArchivedReason      string         `json:"archived_reason,omitempty" db:"archived_reason"`
    
    // Hydrated Fields (not in DB table)
    CurrentForecast     *ForecastSnapshot `json:"current_forecast,omitempty" db:"-"`
}

// Validate implements the Validator interface
func (w *WatchPoint) Validate() error {
    // Implements rules from Section 5
    return nil 
}

type Location struct {
    Lat         float64 `json:"lat" db:"location_lat"`
    Lon         float64 `json:"lon" db:"location_lon"`
    DisplayName string  `json:"display_name,omitempty" db:"location_display_name"`
}

// LocationIdent is used for batch operations (e.g., forecast queries) where
// inputs need to be correlated with outputs via a client-provided ID.
type LocationIdent struct {
    ID  string  `json:"id" validate:"required,max=50"`
    Lat float64 `json:"lat" validate:"required,latitude"` // Custom validator "latitude"
    Lon float64 `json:"lon" validate:"required,longitude"` // Custom validator "longitude"
}

// WatchPointSummary is a lightweight DTO for dashboard list views.
// Excludes heavy JSONB fields (conditions, channels) for performance.
// Includes joined runtime state from watchpoint_evaluation_state table.
type WatchPointSummary struct {
    ID             string     `json:"id" db:"id"`
    OrganizationID string     `json:"organization_id" db:"organization_id"`
    Name           string     `json:"name" db:"name"`
    Location       Location   `json:"location" db:"-"`
    Timezone       string     `json:"timezone" db:"timezone"`
    Status         Status     `json:"status" db:"status"`
    TestMode       bool       `json:"test_mode" db:"test_mode"`
    Tags           []string   `json:"tags" db:"tags"`

    // Joined from watchpoint_evaluation_state (nullable if no state exists yet)
    LastEvaluatedAt   *time.Time    `json:"last_evaluated_at,omitempty" db:"last_evaluated_at"`
    TriggerStatus     *bool         `json:"trigger_status,omitempty" db:"previous_trigger_state"`
    ActiveAlertsCount int           `json:"active_alerts_count" db:"active_alerts_count"`

    CreatedAt time.Time  `json:"created_at" db:"created_at"`
    UpdatedAt time.Time  `json:"updated_at" db:"updated_at"`
}

// WatchPointStats contains aggregated counts for dashboard summary cards.
type WatchPointStats struct {
    Active    int `json:"active"`
    Error     int `json:"error"`
    Triggered int `json:"triggered"`
    Paused    int `json:"paused"`
    Total     int `json:"total"`
}
```

### 2.2 Organization & Users

```go
type Organization struct {
    ID                      string                  `json:"id" db:"id"`
    Name                    string                  `json:"name" db:"name"`
    BillingEmail            string                  `json:"billing_email" db:"billing_email"`
    Plan                    PlanTier                `json:"plan" db:"plan"`
    PlanLimits              PlanLimits              `json:"plan_limits" db:"plan_limits"`
    StripeCustomerID        string                  `json:"-" db:"stripe_customer_id"`
    NotificationPreferences NotificationPreferences `json:"notification_preferences" db:"notification_preferences"`
    CreatedAt               time.Time               `json:"created_at" db:"created_at"`
    UpdatedAt               time.Time               `json:"updated_at" db:"updated_at"`
    DeletedAt               *time.Time              `json:"-" db:"deleted_at"`
}

type User struct {
    ID             string    `json:"id" db:"id"`
    OrganizationID string    `json:"organization_id" db:"organization_id"`
    Email          string    `json:"email" db:"email"`
    PasswordHash   string    `json:"-" db:"password_hash"`
    Role           UserRole  `json:"role" db:"role"`
    CreatedAt      time.Time `json:"created_at" db:"created_at"`
    LastLoginAt    time.Time `json:"last_login_at" db:"last_login_at"`
}

type APIKey struct {
    ID        string     `json:"id" db:"id"`
    KeyPrefix string     `json:"key_prefix" db:"key_prefix"` // "sk_live_abc..."
    Name      string     `json:"name" db:"name"`
    Scopes    []string   `json:"scopes" db:"scopes"`
    Source    string     `json:"source,omitempty" db:"source"` // Vertical app identity (e.g., "wedding_app"). Used for usage attribution.
    ExpiresAt *time.Time `json:"expires_at" db:"expires_at"`
    RevokedAt *time.Time `json:"-" db:"revoked_at"`
}
```

### 2.3 Auth & System State

```go
type Session struct {
    ID             string    `json:"id" db:"id"` // "sess_..."
    UserID         string    `json:"user_id" db:"user_id"`
    OrganizationID string    `json:"organization_id" db:"organization_id"`
    CSRFToken      string    `json:"-" db:"csrf_token"`
    UserAgent      string    `json:"user_agent" db:"user_agent"`
    IPAddress      string    `json:"ip_address" db:"ip_address"`
    ExpiresAt      time.Time `json:"expires_at" db:"expires_at"`
    LastActivityAt time.Time `json:"last_activity_at" db:"last_activity_at"`
    CreatedAt      time.Time `json:"created_at" db:"created_at"`
}

// SecurityEvent represents a unified security event for abuse tracking.
// Replaces the legacy LoginAttempt to support both login and API auth events.
type SecurityEvent struct {
    ID            int64     `db:"id"`
    EventType     string    `db:"event_type"`     // 'login', 'api_auth'
    Identifier    string    `db:"identifier"`     // e.g., email (nullable)
    IPAddress     string    `db:"ip_address"`
    AttemptedAt   time.Time `db:"attempted_at"`
    Success       bool      `db:"success"`
    FailureReason string    `db:"failure_reason"`
}

type ForecastRun struct {
    ID                  string       `json:"id" db:"id"`
    Model               ForecastType `json:"model" db:"model"`
    RunTimestamp        time.Time    `json:"run_timestamp" db:"run_timestamp"`
    SourceDataTimestamp time.Time    `json:"source_data_timestamp" db:"source_data_timestamp"`
    StoragePath         string       `json:"storage_path" db:"storage_path"`
    Status              string       `json:"status" db:"status"` // running, complete, failed
    ExternalID          string       `json:"external_id,omitempty" db:"external_id"`
    RetryCount          int          `json:"retry_count" db:"retry_count"`
    FailureReason       string       `json:"failure_reason,omitempty" db:"failure_reason"`
    DurationMS          int          `json:"duration_ms" db:"inference_duration_ms"`
    CreatedAt           time.Time    `json:"created_at" db:"created_at"`
}

// CalibrationCoefficients for Nowcast probability derivation (Flow: OBS-006)
type CalibrationCoefficients struct {
    LocationID    string    `json:"location_id" db:"location_id"`
    HighThreshold float64   `json:"high_threshold" db:"high_threshold"`
    HighSlope     float64   `json:"high_slope" db:"high_slope"`
    HighBase      float64   `json:"high_base" db:"high_base"`
    MidThreshold  float64   `json:"mid_threshold" db:"mid_threshold"`
    MidSlope      float64   `json:"mid_slope" db:"mid_slope"`
    MidBase       float64   `json:"mid_base" db:"mid_base"`
    LowBase       float64   `json:"low_base" db:"low_base"`
    UpdatedAt     time.Time `json:"updated_at" db:"updated_at"`
}

// RunPodTaskType defines the type of task for the RunPod worker.
// The worker routes to separate internal handlers based on this value.
type RunPodTaskType string
const (
    RunPodTaskInference    RunPodTaskType = "inference"
    RunPodTaskVerification RunPodTaskType = "verification"
    RunPodTaskCalibration  RunPodTaskType = "calibration"
)

// InferencePayload defines the JSON contract sent to the RunPod worker.
type InferencePayload struct {
    TaskType         RunPodTaskType         `json:"task_type"` // Routes to inference.py, verification.py, or calibration.py
    Model            ForecastType           `json:"model"`
    RunTimestamp     time.Time              `json:"run_timestamp"`
    OutputDestination string                `json:"output_destination"`
    InputConfig      InferenceInputConfig   `json:"input_config"`
    Options          InferenceOptions       `json:"options"`
}

// VerificationWindow defines the time range for verification tasks.
type VerificationWindow struct {
    Start time.Time `json:"start"`
    End   time.Time `json:"end"`
}

// CalibrationTimeRange defines the historical data range for calibration regression.
type CalibrationTimeRange struct {
    Start time.Time `json:"start"`
    End   time.Time `json:"end"`
}

type InferenceInputConfig struct {
    // For Nowcast: Map of TileID -> Coefficients
    Calibration map[string]CalibrationCoefficients `json:"calibration,omitempty"`
    // For MediumRange: List of upstream S3 keys/prefixes
    SourcePaths []string                           `json:"gfs_source_paths,omitempty"`
    // For Verification tasks: Time window to compare forecast vs observed data
    VerificationWindow *VerificationWindow         `json:"verification_window,omitempty"`
    // For Calibration tasks: Historical time range for regression analysis
    CalibrationTimeRange *CalibrationTimeRange     `json:"calibration_time_range,omitempty"`
}

type InferenceOptions struct {
    ForceRebuild  bool `json:"force_rebuild"`
    MockInference bool `json:"mock_inference"`
}
```

### 2.4 Evaluation State & Notifications

```go
type EvaluationState struct {
    WatchPointID      string           `db:"watchpoint_id"`
    ConfigVersion     int              `db:"config_version"`
    
    // Evaluation Logic
    LastEvaluatedAt   time.Time        `db:"last_evaluated_at"`
    LastResult        EvaluationResult `db:"last_evaluation_result"` // JSONB
    Triggered         bool             `db:"previous_trigger_state"`
    EscalationLevel   int              `db:"escalation_level"`
    
    // Notification History & Ordering
    LastNotifiedAt    time.Time        `db:"last_notified_at"`
    EventSequence     int64            `db:"event_sequence"`
    SeenThreatHashes  []string         `db:"seen_threat_hashes"` // For monitor mode dedup
    
    // Digest State
    LastDigestContent json.RawMessage  `db:"last_digest_content"` // JSONB
    LastDigestSentAt  *time.Time       `db:"last_digest_sent_at"`
}

type Notification struct {
    ID             string                 `json:"id" db:"id"`
    WatchPointID   string                 `json:"watchpoint_id" db:"watchpoint_id"`
    OrganizationID string                 `json:"organization_id" db:"organization_id"`
    EventType      EventType              `json:"event_type" db:"event_type"`
    Urgency        UrgencyLevel           `json:"urgency" db:"urgency"`
    Payload        map[string]interface{} `json:"payload" db:"payload"`
    TestMode       bool                   `json:"test_mode" db:"test_mode"`
    CreatedAt      time.Time              `json:"created_at" db:"created_at"`
}

type NotificationDelivery struct {
    ID             string      `db:"id"`
    NotificationID string      `db:"notification_id"`
    ChannelType    ChannelType `db:"channel_type"`
    Status         string      `db:"status"` // pending, sent, failed, bounced
    AttemptCount   int         `db:"attempt_count"`
    LastAttemptAt  time.Time   `db:"last_attempt_at"`
    FailureReason  string      `db:"failure_reason"`
    ProviderMsgID  string      `db:"provider_message_id"`
}

// DeliveryResult tracks the outcome of a notification attempt
type DeliveryResult struct {
    ProviderMessageID string
    Status            string // "delivered", "failed", "bounced"
    FailureReason     string
    Retryable         bool
}
```

---

## 3. Value Objects & Configuration

### 3.1 Conditions

```go
type Condition struct {
    Variable  string            `json:"variable"` // e.g., "temperature_c"
    Operator  ConditionOperator `json:"operator"` // e.g., ">", "between"
    
    // Threshold is a slice to support single values ([50]) or ranges ([15, 25])
    Threshold []float64         `json:"threshold"` 
    
    Unit      string            `json:"unit"`
}

type Conditions []Condition

// Implements sql.Scanner/Valuer for JSONB
func (c *Conditions) Scan(value interface{}) error { /* ... */ }
func (c Conditions) Value() (driver.Value, error) { /* ... */ }
```

### 3.2 Channels (Secrets Redacted)

```go
type Channel struct {
    // ID is a stable UUID auto-generated by the API on creation/update.
    // Used to uniquely identify channels within the JSONB array for atomic updates
    // (e.g., secret rotation via UpdateChannelConfig). Format: UUID v4.
    ID      string         `json:"id"`
    Type    ChannelType    `json:"type"`
    // Config keys:
    // - "url", "secret" (standard)
    // - "platform_override": (Optional string) Forces specific formatter (e.g., "slack", "teams")
    //   bypassing auto-detection.
    Config  map[string]any `json:"config"`
    Enabled bool           `json:"enabled"`
}

type ChannelList []Channel

// Custom MarshalJSON to redact secrets prevents leakage in API responses
func (c Channel) MarshalJSON() ([]byte, error) {
    type Alias Channel
    safe := Alias(c)
    if safe.Config != nil {
        safe.Config = make(map[string]any)
        for k, v := range c.Config {
            if isSensitive(k) {
                safe.Config[k] = "[REDACTED]"
            } else {
                safe.Config[k] = v
            }
        }
    }
    return json.Marshal(safe)
}

func isSensitive(key string) bool {
    k := strings.ToLower(key)
    return k == "secret" || k == "password" || strings.Contains(k, "key")
}
```

### 3.3 Forecast & Evaluation

```go
type EvaluationResult struct {
    Timestamp           time.Time        `json:"timestamp"`
    Triggered           bool             `json:"triggered"`
    MatchedConditions   []Condition      `json:"matched_conditions,omitempty"`
    ForecastSnapshot    ForecastSnapshot `json:"forecast_snapshot"`
    ModelUsed           string           `json:"model_used"`
}

type ForecastSnapshot struct {
    PrecipitationProb float64 `json:"precipitation_probability"`
    PrecipitationMM   float64 `json:"precipitation_mm"`
    TemperatureC      float64 `json:"temperature_c"`
    WindSpeedKmh      float64 `json:"wind_speed_kmh"`
    Humidity          float64 `json:"humidity_percent"`
}
```

### 3.4 Config Objects

```go
type PlanLimits struct {
    MaxWatchPoints   int  `json:"watchpoints_max"`
    MaxAPICallsDaily int  `json:"api_calls_daily_max"`
    AllowNowcast     bool `json:"allow_nowcast"`
}

type TimeWindow struct {
    Start time.Time `json:"start"`
    End   time.Time `json:"end"`
}

type MonitorConfig struct {
    WindowHours int       `json:"window_hours" validate:"required,min=6,max=168"`
    ActiveHours [][2]int  `json:"active_hours"` // [[6,18]]
    ActiveDays  []int     `json:"active_days"`  // [1,2,3,4,5]
}

type NotificationPreferences struct {
    QuietHours  *QuietHoursConfig `json:"quiet_hours,omitempty"`
    Digest      *DigestConfig     `json:"digest,omitempty"`
}

type QuietHoursConfig struct {
    Enabled  bool          `json:"enabled"`
    Schedule []QuietPeriod `json:"schedule"`
    Timezone string        `json:"timezone"`
}

type QuietPeriod struct {
    Days  []string `json:"days"` // ["mon", "tue"]
    Start string   `json:"start"` // "22:00"
    End   string   `json:"end"`   // "07:00"
}

type DigestConfig struct {
    Enabled      bool   `json:"enabled"`
    Frequency    string `json:"frequency"` // "daily"
    DeliveryTime string `json:"delivery_time"`

    // New Fields
    SendEmpty    bool   `json:"send_empty"`   // If false, skip digest if no changes/triggers. Default false.
    TemplateSet  string `json:"template_set"` // Optional override (e.g. "executive_summary"). Default "digest_default".
}

type Preferences struct {
    NotifyOnClear          bool `json:"notify_on_clear"`
    NotifyOnForecastChange bool `json:"notify_on_forecast_change"`
}
```

---

## 4. Enums & Constants

### 4.1 WatchPoint Logic
```go
type Status string
const (
    StatusActive   Status = "active"
    StatusPaused   Status = "paused"
    StatusArchived Status = "archived"
)

type ConditionLogic string
const (
    LogicAny ConditionLogic = "ANY"
    LogicAll ConditionLogic = "ALL"
)

type ConditionOperator string
const (
    OpGreaterThan      ConditionOperator = ">"
    OpGreaterThanEq    ConditionOperator = ">="
    OpLessThan         ConditionOperator = "<"
    OpLessThanEq       ConditionOperator = "<="
    OpEqual            ConditionOperator = "=="
    OpNotEqual         ConditionOperator = "!="
    OpBetween          ConditionOperator = "between"
)
```

### 4.2 Forecasts
```go
type ForecastType string
const (
    ForecastMediumRange ForecastType = "medium_range"
    ForecastNowcast     ForecastType = "nowcast"
)
```

### 4.5 Hysteresis Constants
```go
// Hysteresis constants prevent alert flapping
// Logic: Clear Trigger when Current < Threshold - (Threshold * Factor) - Absolute
const (
    HysteresisFactorDefault = 0.10 // 10% buffer
    HysteresisTemp          = 1.0  // 1°C absolute buffer
    HysteresisPrecipProb    = 5.0  // 5% absolute buffer
)
```

### 4.3 Users & Plans
```go
type UserRole string
const (
    RoleOwner  UserRole = "owner"
    RoleAdmin  UserRole = "admin"
    RoleMember UserRole = "member"
)

type PausedReason string
const (
    PausedReasonUser               PausedReason = "user_action"
    PausedReasonBillingDelinquency PausedReason = "billing_delinquency"
    PausedReasonSystem             PausedReason = "system_maintenance"
)

type PlanTier string
const (
    PlanFree       PlanTier = "free"
    PlanStarter    PlanTier = "starter"
    PlanPro        PlanTier = "pro"
    PlanBusiness   PlanTier = "business"
    PlanEnterprise PlanTier = "enterprise"
)
```

### 4.5 API Scopes
```go
// AllScopes defines the complete set of valid API key scopes.
// Used by validators to check requested scopes during key creation.
var AllScopes = []string{
    "watchpoints:read",
    "watchpoints:write",
    "forecasts:read",
    "notifications:read",
    "account:read",
    "account:write",
}
```

### 4.4 Events & Urgency
```go
type EventType string
const (
    EventThresholdCrossed EventType = "threshold_crossed"
    EventThresholdCleared EventType = "threshold_cleared"
    EventForecastChanged  EventType = "forecast_changed"
    EventImminentAlert    EventType = "imminent_alert"
    EventDigest           EventType = "monitor_digest"
    EventSystemAlert      EventType = "system_alert"       // Internal system alerts (e.g., bounce notifications to owner)
    EventBillingWarning   EventType = "billing_warning"    // Triggered when usage exceeds 80% of plan limits
    EventBillingReceipt   EventType = "billing_receipt"    // Triggered when a payment succeeds to send a receipt
)

type UrgencyLevel string
const (
    UrgencyRoutine  UrgencyLevel = "routine"
    UrgencyWatch    UrgencyLevel = "watch"
    UrgencyWarning  UrgencyLevel = "warning"
    UrgencyCritical UrgencyLevel = "critical"
)
```

---

## 5. Validation & Security

### 5.1 Rules

| Constraint | Value | Description |
|---|---|---|
| `MinLat` / `MaxLat` | -90.0 / 90.0 | Geographical latitude bounds |
| `MinLon` / `MaxLon` | -180.0 / 180.0 | Geographical longitude bounds |
| `MaxConditions` | 10 | Max conditions per WatchPoint |
| `MaxNameLength` | 200 | Max chars for WatchPoint/Org names |
| `ConusMinLat` | 24.0 | CONUS bounding box (for Nowcast) |
| `ConusMaxLat` | 50.0 | CONUS bounding box |
| `ConusMinLon` | -125.0 | CONUS bounding box |
| `ConusMaxLon` | -66.0 | CONUS bounding box |
| `MaxMonitorWindow` | 168 | Max hours for monitor window (7 days) |
| `MinMonitorWindow` | 6 | Min hours for monitor window |

### 5.2 Validators

```go
// Validator is implemented by entities to self-validate
type Validator interface {
    Validate() error
}

// SSRFValidator checks if a webhook URL is safe to call
// See Addendum §2.5 / Tech Stack v3.3
type SSRFValidator func(url string) error

func IsCONUS(lat, lon float64) bool {
    return lat >= 24.0 && lat <= 50.0 && lon >= -125.0 && lon <= -66.0
}
```

### 5.3 Security Service

```go
// SecurityService provides unified security event tracking and IP-based blocking.
// Centralizes abuse prevention logic for login attempts, API auth failures, and
// proactive rate limiting across the platform.
type SecurityService interface {
    // RecordAttempt logs a security event (login, api_auth, etc.) for tracking.
    // Called by auth handlers on both successful and failed authentication attempts.
    RecordAttempt(ctx context.Context, eventType string, identifier string, ip string, success bool, reason string) error

    // IsIPBlocked checks if an IP address should be blocked based on recent
    // failed attempts across all event types. Returns true if the IP has exceeded
    // the configured threshold (e.g., 100 failures in 15 minutes).
    IsIPBlocked(ctx context.Context, ip string) bool

    // IsIdentifierBlocked checks if a specific identifier (e.g., email) should
    // be blocked based on recent failed attempts. Returns true if the identifier
    // has exceeded the configured threshold (e.g., 5 failures in 15 minutes).
    IsIdentifierBlocked(ctx context.Context, identifier string) bool
}
```

---

## 6. Core Interfaces

### 6.1 Rate Limiting

```go
type RateLimitInfo struct {
    Limit     int
    Remaining int
    ResetAt   time.Time
}

type RateLimiter interface {
    // Allow checks if the actor can perform the action.
    Allow(ctx context.Context, actorID string, action string) (RateLimitInfo, bool, error)
}
```

### 6.2 Repositories & Transactions

```go
type RepositoryRegistry interface {
    WatchPoints() WatchPointRepository
    Organizations() OrganizationRepository
    Users() UserRepository
    Notifications() NotificationRepository
}

type TransactionManager interface {
    RunInTx(ctx context.Context, fn func(ctx context.Context, repos RepositoryRegistry) error) error
}
```

### 6.3 Notification Channels

```go
type ChannelType string
const (
    ChannelEmail   ChannelType = "email"
    ChannelWebhook ChannelType = "webhook"
)

type NotificationChannel interface {
    Type() ChannelType
    Format(n *Notification) ([]byte, error)
    Deliver(ctx context.Context, payload []byte, destination string) (*DeliveryResult, error)
    ShouldRetry(err error) bool
}
```

### 6.4 Forecast & Evaluation

```go
// ForecastSource defines how we retrieve forecast data (abstracts Zarr/S3)
type ForecastSource interface {
    GetPoint(ctx context.Context, lat, lon float64, t time.Time) (*ForecastSnapshot, error)
    GetTile(ctx context.Context, tileID string, t time.Time) (map[string]*ForecastSnapshot, error)
}

// Evaluator defines the contract for checking conditions against forecasts
type Evaluator interface {
    Evaluate(wp *WatchPoint, forecast *ForecastSnapshot) (EvaluationResult, error)
}
```

### 6.5 Time Abstraction

```go
type Clock interface {
    Now() time.Time
}

type RealClock struct{}
func (RealClock) Now() time.Time { return time.Now().UTC() }
```

---

## 7. Error Handling

```go
type ErrorCode string

const (
    // Validation (400)
    ErrValidationInvalidLat       ErrorCode = "validation_invalid_latitude"
    ErrValidationInvalidLon       ErrorCode = "validation_invalid_longitude"
    ErrValidationInvalidTimezone  ErrorCode = "validation_invalid_timezone"
    ErrValidationInvalidConditions ErrorCode = "validation_invalid_conditions"
    ErrValidationThresholdRange   ErrorCode = "validation_threshold_out_of_range"
    ErrValidationTimeWindow       ErrorCode = "validation_time_window_invalid"
    ErrValidationMissingField     ErrorCode = "validation_missing_required_field"
    ErrValidationInvalidEmail     ErrorCode = "validation_invalid_email"
    ErrValidationInvalidWebhook   ErrorCode = "validation_invalid_webhook_url"
    ErrValidationMaxConditions    ErrorCode = "validation_too_many_conditions"
    
    // Auth (401)
    ErrAuthTokenMissing           ErrorCode = "auth_token_missing"
    ErrAuthTokenInvalid           ErrorCode = "auth_token_invalid"
    ErrAuthTokenExpired           ErrorCode = "auth_token_expired"
    ErrAuthSessionExpired         ErrorCode = "auth_session_expired"

    // Permission (403)
    ErrPermissionScope            ErrorCode = "permission_scope_insufficient"
    ErrPermissionOrgMismatch      ErrorCode = "permission_organization_mismatch"
    ErrPermissionRole             ErrorCode = "permission_role_insufficient"
    
    // Limits (403/429)
    ErrLimitWatchpoints           ErrorCode = "limit_watchpoints_exceeded"
    ErrLimitAPICalls              ErrorCode = "limit_api_rate_exceeded"
    ErrRateLimit                  ErrorCode = "rate_limit_exceeded"

    // Not Found (404)
    ErrNotFoundWatchpoint         ErrorCode = "not_found_watchpoint"
    ErrNotFoundOrg                ErrorCode = "not_found_organization"
    ErrNotFoundUser               ErrorCode = "not_found_user"
    ErrNotFoundAPIKey             ErrorCode = "not_found_api_key"
    
    // Conflict (409)
    ErrConflictPaused             ErrorCode = "conflict_already_paused"
    ErrConflictActive             ErrorCode = "conflict_already_active"
    ErrConflictEmail              ErrorCode = "conflict_email_exists"
    ErrConflictIdempotency        ErrorCode = "conflict_idempotency_mismatch"

    // Internal/Upstream (500/502)
    ErrInternalDB                 ErrorCode = "internal_database_error"
    ErrInternalUnexpected         ErrorCode = "internal_unexpected_error"
    ErrUpstreamStripe             ErrorCode = "upstream_stripe_unavailable"
)

type AppError struct {
    Code    ErrorCode
    Message string
    Err     error
    Details map[string]any
}

func (e *AppError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
```

---

## 8. Context & Actor Pattern

```go
type ActorType string
const (
    ActorTypeUser   ActorType = "user"
    ActorTypeAPIKey ActorType = "api_key"
    ActorTypeSystem ActorType = "system"
)

type Actor struct {
    ID             string
    Type           ActorType
    OrganizationID string
    IsTestMode     bool
    Source         string    // Origin of the request (e.g., "wedding_app", "dashboard"). Populated from APIKey.Source or defaults to "default".
}

// Context Keys
type contextKey string
const (
    actorKey     contextKey = "actor"
    requestIDKey contextKey = "request_id"
    loggerKey    contextKey = "logger"
)

// Helpers
func WithActor(ctx context.Context, actor Actor) context.Context {
    return context.WithValue(ctx, actorKey, actor)
}

func GetActor(ctx context.Context) (Actor, bool) {
    actor, ok := ctx.Value(actorKey).(Actor)
    return actor, ok
}

func GetRequestID(ctx context.Context) string {
    id, _ := ctx.Value(requestIDKey).(string)
    return id
}

func IsTestKey(key string) bool {
    return strings.HasPrefix(key, "sk_test_")
}
```

---

## 9. Utilities & Patterns

### 9.1 Pagination

```go
type PageInfo struct {
    HasMore    bool   `json:"has_more"`
    NextCursor string `json:"next_cursor,omitempty"`
    TotalItems *int   `json:"total_items,omitempty"`
}

type ListResponse[T any] struct {
    Data     []T      `json:"data"`
    PageInfo PageInfo `json:"pagination"`
}
```

### 9.2 Response Metadata

```go
// ResponseMeta contains non-blocking metadata returned with API responses.
// Used to convey warnings, deprecation notices, and other advisory information
// that should not block the request but should be surfaced to the client.
type ResponseMeta struct {
    // Warnings contains non-blocking advisory messages (e.g., deprecation notices).
    // Example: "Teams Connectors are retiring December 2025. Migrate to Power Automate Workflows."
    Warnings []string `json:"warnings,omitempty"`

    // Pagination info (optional, used by list endpoints)
    Pagination *PageInfo `json:"pagination,omitempty"`
}
```

### 9.3 Bulk Operation Types

These types are hoisted from handler-level definitions to enable Repository layer access without circular dependencies.

```go
// BulkFilter defines criteria for selecting WatchPoints in bulk operations.
// Used by batch endpoints (Pause, Resume, Delete, TagUpdate) when operating
// on sets of resources rather than explicit ID lists.
type BulkFilter struct {
    Status []Status   `json:"status,omitempty"`   // Filter by WatchPoint status (active, paused, archived)
    Tags   []string   `json:"tags,omitempty"`     // Filter by tag membership (AND semantics)
}
```

### 9.4 Audit Log

```go
type AuditEvent struct {
    ID           string          `json:"id"`
    Actor        Actor           `json:"actor"`
    Action       string          `json:"action"` // e.g., "watchpoint.create"
    ResourceID   string          `json:"resource_id"`
    ResourceType string          `json:"resource_type"`
    OldValue     json.RawMessage `json:"old_value,omitempty"`
    NewValue     json.RawMessage `json:"new_value,omitempty"`
    Timestamp    time.Time       `json:"timestamp"`
}

// Standard Audit Action Strings
// Handlers MUST use these action strings for consistency.
const (
    // Single-resource actions
    AuditActionWatchPointCreated  = "watchpoint.created"
    AuditActionWatchPointUpdated  = "watchpoint.updated"
    AuditActionWatchPointPaused   = "watchpoint.paused"
    AuditActionWatchPointResumed  = "watchpoint.resumed"
    AuditActionWatchPointDeleted  = "watchpoint.deleted"
    AuditActionWatchPointCloned   = "watchpoint.cloned"

    // Bulk actions - emit AGGREGATED events containing count/filter in metadata
    // Do NOT emit individual events per item in bulk operations.
    AuditActionWatchPointBulkCreated = "watchpoint.bulk_created"
    AuditActionWatchPointBulkPaused  = "watchpoint.bulk_paused"
    AuditActionWatchPointBulkResumed = "watchpoint.bulk_resumed"
    AuditActionWatchPointBulkDeleted = "watchpoint.bulk_deleted"
    AuditActionWatchPointBulkCloned  = "watchpoint.bulk_cloned"
)

// AuditQueryFilters defines parameters for querying audit log entries.
type AuditQueryFilters struct {
    OrganizationID string    `json:"organization_id"`
    ActorID        string    `json:"actor_id,omitempty"`
    ResourceType   string    `json:"resource_type,omitempty"`
    StartTime      time.Time `json:"start_time,omitempty"`
    EndTime        time.Time `json:"end_time,omitempty"`
    Limit          int       `json:"limit,omitempty"`
    Cursor         string    `json:"cursor,omitempty"`
}
```

### 9.5 Structured Logging

```go
type Logger interface {
    Info(msg string, args ...any)
    Error(msg string, args ...any)
    Warn(msg string, args ...any)
    With(args ...any) Logger
}

// LoggerFromContext retrieves the logger enriched with RequestID and ActorID
func LoggerFromContext(ctx context.Context) Logger {
    // Implementation to pull from context or return default
    if l, ok := ctx.Value(loggerKey).(Logger); ok {
        return l
    }
    // Return default slog wrapper
    return nil
}
```

---

## 10. Hoisted Types (Consolidated)

This section contains types hoisted from sub-documents to establish a Single Source of Truth. All downstream documents must reference `types.X` instead of defining locally.

### 10.1 Notification Types (from `08a-notification-core.md`)

```go
// DeliveryStatus enumerates all valid states for a notification delivery attempt.
// These values MUST match the CHECK constraint in notification_deliveries table.
type DeliveryStatus string
const (
    DeliveryStatusPending  DeliveryStatus = "pending"   // Awaiting worker pickup
    DeliveryStatusSent     DeliveryStatus = "sent"      // Successfully delivered
    DeliveryStatusFailed   DeliveryStatus = "failed"    // Permanent failure (max retries exceeded)
    DeliveryStatusBounced  DeliveryStatus = "bounced"   // Recipient rejected (email invalid, webhook 410)
    DeliveryStatusRetrying DeliveryStatus = "retrying"  // Transient failure, will retry
    DeliveryStatusSkipped  DeliveryStatus = "skipped"   // Suppressed (TestMode, policy decision)
    DeliveryStatusDeferred DeliveryStatus = "deferred"  // Quiet Hours - will resume at ResumeAt time
)

// NotificationPayload defines the schema stored in the `payload` JSONB column.
// This is the CRITICAL contract between Eval Worker and Notification Workers.
type NotificationPayload struct {
    NotificationID string           `json:"notification_id"`
    WatchPointID   string           `json:"watchpoint_id"`
    WatchPointName string           `json:"watchpoint_name"`
    Timezone       string           `json:"timezone"` // Injected from WatchPoint config

    EventType      EventType        `json:"event_type"`
    TriggeredAt    time.Time        `json:"triggered_at"`

    // Context
    Location       LocationSnapshot `json:"location"`
    TimeWindow     *TimeWindow      `json:"time_window,omitempty"`

    // Data
    Forecast       ForecastSnapshot  `json:"forecast_snapshot"`
    Conditions     []ConditionResult `json:"conditions_evaluated"`

    // Metadata
    Urgency        UrgencyLevel      `json:"urgency"`
    SourceModel    ForecastType      `json:"source_model"`
    Ordering       OrderingMetadata  `json:"ordering"`
}

type OrderingMetadata struct {
    EventSequence     int64     `json:"event_sequence"`
    ForecastTimestamp time.Time `json:"forecast_timestamp"`
    EvalTimestamp     time.Time `json:"eval_timestamp"`
}

type LocationSnapshot struct {
    Lat         float64 `json:"lat"`
    Lon         float64 `json:"lon"`
    DisplayName string  `json:"display_name"`
}

type ConditionResult struct {
    Variable      string            `json:"variable"`
    Operator      ConditionOperator `json:"operator"`
    Threshold     []float64         `json:"threshold"`
    ActualValue   float64           `json:"actual_value"`
    PreviousValue *float64          `json:"previous_value,omitempty"` // Context for clearance/escalation (e.g., "Wind dropped from 80km/h to 20km/h")
    Matched       bool              `json:"matched"`
}

// MonitorSummary captures the aggregation of a rolling window for digests.
// Replaces generic JSON maps to ensure type safety between Eval Worker and Digest Generator.
//
// CONTRACT: Defines the strict schema for the `last_forecast_summary` JSONB column.
// Acts as the data contract between the Python Eval Worker (writer) and Go Digest Generator (reader).
// Any changes to this struct require coordinated updates to both components.
type MonitorSummary struct {
    WindowStart      time.Time          `json:"window_start"`
    WindowEnd        time.Time          `json:"window_end"`
    MaxValues        map[string]float64 `json:"max_values"`        // e.g. "precip_prob": 80.0
    TriggeredPeriods []TimeRange        `json:"triggered_periods"` // Union of all violation ranges
}

type TimeRange struct {
    Start time.Time `json:"start"`
    End   time.Time `json:"end"`
}
```

### 10.2 Billing Types (from `05e-api-billing.md`)

```go
// SubscriptionDetails abstracts the provider's subscription object.
type SubscriptionDetails struct {
    Plan               PlanTier           `json:"plan"`
    Status             SubscriptionStatus `json:"status"`
    CurrentPeriodStart time.Time          `json:"current_period_start"`
    CurrentPeriodEnd   time.Time          `json:"current_period_end"`
    CancelAtPeriodEnd  bool               `json:"cancel_at_period_end"`
    PaymentMethod      *PaymentMethodInfo `json:"payment_method,omitempty"`
}

type PaymentMethodInfo struct {
    Type      string `json:"type"`       // "card"
    Last4     string `json:"last4"`      // "4242"
    ExpMonth  int    `json:"exp_month"`
    ExpYear   int    `json:"exp_year"`
}

// Invoice represents a billing statement.
type Invoice struct {
    ID          string     `json:"id"`
    AmountCents int64      `json:"amount_cents"`
    Status      string     `json:"status"` // "paid", "open", "void", "uncollectible"
    PeriodStart time.Time  `json:"period_start"`
    PeriodEnd   time.Time  `json:"period_end"`
    PDFURL      string     `json:"pdf_url"` // Direct Stripe URL
    PaidAt      *time.Time `json:"paid_at,omitempty"`
}

// ListInvoicesParams defines filtering for invoice retrieval.
type ListInvoicesParams struct {
    Limit  int    `json:"limit"`
    Cursor string `json:"cursor"` // Stripe pagination cursor
}

// UsageSnapshot combines strict limits with actual consumption.
type UsageSnapshot struct {
    ResourceUsage map[ResourceType]int         `json:"resource_usage"`
    LimitDetails  map[ResourceType]LimitDetail `json:"limit_details"`
}

type LimitDetail struct {
    Limit     int            `json:"limit"`
    Used      int            `json:"used"`
    ResetType ResetFrequency `json:"reset_type"`
    NextReset *time.Time     `json:"next_reset,omitempty"`
}

// UsageDataPoint represents a point in a usage graph.
type UsageDataPoint struct {
    Timestamp time.Time `json:"timestamp"`
    Value     int       `json:"value"`
}

// DailyUsageStat matches the DB aggregation row.
type DailyUsageStat struct {
    Date              time.Time
    APICalls          int
    ActiveWatchPoints int
    NotificationsSent int
}

// SubscriptionStatus enum
type SubscriptionStatus string
const (
    SubStatusActive            SubscriptionStatus = "active"
    SubStatusPastDue           SubscriptionStatus = "past_due"
    SubStatusCanceled          SubscriptionStatus = "canceled"
    SubStatusIncomplete        SubscriptionStatus = "incomplete"
    SubStatusIncompleteExpired SubscriptionStatus = "incomplete_expired"
    SubStatusTrialing          SubscriptionStatus = "trialing"
    SubStatusUnpaid            SubscriptionStatus = "unpaid"
)

// ResourceType constants
type ResourceType string
const (
    ResourceWatchPoints ResourceType = "watchpoints"
    ResourceAPICalls    ResourceType = "api_calls"
)

// ResetFrequency enum
type ResetFrequency string
const (
    ResetDaily   ResetFrequency = "daily"
    ResetMonthly ResetFrequency = "monthly"
    ResetNever   ResetFrequency = "never"
)

// TimeGranularity enum
type TimeGranularity string
const (
    GranularityDaily   TimeGranularity = "daily"
    GranularityMonthly TimeGranularity = "monthly"
)
```

### 10.3 Auth Types (from `05f-api-auth.md`)

```go
// OAuthProfile is the normalized user data from an external provider.
type OAuthProfile struct {
    Provider      string // "google" or "github"
    ProviderID    string
    Email         string
    Name          string
    AvatarURL     string
    EmailVerified bool   // Critical for security checks
}
```

### 10.4 Email Types (from `08b-email-worker.md`)

```go
// SendInput defines the contract for email transmission.
type SendInput struct {
    To           string
    From         SenderIdentity
    TemplateID   string
    TemplateData map[string]interface{}
    ReferenceID  string // Internal NotificationID for correlation
}

type SenderIdentity struct {
    Name    string // e.g. "WatchPoint Alerts" or "Wedding Weather"
    Address string // Fixed verified domain, e.g. "alerts@watchpoint.io"
}
```

### 10.5 Scheduled Job Types (from `09-scheduled-jobs.md`)

```go
// VerificationResult stores forecast accuracy metrics.
type VerificationResult struct {
    ID             int64     `json:"id" db:"id"`
    ForecastRunID  string    `json:"forecast_run_id" db:"forecast_run_id"`
    LocationID     string    `json:"location_id" db:"location_id"`
    MetricType     string    `json:"metric_type" db:"metric_type"` // 'rmse', 'bias', 'brier'
    Variable       string    `json:"variable" db:"variable"`       // 'temperature_c', etc.
    Value          float64   `json:"value" db:"value"`
    ComputedAt     time.Time `json:"computed_at" db:"computed_at"`
}

// VerificationMetric represents aggregated verification metrics for dashboard display.
// Used by the API to return pre-aggregated results (e.g., Average RMSE for TempC)
// computed via SQL-level aggregation on the verification_results table.
type VerificationMetric struct {
    Model      ForecastType `json:"model"`       // Forecast model type (medium_range, nowcast)
    Variable   string       `json:"variable"`    // Weather variable (temperature_c, etc.)
    MetricType string       `json:"metric_type"` // Aggregation type (rmse, bias, brier)
    Value      float64      `json:"value"`       // Aggregated metric value
    Timestamp  time.Time    `json:"timestamp"`   // Aggregation window end time
}

// JobRun tracks scheduled job execution history.
type JobRun struct {
    ID         int64           `json:"id" db:"id"`
    JobType    string          `json:"job_type" db:"job_type"`
    StartedAt  time.Time       `json:"started_at" db:"started_at"`
    FinishedAt *time.Time      `json:"finished_at" db:"finished_at"`
    Status     string          `json:"status" db:"status"` // "running", "success", "failed"
    ItemsCount int             `json:"items_count" db:"items_count"`
    Error      string          `json:"error,omitempty" db:"error"`
    Metadata   json.RawMessage `json:"metadata" db:"metadata"`
}
```

### 10.6 Notification History Types

These types support notification history retrieval at both WatchPoint and Organization scope.

```go
// DeliverySummary captures delivery status per channel for history views.
type DeliverySummary struct {
    Channel string     `json:"channel"`
    Status  string     `json:"status"`
    SentAt  *time.Time `json:"sent_at,omitempty"`
}

// NotificationHistoryItem represents a notification in history listings.
// Used by both WatchPoint-scoped and Organization-scoped history endpoints.
type NotificationHistoryItem struct {
    ID                  string            `json:"id"`
    EventType           EventType         `json:"event_type"`
    SentAt              time.Time         `json:"sent_at"` // Derived from created_at
    ForecastSnapshot    ForecastSnapshot  `json:"forecast_snapshot"`
    TriggeredConditions []Condition       `json:"triggered_conditions"`
    Channels            []DeliverySummary `json:"channels"` // Delivery status per channel
}

// NotificationFilter defines filtering parameters for notification history queries.
type NotificationFilter struct {
    OrganizationID string        `json:"organization_id"`
    WatchPointID   string        `json:"watchpoint_id,omitempty"` // Optional - if empty, returns org-wide
    EventTypes     []EventType   `json:"event_types,omitempty"`
    Urgency        []UrgencyLevel `json:"urgency,omitempty"`
    Pagination     PageInfo      `json:"pagination"`
}
```

### 10.7 Event Envelope & EvalMessage (from Design Addendum)

```go
// EventEnvelope is the standard wrapper for all internal events.
type EventEnvelope struct {
    EventID   string          `json:"event_id"`   // "evt_..." unique ID for deduplication
    EventType string          `json:"event_type"` // Dot-namespaced (e.g., "forecast.ready")
    Timestamp time.Time       `json:"timestamp"`  // ISO 8601 UTC
    Source    string          `json:"source"`     // Component name
    Version   string          `json:"version"`    // Schema version
    Payload   json.RawMessage `json:"payload"`
    Metadata  *EventMetadata  `json:"metadata,omitempty"`
}

type EventMetadata struct {
    CorrelationID string `json:"correlation_id,omitempty"`
    TraceID       string `json:"trace_id,omitempty"`
}

// EvalAction determines the worker logic for processing a message.
type EvalAction string
const (
    // EvalActionEvaluate is the default action - evaluate conditions against forecast data.
    EvalActionEvaluate       EvalAction = "evaluate"
    // EvalActionGenerateSummary uses the worker's scientific libraries to compute
    // post-event accuracy reports (Predicted vs Actual) upon WatchPoint archival.
    EvalActionGenerateSummary EvalAction = "generate_summary"
)

// EvalMessage is the SQS payload sent from Batcher to Eval Worker.
type EvalMessage struct {
    BatchID      string       `json:"batch_id"`
    TraceID      string       `json:"trace_id"`
    ForecastType ForecastType `json:"forecast_type"`
    RunTimestamp time.Time    `json:"run_timestamp"`
    TileID       string       `json:"tile_id"`
    Page         int          `json:"page"`
    PageSize     int          `json:"page_size"`
    TotalItems   int          `json:"total_items"`

    // Action determines the worker logic. Defaults to "evaluate".
    // Set to "generate_summary" to compute post-event accuracy reports.
    Action EvalAction `json:"action,omitempty"`

    // SpecificWatchPointIDs restricts evaluation to only these WatchPoint IDs.
    // If populated, the worker must filter execution to only these IDs.
    // Used for immediate Resume/Update triggers.
    SpecificWatchPointIDs []string `json:"specific_watchpoint_ids,omitempty"`
}
```

---

## 11. Variable Registry & Validation

This section defines the canonical rules for weather variables shared by the API (Write path) and Eval Worker (Read path). All components MUST validate against these ranges.

```go
// VariableMetadata defines the canonical rules for a weather variable.
type VariableMetadata struct {
    ID          string     `json:"id"`
    Unit        string     `json:"unit"`
    Range       [2]float64 `json:"valid_range"` // [min, max]
    Description string     `json:"description"`
}

// StandardVariables defines the authoritative constraints for the platform.
// All components MUST validate against these ranges.
var StandardVariables = map[string]VariableMetadata{
    "temperature_c":           {ID: "temperature_c", Unit: "celsius", Range: [2]float64{-60, 60}, Description: "Air temperature at 2m above ground level"},
    "precipitation_probability": {ID: "precipitation_probability", Unit: "percent", Range: [2]float64{0, 100}, Description: "Probability of precipitation"},
    "precipitation_mm":        {ID: "precipitation_mm", Unit: "mm", Range: [2]float64{0, 500}, Description: "Accumulated precipitation"},
    "wind_speed_kmh":          {ID: "wind_speed_kmh", Unit: "kmh", Range: [2]float64{0, 300}, Description: "Wind speed at 10m above ground level"},
    "humidity_percent":        {ID: "humidity_percent", Unit: "percent", Range: [2]float64{0, 100}, Description: "Relative humidity"},
    "cloud_cover_percent":     {ID: "cloud_cover_percent", Unit: "percent", Range: [2]float64{0, 100}, Description: "Cloud cover percentage"},
    "uv_index":                {ID: "uv_index", Unit: "index", Range: [2]float64{0, 15}, Description: "UV radiation index"},
}

// ValidateConditionThreshold checks if a threshold value is within the valid range for its variable.
func ValidateConditionThreshold(variable string, threshold float64) error {
    meta, ok := StandardVariables[variable]
    if !ok {
        return fmt.Errorf("%s: unknown variable '%s'", ErrCodeValidationInvalidConditions, variable)
    }
    if threshold < meta.Range[0] || threshold > meta.Range[1] {
        return fmt.Errorf("%s: threshold %.2f outside valid range [%.2f, %.2f] for %s",
            ErrCodeValidationThresholdRange, threshold, meta.Range[0], meta.Range[1], variable)
    }
    return nil
}
```

---

## 12. Standardized Constants

### 12.1 Complete Error Codes

All error codes used throughout the platform. Handlers MUST use these constants instead of hardcoded strings.

```go
const (
    // Validation (400)
    ErrCodeValidationInvalidLat       ErrorCode = "validation_invalid_latitude"
    ErrCodeValidationInvalidLon       ErrorCode = "validation_invalid_longitude"
    ErrCodeValidationInvalidTimezone  ErrorCode = "validation_invalid_timezone"
    ErrCodeValidationInvalidConditions ErrorCode = "validation_invalid_conditions"
    ErrCodeValidationThresholdRange   ErrorCode = "validation_threshold_out_of_range"
    ErrCodeValidationTimeWindow       ErrorCode = "validation_time_window_invalid"
    ErrCodeValidationMissingField     ErrorCode = "validation_missing_required_field"
    ErrCodeValidationInvalidEmail     ErrorCode = "validation_invalid_email"
    ErrCodeValidationInvalidWebhook   ErrorCode = "validation_invalid_webhook_url"
    ErrCodeValidationMaxConditions    ErrorCode = "validation_too_many_conditions"

    // Auth (401)
    ErrCodeAuthTokenMissing           ErrorCode = "auth_token_missing"
    ErrCodeAuthTokenInvalid           ErrorCode = "auth_token_invalid"
    ErrCodeAuthTokenExpired           ErrorCode = "auth_token_expired"
    ErrCodeAuthTokenRevoked           ErrorCode = "auth_token_revoked"
    ErrCodeAuthSessionExpired         ErrorCode = "auth_session_expired"

    // Permission (403)
    ErrCodePermissionScope            ErrorCode = "permission_scope_insufficient"
    ErrCodePermissionOrgMismatch      ErrorCode = "permission_organization_mismatch"
    ErrCodePermissionRole             ErrorCode = "permission_role_insufficient"

    // Limits (403/429)
    ErrCodeLimitWatchpoints           ErrorCode = "limit_watchpoints_exceeded"
    ErrCodeLimitAPICalls              ErrorCode = "limit_api_rate_exceeded"
    ErrCodeRateLimit                  ErrorCode = "rate_limit_exceeded"

    // Not Found (404)
    ErrCodeNotFoundWatchpoint         ErrorCode = "not_found_watchpoint"
    ErrCodeNotFoundOrg                ErrorCode = "not_found_organization"
    ErrCodeNotFoundUser               ErrorCode = "not_found_user"
    ErrCodeNotFoundAPIKey             ErrorCode = "not_found_api_key"
    ErrCodeNotFoundNotification       ErrorCode = "not_found_notification"

    // Conflict (409)
    ErrCodeConflictPaused             ErrorCode = "conflict_already_paused"
    ErrCodeConflictActive             ErrorCode = "conflict_already_active"
    ErrCodeConflictEmail              ErrorCode = "conflict_email_exists"
    ErrCodeConflictConcurrent         ErrorCode = "conflict_concurrent_modification"
    ErrCodeConflictIdempotency        ErrorCode = "conflict_idempotency_mismatch"

    // Internal/Upstream (500/502)
    ErrCodeInternalDB                 ErrorCode = "internal_database_error"
    ErrCodeInternalUnexpected         ErrorCode = "internal_unexpected_error"
    ErrCodeUpstreamStripe             ErrorCode = "upstream_stripe_unavailable"
    ErrCodeUpstreamEmailProvider      ErrorCode = "upstream_email_provider_unavailable"
    ErrCodeUpstreamForecast           ErrorCode = "upstream_forecast_unavailable"
    ErrCodeUpstreamRateLimited        ErrorCode = "upstream_rate_limited"

    // Payment-specific
    ErrCodePaymentDeclined            ErrorCode = "payment_declined"
    ErrCodeEmailBlocked               ErrorCode = "email_blocked"
)
```

### 12.2 Telemetry Metrics

Standard metric names and dimensions for CloudWatch. All components MUST use these constants.

```go
const (
    // Metric Names
    MetricForecastReady       = "ForecastReady"
    MetricEvaluationLag       = "EvaluationLag"       // Seconds behind real-time for forecast processing (Now - ForecastTimestamp)
    MetricNotificationFailure = "NotificationFailure"
    MetricBillingWarning      = "BillingWarning"
    MetricAPILatency          = "APILatency"
    MetricExternalAPIFailure  = "ExternalAPIFailure"
    MetricDeliveryAttempt     = "DeliveryAttempt"
    MetricDeliverySuccess     = "DeliverySuccess"     // Tracks successful notification deliveries (HOOK-005)
    MetricDeliveryFailed      = "DeliveryFailed"

    // Dimension Keys
    DimForecastType   = "ForecastType"
    DimQueue          = "Queue"
    DimChannel        = "Channel"
    DimOrgID          = "OrgID"
    DimEndpoint       = "Endpoint"
    DimProvider       = "Provider"
    DimEventType      = "EventType"

    // Metric Namespace
    MetricNamespace   = "WatchPoint"
)
```

### 12.3 Zarr Schema Variables

Canonical variable names for forecast data. The Python Eval Worker MUST use these exact keys.

```go
const (
    ZarrVarTemperatureC       = "temperature_c"
    ZarrVarPrecipitationMM    = "precipitation_mm"
    ZarrVarPrecipitationProb  = "precipitation_probability"
    ZarrVarWindSpeedKmh       = "wind_speed_kmh"
    ZarrVarHumidityPercent    = "humidity_percent"
    ZarrVarPressureHPa        = "pressure_hpa"
    ZarrVarCloudCoverPercent  = "cloud_cover_percent"
    ZarrVarUVIndex            = "uv_index"
)
```

---

## 13. Cross-Language Data Contract

This section defines the strict specification for data exchange between Go and Python components.

### 13.1 JSON Key Convention

**RULE**: All JSON keys MUST be `snake_case` to ensure compatibility across languages.

| Go Struct Tag | Python Pydantic Field | JSON Key |
|---|---|---|
| `json:"forecast_type"` | `forecast_type: str` | `forecast_type` |
| `json:"watchpoint_id"` | `watchpoint_id: str` | `watchpoint_id` |
| `json:"run_timestamp"` | `run_timestamp: datetime` | `run_timestamp` |

### 13.2 Pydantic Synchronization

**RULE**: Python Pydantic models in the Eval Worker (`07-eval-worker.md`) MUST be kept in sync with the Go structs defined in this document.

The following types have Python equivalents that must match:

| Go Type (this document) | Python Pydantic Model |
|---|---|
| `EvalMessage` | `EvalMessage` |
| `NotificationPayload` | `NotificationEvent.payload` |
| `ForecastSnapshot` | `dict` mapping to Zarr constants |
| `ConditionResult` | `ConditionResult` |

### 13.3 Metric Dimension Alignment

**RULE**: The Python Eval Worker MUST use the metric dimension keys defined in Section 11.2 verbatim (e.g., `"ForecastType"`, NOT `"forecast_type"`) when emitting CloudWatch metrics.

---

## 14. Validation Logic Specifications

### 14.1 SSRF Protection (CIDR Blocks)

The `SSRFValidator` function MUST block the following IP ranges:

```go
var SSRFBlockedCIDRs = []string{
    "127.0.0.0/8",        // Localhost
    "10.0.0.0/8",         // Private Class A
    "172.16.0.0/12",      // Private Class B
    "192.168.0.0/16",     // Private Class C
    "169.254.0.0/16",     // Link-local (AWS Metadata!)
    "0.0.0.0/8",          // Current network
    "224.0.0.0/4",        // Multicast
    "240.0.0.0/4",        // Reserved
    "100.64.0.0/10",      // Shared Address Space (CGN)
    "198.18.0.0/15",      // Benchmark testing
    "fc00::/7",           // IPv6 private
    "fe80::/10",          // IPv6 link-local
    "::1/128",            // IPv6 localhost
}

// SSRFValidator returns an error if the URL resolves to a blocked IP.
// Implementations MUST resolve DNS and check against SSRFBlockedCIDRs.
```

### 14.2 TimeWindow Validation

```go
// ValidateTimeWindow ensures End > Start and applies business rules.
func ValidateTimeWindow(tw *TimeWindow) error {
    if tw == nil {
        return nil
    }
    if !tw.End.After(tw.Start) {
        return fmt.Errorf("%s: end must be after start", ErrCodeValidationTimeWindow)
    }
    // Additional: Maximum duration = 30 days
    if tw.End.Sub(tw.Start) > 30*24*time.Hour {
        return fmt.Errorf("%s: maximum window is 30 days", ErrCodeValidationTimeWindow)
    }
    return nil
}
```

### 14.3 Webhook URL Validation

```go
// ValidateWebhookURL checks that a URL is safe for webhook delivery.
func ValidateWebhookURL(urlStr string) error {
    parsed, err := url.Parse(urlStr)
    if err != nil {
        return fmt.Errorf("%s: invalid URL", ErrCodeValidationInvalidWebhook)
    }
    if parsed.Scheme != "https" {
        return fmt.Errorf("%s: must use HTTPS", ErrCodeValidationInvalidWebhook)
    }
    // SSRF check is performed at delivery time, not validation
    return nil
}
```

### 14.4 Interpolation Specification

To ensure consistency between the Python Eval Worker and the Go API, all geographic point extractions MUST use **Bilinear Interpolation**. Implementations must calculate the weighted average of the four nearest grid points based on distance. Simple nearest-neighbor is prohibited.

### 14.5 Canary Metadata Standard

Canary WatchPoints are system-managed entities used for automated verification of the forecast pipeline.
The Verification Service identifies Canaries by inspecting WatchPoint metadata.

**Authoritative Schema** (stored in `watchpoints.metadata` JSONB):
```json
{
  "system_role": "canary",
  "canary_expect_trigger": true
}
```

| Field | Type | Description |
|---|---|---|
| `system_role` | string | Must be `"canary"` to identify as a Canary WatchPoint |
| `canary_expect_trigger` | boolean | `true` if the Canary is configured to trigger under normal conditions; `false` if it should remain dormant |

**Behavior**:
- Canary WatchPoints are excluded from billing and usage limits.
- The Verification Service uses `canary_expect_trigger` to validate pipeline health:
  - If `true` and no trigger occurred, emit alert `CanaryMissedTrigger`.
  - If `false` and trigger occurred, emit alert `CanaryFalseTrigger`.