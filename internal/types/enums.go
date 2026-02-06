package types

// Status represents the lifecycle state of a WatchPoint.
type Status string

const (
	StatusActive   Status = "active"
	StatusPaused   Status = "paused"
	StatusArchived Status = "archived"
)

// ConditionLogic determines how multiple conditions are combined.
type ConditionLogic string

const (
	LogicAny ConditionLogic = "ANY"
	LogicAll ConditionLogic = "ALL"
)

// ConditionOperator defines comparison operators for condition evaluation.
type ConditionOperator string

const (
	OpGreaterThan   ConditionOperator = ">"
	OpGreaterThanEq ConditionOperator = ">="
	OpLessThan      ConditionOperator = "<"
	OpLessThanEq    ConditionOperator = "<="
	OpEqual         ConditionOperator = "=="
	OpNotEqual      ConditionOperator = "!="
	OpBetween       ConditionOperator = "between"
)

// ForecastType identifies the forecast model used.
type ForecastType string

const (
	ForecastMediumRange ForecastType = "medium_range"
	ForecastNowcast     ForecastType = "nowcast"
)

// UserRole defines authorization levels within an organization.
type UserRole string

const (
	RoleOwner  UserRole = "owner"
	RoleAdmin  UserRole = "admin"
	RoleMember UserRole = "member"
)

// UserStatus represents the account lifecycle state of a user.
type UserStatus string

const (
	UserStatusActive  UserStatus = "active"
	UserStatusInvited UserStatus = "invited"
)

// PausedReason describes why a WatchPoint was paused.
type PausedReason string

const (
	PausedReasonUser               PausedReason = "user_action"
	PausedReasonBillingDelinquency PausedReason = "billing_delinquency"
	PausedReasonSystem             PausedReason = "system_maintenance"
)

// PlanTier identifies the billing plan for an organization.
type PlanTier string

const (
	PlanFree       PlanTier = "free"
	PlanStarter    PlanTier = "starter"
	PlanPro        PlanTier = "pro"
	PlanBusiness   PlanTier = "business"
	PlanEnterprise PlanTier = "enterprise"
)

// EventType identifies the kind of notification event.
type EventType string

const (
	EventThresholdCrossed EventType = "threshold_crossed"
	EventThresholdCleared EventType = "threshold_cleared"
	EventForecastChanged  EventType = "forecast_changed"
	EventImminentAlert    EventType = "imminent_alert"
	EventDigest           EventType = "monitor_digest"
	EventSystemAlert      EventType = "system_alert"
	EventBillingWarning   EventType = "billing_warning"
	EventBillingReceipt   EventType = "billing_receipt"
)

// UrgencyLevel determines notification priority and delivery behavior.
type UrgencyLevel string

const (
	UrgencyRoutine  UrgencyLevel = "routine"
	UrgencyWatch    UrgencyLevel = "watch"
	UrgencyWarning  UrgencyLevel = "warning"
	UrgencyCritical UrgencyLevel = "critical"
)

// ChannelType identifies a notification delivery channel.
type ChannelType string

const (
	ChannelEmail   ChannelType = "email"
	ChannelWebhook ChannelType = "webhook"
)

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

// Hysteresis constants prevent alert flapping.
// Logic: Clear Trigger when Current < Threshold - (Threshold * Factor) - Absolute
const (
	HysteresisFactorDefault = 0.10 // 10% buffer
	HysteresisTemp          = 1.0  // 1 degree C absolute buffer
	HysteresisPrecipProb    = 5.0  // 5% absolute buffer
)

// DeliveryStatus enumerates all valid states for a notification delivery attempt.
// These values MUST match the CHECK constraint in notification_deliveries table.
type DeliveryStatus string

const (
	DeliveryStatusPending  DeliveryStatus = "pending"
	DeliveryStatusSent     DeliveryStatus = "sent"
	DeliveryStatusFailed   DeliveryStatus = "failed"
	DeliveryStatusBounced  DeliveryStatus = "bounced"
	DeliveryStatusRetrying DeliveryStatus = "retrying"
	DeliveryStatusSkipped  DeliveryStatus = "skipped"
	DeliveryStatusDeferred DeliveryStatus = "deferred"
)

// SubscriptionStatus represents the state of a billing subscription.
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

// ResourceType identifies a billable resource.
type ResourceType string

const (
	ResourceWatchPoints ResourceType = "watchpoints"
	ResourceAPICalls    ResourceType = "api_calls"
)

// ResetFrequency defines how often a usage counter resets.
type ResetFrequency string

const (
	ResetDaily   ResetFrequency = "daily"
	ResetMonthly ResetFrequency = "monthly"
	ResetNever   ResetFrequency = "never"
)

// TimeGranularity defines the granularity for usage data aggregation.
type TimeGranularity string

const (
	GranularityDaily   TimeGranularity = "daily"
	GranularityMonthly TimeGranularity = "monthly"
)

// RunPodTaskType defines the type of task for the RunPod worker.
// The worker routes to separate internal handlers based on this value.
type RunPodTaskType string

const (
	RunPodTaskInference    RunPodTaskType = "inference"
	RunPodTaskVerification RunPodTaskType = "verification"
	RunPodTaskCalibration  RunPodTaskType = "calibration"
)

// EvalAction determines the worker logic for processing a message.
type EvalAction string

const (
	// EvalActionEvaluate is the default action - evaluate conditions against forecast data.
	EvalActionEvaluate EvalAction = "evaluate"
	// EvalActionGenerateSummary uses the worker's scientific libraries to compute
	// post-event accuracy reports (Predicted vs Actual) upon WatchPoint archival.
	EvalActionGenerateSummary EvalAction = "generate_summary"
)
