package types

import (
	"encoding/json"
	"time"
)

// Location represents a geographic coordinate with an optional display name.
type Location struct {
	Lat         float64 `json:"lat" db:"location_lat"`
	Lon         float64 `json:"lon" db:"location_lon"`
	DisplayName string  `json:"display_name,omitempty" db:"location_display_name"`
}

// LocationIdent is used for batch operations (e.g., forecast queries) where
// inputs need to be correlated with outputs via a client-provided ID.
type LocationIdent struct {
	ID  string  `json:"id" validate:"required,max=50"`
	Lat float64 `json:"lat" validate:"required,latitude"`
	Lon float64 `json:"lon" validate:"required,longitude"`
}

// WatchPoint is the core domain entity representing a weather monitoring configuration.
type WatchPoint struct {
	ID             string `json:"id" db:"id"`
	OrganizationID string `json:"organization_id" db:"organization_id"`

	// Core Identity
	Name     string   `json:"name" db:"name"`
	Location Location `json:"location" db:"-"`
	Timezone string   `json:"timezone" db:"timezone"`
	TileID   string   `json:"tile_id" db:"tile_id"`

	// Modes (Mutually Exclusive)
	TimeWindow    *TimeWindow    `json:"time_window,omitempty" db:"time_window_start,time_window_end"`
	MonitorConfig *MonitorConfig `json:"monitor_config,omitempty" db:"monitor_config"`

	// Logic
	Conditions     Conditions     `json:"conditions" db:"conditions"`
	ConditionLogic ConditionLogic `json:"condition_logic" db:"condition_logic"`

	// Delivery
	Channels          ChannelList  `json:"channels" db:"channels"`
	TemplateSet       string       `json:"template_set" db:"template_set"`
	NotificationPrefs *Preferences `json:"preferences,omitempty" db:"preferences"`

	// Meta
	Status         Status   `json:"status" db:"status"`
	TestMode       bool     `json:"test_mode" db:"test_mode"`
	Tags           []string `json:"tags" db:"tags"`
	ConfigVersion  int      `json:"-" db:"config_version"`
	Source         string   `json:"source,omitempty" db:"source"`

	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at" db:"updated_at"`
	ArchivedAt     *time.Time `json:"archived_at,omitempty" db:"archived_at"`
	ArchivedReason string     `json:"archived_reason,omitempty" db:"archived_reason"`

	// Hydrated Fields (not in DB table)
	CurrentForecast *ForecastSnapshot `json:"current_forecast,omitempty" db:"-"`
}

// Validate implements the Validator interface for WatchPoint.
func (w *WatchPoint) Validate() error {
	// Validation rules are implemented in validation.go
	return nil
}

// WatchPointSummary is a lightweight DTO for dashboard list views.
// Excludes heavy JSONB fields (conditions, channels) for performance.
// Includes joined runtime state from watchpoint_evaluation_state table.
type WatchPointSummary struct {
	ID             string   `json:"id" db:"id"`
	OrganizationID string   `json:"organization_id" db:"organization_id"`
	Name           string   `json:"name" db:"name"`
	Location       Location `json:"location" db:"-"`
	Timezone       string   `json:"timezone" db:"timezone"`
	Status         Status   `json:"status" db:"status"`
	TestMode       bool     `json:"test_mode" db:"test_mode"`
	Tags           []string `json:"tags" db:"tags"`

	// Joined from watchpoint_evaluation_state (nullable if no state exists yet)
	LastEvaluatedAt   *time.Time `json:"last_evaluated_at,omitempty" db:"last_evaluated_at"`
	TriggerStatus     *bool      `json:"trigger_status,omitempty" db:"previous_trigger_state"`
	ActiveAlertsCount int        `json:"active_alerts_count" db:"active_alerts_count"`

	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// WatchPointStats contains aggregated counts for dashboard summary cards.
type WatchPointStats struct {
	Active    int `json:"active"`
	Error     int `json:"error"`
	Triggered int `json:"triggered"`
	Paused    int `json:"paused"`
	Total     int `json:"total"`
}

// Organization represents a billable entity that owns users and WatchPoints.
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

// User represents a human user within an organization.
type User struct {
	ID             string     `json:"id" db:"id"`
	OrganizationID string     `json:"organization_id" db:"organization_id"`
	Email          string     `json:"email" db:"email"`
	Name           string     `json:"name,omitempty" db:"name"`
	PasswordHash   string     `json:"-" db:"password_hash"`
	Role           UserRole   `json:"role" db:"role"`
	Status         UserStatus `json:"status" db:"status"`

	// OAuth fields (from 02-foundation-db.md Section 6.5)
	AuthProvider   string `json:"auth_provider,omitempty" db:"auth_provider"`
	AuthProviderID string `json:"-" db:"auth_provider_id"`

	// Invite fields (from 02-foundation-db.md Section 6.5)
	InviteTokenHash string     `json:"-" db:"invite_token_hash"`
	InviteExpiresAt *time.Time `json:"-" db:"invite_expires_at"`

	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty" db:"last_login_at"`
	DeletedAt   *time.Time `json:"-" db:"deleted_at"`
}

// APIKey represents an API key for programmatic access.
type APIKey struct {
	ID        string     `json:"id" db:"id"`
	KeyPrefix string     `json:"key_prefix" db:"key_prefix"`
	Name      string     `json:"name" db:"name"`
	Scopes    []string   `json:"scopes" db:"scopes"`
	Source    string     `json:"source,omitempty" db:"source"`
	ExpiresAt *time.Time `json:"expires_at" db:"expires_at"`
	RevokedAt *time.Time `json:"-" db:"revoked_at"`
}

// Session represents an authenticated user session.
type Session struct {
	ID             string    `json:"id" db:"id"`
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
	EventType     string    `db:"event_type"`
	Identifier    string    `db:"identifier"`
	IPAddress     string    `db:"ip_address"`
	AttemptedAt   time.Time `db:"attempted_at"`
	Success       bool      `db:"success"`
	FailureReason string    `db:"failure_reason"`
}

// ForecastRun represents a single execution of a forecast model.
type ForecastRun struct {
	ID                  string       `json:"id" db:"id"`
	Model               ForecastType `json:"model" db:"model"`
	RunTimestamp        time.Time    `json:"run_timestamp" db:"run_timestamp"`
	SourceDataTimestamp time.Time    `json:"source_data_timestamp" db:"source_data_timestamp"`
	StoragePath         string       `json:"storage_path" db:"storage_path"`
	Status              string       `json:"status" db:"status"`
	ExternalID          string       `json:"external_id,omitempty" db:"external_id"`
	RetryCount          int          `json:"retry_count" db:"retry_count"`
	FailureReason       string       `json:"failure_reason,omitempty" db:"failure_reason"`
	DurationMS          int          `json:"duration_ms" db:"inference_duration_ms"`
	CreatedAt           time.Time    `json:"created_at" db:"created_at"`
}

// CalibrationCoefficients for Nowcast probability derivation.
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

// InferencePayload defines the JSON contract sent to the RunPod worker.
type InferencePayload struct {
	TaskType          RunPodTaskType       `json:"task_type"`
	Model             ForecastType         `json:"model"`
	RunTimestamp      time.Time            `json:"run_timestamp"`
	OutputDestination string               `json:"output_destination"`
	InputConfig       InferenceInputConfig `json:"input_config"`
	Options           InferenceOptions     `json:"options"`
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

// InferenceInputConfig defines the input parameters for an inference task.
type InferenceInputConfig struct {
	Calibration          map[string]CalibrationCoefficients `json:"calibration,omitempty"`
	SourcePaths          []string                           `json:"gfs_source_paths,omitempty"`
	VerificationWindow   *VerificationWindow                `json:"verification_window,omitempty"`
	CalibrationTimeRange *CalibrationTimeRange              `json:"calibration_time_range,omitempty"`
}

// InferenceOptions controls inference execution behavior.
type InferenceOptions struct {
	ForceRebuild  bool `json:"force_rebuild"`
	MockInference bool `json:"mock_inference"`
}

// EvaluationState tracks the evaluation history and state for a WatchPoint.
type EvaluationState struct {
	WatchPointID  string           `db:"watchpoint_id"`
	ConfigVersion int              `db:"config_version"`

	// Evaluation Logic
	LastEvaluatedAt time.Time        `db:"last_evaluated_at"`
	LastResult      EvaluationResult `db:"last_evaluation_result"`
	Triggered       bool             `db:"previous_trigger_state"`
	EscalationLevel int              `db:"escalation_level"`

	// Notification History & Ordering
	LastNotifiedAt time.Time `db:"last_notified_at"`
	EventSequence  int64     `db:"event_sequence"`
	SeenThreatHashes []string `db:"seen_threat_hashes"`

	// Digest State
	LastDigestContent json.RawMessage `db:"last_digest_content"`
	LastDigestSentAt  *time.Time      `db:"last_digest_sent_at"`
}

// Notification represents a notification event record.
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

// NotificationDelivery tracks a single delivery attempt for a notification.
type NotificationDelivery struct {
	ID             string      `db:"id"`
	NotificationID string      `db:"notification_id"`
	ChannelType    ChannelType `db:"channel_type"`
	Status         string      `db:"status"`
	AttemptCount   int         `db:"attempt_count"`
	LastAttemptAt  time.Time   `db:"last_attempt_at"`
	FailureReason  string      `db:"failure_reason"`
	ProviderMsgID  string      `db:"provider_message_id"`
}

// DeliveryResult tracks the outcome of a notification attempt.
type DeliveryResult struct {
	ProviderMessageID string
	Status            string
	FailureReason     string
	Retryable         bool
}

// OAuthProfile is the normalized user data from an external provider.
type OAuthProfile struct {
	Provider      string
	ProviderID    string
	Email         string
	Name          string
	AvatarURL     string
	EmailVerified bool
}

// SendInput defines the contract for email transmission.
type SendInput struct {
	To           string
	From         SenderIdentity
	TemplateID   string
	TemplateData map[string]interface{}
	ReferenceID  string
}

// SenderIdentity defines the sender for outgoing emails.
type SenderIdentity struct {
	Name    string
	Address string
}

// VerificationResult stores forecast accuracy metrics.
type VerificationResult struct {
	ID            int64     `json:"id" db:"id"`
	ForecastRunID string    `json:"forecast_run_id" db:"forecast_run_id"`
	LocationID    string    `json:"location_id" db:"location_id"`
	MetricType    string    `json:"metric_type" db:"metric_type"`
	Variable      string    `json:"variable" db:"variable"`
	Value         float64   `json:"value" db:"value"`
	ComputedAt    time.Time `json:"computed_at" db:"computed_at"`
}

// VerificationMetric represents aggregated verification metrics for dashboard display.
type VerificationMetric struct {
	Model      ForecastType `json:"model"`
	Variable   string       `json:"variable"`
	MetricType string       `json:"metric_type"`
	Value      float64      `json:"value"`
	Timestamp  time.Time    `json:"timestamp"`
}

// JobRun tracks scheduled job execution history.
type JobRun struct {
	ID         int64           `json:"id" db:"id"`
	JobType    string          `json:"job_type" db:"job_type"`
	StartedAt  time.Time       `json:"started_at" db:"started_at"`
	FinishedAt *time.Time      `json:"finished_at" db:"finished_at"`
	Status     string          `json:"status" db:"status"`
	ItemsCount int             `json:"items_count" db:"items_count"`
	Error      string          `json:"error,omitempty" db:"error"`
	Metadata   json.RawMessage `json:"metadata" db:"metadata"`
}

// NotificationPayload defines the schema stored in the payload JSONB column.
// This is the CRITICAL contract between Eval Worker and Notification Workers.
type NotificationPayload struct {
	NotificationID string           `json:"notification_id"`
	WatchPointID   string           `json:"watchpoint_id"`
	WatchPointName string           `json:"watchpoint_name"`
	Timezone       string           `json:"timezone"`
	EventType      EventType        `json:"event_type"`
	TriggeredAt    time.Time        `json:"triggered_at"`
	Location       LocationSnapshot `json:"location"`
	TimeWindow     *TimeWindow      `json:"time_window,omitempty"`
	Forecast       ForecastSnapshot `json:"forecast_snapshot"`
	Conditions     []ConditionResult `json:"conditions_evaluated"`
	Urgency        UrgencyLevel     `json:"urgency"`
	SourceModel    ForecastType     `json:"source_model"`
	Ordering       OrderingMetadata `json:"ordering"`
}

// OrderingMetadata provides sequencing information for notification deduplication.
type OrderingMetadata struct {
	EventSequence     int64     `json:"event_sequence"`
	ForecastTimestamp time.Time `json:"forecast_timestamp"`
	EvalTimestamp     time.Time `json:"eval_timestamp"`
}

// LocationSnapshot captures a location at a point in time for notification payloads.
type LocationSnapshot struct {
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	DisplayName string  `json:"display_name"`
}

// ConditionResult captures the evaluation outcome of a single condition.
type ConditionResult struct {
	Variable      string            `json:"variable"`
	Operator      ConditionOperator `json:"operator"`
	Threshold     []float64         `json:"threshold"`
	ActualValue   float64           `json:"actual_value"`
	PreviousValue *float64          `json:"previous_value,omitempty"`
	Matched       bool              `json:"matched"`
}

// MonitorSummary captures the aggregation of a rolling window for digests.
// CONTRACT: Defines the strict schema for the last_forecast_summary JSONB column.
type MonitorSummary struct {
	WindowStart      time.Time          `json:"window_start"`
	WindowEnd        time.Time          `json:"window_end"`
	MaxValues        map[string]float64 `json:"max_values"`
	TriggeredPeriods []TimeRange        `json:"triggered_periods"`
}

// TimeRange defines a start/end time pair.
type TimeRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// SubscriptionDetails abstracts the provider's subscription object.
type SubscriptionDetails struct {
	Plan               PlanTier           `json:"plan"`
	Status             SubscriptionStatus `json:"status"`
	CurrentPeriodStart time.Time          `json:"current_period_start"`
	CurrentPeriodEnd   time.Time          `json:"current_period_end"`
	CancelAtPeriodEnd  bool               `json:"cancel_at_period_end"`
	PaymentMethod      *PaymentMethodInfo `json:"payment_method,omitempty"`
}

// PaymentMethodInfo contains payment method details.
type PaymentMethodInfo struct {
	Type     string `json:"type"`
	Last4    string `json:"last4"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
}

// Invoice represents a billing statement.
type Invoice struct {
	ID          string     `json:"id"`
	AmountCents int64      `json:"amount_cents"`
	Status      string     `json:"status"`
	PeriodStart time.Time  `json:"period_start"`
	PeriodEnd   time.Time  `json:"period_end"`
	PDFURL      string     `json:"pdf_url"`
	PaidAt      *time.Time `json:"paid_at,omitempty"`
}

// ListInvoicesParams defines filtering for invoice retrieval.
type ListInvoicesParams struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
}

// UsageSnapshot combines strict limits with actual consumption.
type UsageSnapshot struct {
	ResourceUsage map[ResourceType]int         `json:"resource_usage"`
	LimitDetails  map[ResourceType]LimitDetail `json:"limit_details"`
}

// LimitDetail describes a single resource limit with current usage.
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

// DeliverySummary captures delivery status per channel for history views.
type DeliverySummary struct {
	Channel string     `json:"channel"`
	Status  string     `json:"status"`
	SentAt  *time.Time `json:"sent_at,omitempty"`
}

// NotificationHistoryItem represents a notification in history listings.
type NotificationHistoryItem struct {
	ID                  string            `json:"id"`
	EventType           EventType         `json:"event_type"`
	SentAt              time.Time         `json:"sent_at"`
	ForecastSnapshot    ForecastSnapshot  `json:"forecast_snapshot"`
	TriggeredConditions []Condition       `json:"triggered_conditions"`
	Channels            []DeliverySummary `json:"channels"`
}

// NotificationFilter defines filtering parameters for notification history queries.
type NotificationFilter struct {
	OrganizationID string         `json:"organization_id"`
	WatchPointID   string         `json:"watchpoint_id,omitempty"`
	EventTypes     []EventType    `json:"event_types,omitempty"`
	Urgency        []UrgencyLevel `json:"urgency,omitempty"`
	Pagination     PageInfo       `json:"pagination"`
}

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

// EventMetadata carries optional correlation and tracing data.
type EventMetadata struct {
	CorrelationID string `json:"correlation_id,omitempty"`
	TraceID       string `json:"trace_id,omitempty"`
}

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
