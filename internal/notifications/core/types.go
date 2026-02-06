// Package core provides the shared notification infrastructure used by all
// delivery workers (email, webhook). It centralizes state management, policy
// enforcement, retry logic, and observability, ensuring consistency across
// notification channels.
//
// Architecture reference: 08a-notification-core.md
package core

import (
	"context"
	"errors"
	"time"

	"watchpoint/internal/types"
)

// PolicyDecision represents the outcome of a policy evaluation.
type PolicyDecision string

const (
	// PolicyDeliverImmediately indicates the notification should be sent now.
	PolicyDeliverImmediately PolicyDecision = "deliver"

	// PolicySuppress indicates the notification should be permanently suppressed.
	PolicySuppress PolicyDecision = "suppress"

	// PolicyDefer indicates the notification should be deferred to a later time.
	PolicyDefer PolicyDecision = "defer"
)

// PolicyResult contains the outcome and metadata from a policy evaluation.
type PolicyResult struct {
	Decision PolicyDecision
	Reason   string
	ResumeAt *time.Time // Set when Decision is PolicyDefer
}

// PolicyEngine determines whether a notification should be delivered now,
// deferred, or suppressed based on organizational preferences such as
// Quiet Hours, frequency caps, and urgency overrides.
type PolicyEngine interface {
	// Evaluate checks Quiet Hours, Frequency Caps, and Urgency overrides.
	//
	// Timezone Resolution: Quiet Hours evaluation uses the Organization's
	// QuietHoursConfig.Timezone to resolve the user's local time. This ensures
	// Quiet Hours respect user preferences regardless of WatchPoint location.
	//
	// Clearance Override: If EventType is threshold_cleared, the Policy Engine
	// bypasses the standard CooldownMinutes check. Clearance notifications are
	// time-sensitive and must always be delivered if enabled.
	Evaluate(ctx context.Context, n *types.Notification, org *types.Organization, prefs types.NotificationPreferences) (PolicyResult, error)
}

// DeliveryManager abstracts database state transitions for notification deliveries.
// It wraps the NotificationRepository to provide higher-level operations that
// enforce business rules around delivery state management.
type DeliveryManager interface {
	// EnsureDeliveryExists is idempotent. Uses INSERT ... ON CONFLICT DO NOTHING.
	// Returns the delivery ID, whether it was newly created, and any error.
	EnsureDeliveryExists(ctx context.Context, notifID string, chType types.ChannelType, idx int) (string, bool, error)

	// RecordAttempt logs that a worker is about to try sending.
	RecordAttempt(ctx context.Context, deliveryID string) error

	// MarkSuccess updates status to 'sent' and sets delivered_at.
	MarkSuccess(ctx context.Context, deliveryID string, providerMsgID string) error

	// MarkFailure updates status, increments attempts, calculates next_retry_at.
	MarkFailure(ctx context.Context, deliveryID string, reason string) (shouldRetry bool, err error)

	// MarkSkipped is used for TestMode or permanent policy suppression.
	MarkSkipped(ctx context.Context, deliveryID string, reason string) error

	// MarkDeferred sets status to 'deferred' and schedules resumption for Quiet Hours.
	// Used when PolicyEngine returns PolicyDefer. The notification will be re-queued
	// by the RequeueDeferredNotifications scheduled job once resumeAt time passes.
	MarkDeferred(ctx context.Context, deliveryID string, resumeAt time.Time) error

	// CheckAggregateFailure determines if all sibling deliveries for a notification
	// have failed. Returns true if count(deliveries where status != failed) == 0.
	CheckAggregateFailure(ctx context.Context, notificationID string) (allFailed bool, err error)

	// ResetNotificationState performs a compensatory transaction when all delivery
	// channels fail. Sets last_notified_at = NULL and last_notified_state = NULL
	// in watchpoint_evaluation_state. This ensures the Evaluation Engine perceives
	// the WatchPoint as "not notified" during the next cycle.
	ResetNotificationState(ctx context.Context, watchpointID string) error

	// CancelDeferred wraps Repo.CancelDeferredDeliveries. Purges any pending
	// deliveries with status='deferred' for this WatchPoint. Used when a WatchPoint
	// is Resumed to prevent flooding users with stale alerts.
	CancelDeferred(ctx context.Context, watchpointID string) error
}

// MetricResult categorizes a delivery outcome for metrics reporting.
type MetricResult string

const (
	MetricSuccess MetricResult = "success"
	MetricFailed  MetricResult = "failed"
	MetricSkipped MetricResult = "skipped"
)

// NotificationMetrics abstracts CloudWatch/telemetry operations for the
// notification system.
type NotificationMetrics interface {
	RecordDelivery(ctx context.Context, channel types.ChannelType, result MetricResult)
	RecordLatency(ctx context.Context, channel types.ChannelType, duration time.Duration)
	RecordQueueLag(ctx context.Context, lag time.Duration)
}

// RetryPolicy defines the exponential backoff parameters for delivery retries.
type RetryPolicy struct {
	MaxAttempts   int
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	BackoffFactor float64
}

// Standard retry policies for each channel type.
var (
	WebhookRetryPolicy = RetryPolicy{
		MaxAttempts:   3,
		BaseDelay:     1 * time.Second,
		MaxDelay:      30 * time.Second,
		BackoffFactor: 5.0,
	}
	EmailRetryPolicy = RetryPolicy{
		MaxAttempts:   3,
		BaseDelay:     1 * time.Second,
		MaxDelay:      10 * time.Second,
		BackoffFactor: 2.0,
	}
)

// CalculateNextRetry computes the delay before the next retry attempt using
// exponential backoff: delay = min(BaseDelay * BackoffFactor^attempt, MaxDelay).
func CalculateNextRetry(policy RetryPolicy, attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}

	delay := float64(policy.BaseDelay)
	for i := 0; i < attempt; i++ {
		delay *= policy.BackoffFactor
	}

	d := time.Duration(delay)
	if d > policy.MaxDelay {
		d = policy.MaxDelay
	}
	if d < 0 {
		// Guard against overflow
		d = policy.MaxDelay
	}

	return d
}

// ErrDigestEmpty is returned when SendEmpty=false and the digest has no content.
var ErrDigestEmpty = errors.New("digest empty and SendEmpty=false")
