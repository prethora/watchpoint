package core

import (
	"context"
	"fmt"
	"time"

	"watchpoint/internal/types"
)

// Compile-time assertion that DeliveryManagerImpl implements DeliveryManager.
var _ DeliveryManager = (*DeliveryManagerImpl)(nil)

// DeliveryRepository defines the minimal persistence interface required by
// the DeliveryManagerImpl. This is a subset of the full NotificationRepository
// plus additional queries specific to delivery management.
//
// By depending on this narrow interface rather than the full repository,
// the DeliveryManager is testable with lightweight mocks.
type DeliveryRepository interface {
	// InsertDeliveryIfNotExists performs an idempotent insert using
	// INSERT ... ON CONFLICT DO NOTHING. Returns the delivery ID and whether
	// it was newly created. The deterministic ID is constructed from the
	// notification ID, channel type, and channel index.
	InsertDeliveryIfNotExists(ctx context.Context, delivery *types.NotificationDelivery) (id string, created bool, err error)

	// UpdateDeliveryStatus updates a delivery record's status, reason, and
	// related timestamps atomically.
	UpdateDeliveryStatus(ctx context.Context, deliveryID string, status string, reason string) error

	// SetDeliverySuccess marks a delivery as sent with the provider message ID.
	SetDeliverySuccess(ctx context.Context, deliveryID string, providerMsgID string) error

	// SetDeliveryDeferred marks a delivery as deferred with a resumeAt time.
	SetDeliveryDeferred(ctx context.Context, deliveryID string, resumeAt time.Time) error

	// IncrementAttempt updates the last_attempt_at and attempt_count for a delivery.
	IncrementAttempt(ctx context.Context, deliveryID string) error

	// GetDeliveryAttemptCount returns the current attempt count for a delivery.
	GetDeliveryAttemptCount(ctx context.Context, deliveryID string) (int, error)

	// CountNonFailedDeliveries counts deliveries for a notification where
	// status is NOT 'failed'. Used by CheckAggregateFailure.
	CountNonFailedDeliveries(ctx context.Context, notificationID string) (int, error)

	// ResetEvaluationNotificationState sets last_notified_at = NULL and
	// last_notified_state = NULL in watchpoint_evaluation_state for the given
	// WatchPoint. This compensatory action allows the eval engine to retry
	// notification generation when all channels have failed.
	ResetEvaluationNotificationState(ctx context.Context, watchpointID string) error

	// CancelDeferredDeliveries sets status='skipped' for all 'deferred'
	// deliveries associated with a WatchPoint.
	CancelDeferredDeliveries(ctx context.Context, watchpointID string) error
}

// DeliveryManagerImpl is the production implementation of DeliveryManager.
// It orchestrates delivery state transitions using a repository and enforces
// retry policies.
type DeliveryManagerImpl struct {
	repo        DeliveryRepository
	retryPolicy RetryPolicy
	logger      types.Logger
}

// NewDeliveryManager creates a new DeliveryManagerImpl with the given
// repository, retry policy, and logger.
func NewDeliveryManager(repo DeliveryRepository, retryPolicy RetryPolicy, logger types.Logger) *DeliveryManagerImpl {
	return &DeliveryManagerImpl{
		repo:        repo,
		retryPolicy: retryPolicy,
		logger:      logger,
	}
}

// EnsureDeliveryExists performs an idempotent insert of a delivery record.
// The delivery ID is deterministic: "del_{notifID}_{channelType}_{idx}".
// If a record with this ID already exists, it returns the existing ID with
// created=false.
func (m *DeliveryManagerImpl) EnsureDeliveryExists(ctx context.Context, notifID string, chType types.ChannelType, idx int) (string, bool, error) {
	deliveryID := fmt.Sprintf("del_%s_%s_%d", notifID, string(chType), idx)

	delivery := &types.NotificationDelivery{
		ID:             deliveryID,
		NotificationID: notifID,
		ChannelType:    chType,
		Status:         string(types.DeliveryStatusPending),
		AttemptCount:   0,
	}

	id, created, err := m.repo.InsertDeliveryIfNotExists(ctx, delivery)
	if err != nil {
		return "", false, fmt.Errorf("EnsureDeliveryExists: %w", err)
	}

	if created {
		m.logger.Info("delivery record created",
			"delivery_id", id,
			"notification_id", notifID,
			"channel_type", string(chType),
			"channel_index", idx,
		)
	}

	return id, created, nil
}

// RecordAttempt logs that a worker is about to attempt delivery. Updates
// last_attempt_at and increments attempt_count.
func (m *DeliveryManagerImpl) RecordAttempt(ctx context.Context, deliveryID string) error {
	if err := m.repo.IncrementAttempt(ctx, deliveryID); err != nil {
		return fmt.Errorf("RecordAttempt: %w", err)
	}
	return nil
}

// MarkSuccess updates the delivery status to 'sent', records the provider
// message ID, and sets delivered_at.
func (m *DeliveryManagerImpl) MarkSuccess(ctx context.Context, deliveryID string, providerMsgID string) error {
	if err := m.repo.SetDeliverySuccess(ctx, deliveryID, providerMsgID); err != nil {
		return fmt.Errorf("MarkSuccess: %w", err)
	}

	m.logger.Info("delivery succeeded",
		"delivery_id", deliveryID,
		"provider_message_id", providerMsgID,
	)

	return nil
}

// MarkFailure updates the delivery status. If the attempt count is below the
// retry policy max, it returns shouldRetry=true so the caller can re-enqueue.
// If max attempts are exhausted, it marks as permanently failed.
func (m *DeliveryManagerImpl) MarkFailure(ctx context.Context, deliveryID string, reason string) (bool, error) {
	attemptCount, err := m.repo.GetDeliveryAttemptCount(ctx, deliveryID)
	if err != nil {
		return false, fmt.Errorf("MarkFailure: get attempt count: %w", err)
	}

	if attemptCount < m.retryPolicy.MaxAttempts {
		// Mark as retrying - the caller will re-enqueue with delay.
		if err := m.repo.UpdateDeliveryStatus(ctx, deliveryID, string(types.DeliveryStatusRetrying), reason); err != nil {
			return false, fmt.Errorf("MarkFailure: update status to retrying: %w", err)
		}

		m.logger.Warn("delivery failed, will retry",
			"delivery_id", deliveryID,
			"attempt", attemptCount,
			"max_attempts", m.retryPolicy.MaxAttempts,
			"reason", reason,
		)

		return true, nil
	}

	// Max retries exhausted - mark as permanently failed.
	if err := m.repo.UpdateDeliveryStatus(ctx, deliveryID, string(types.DeliveryStatusFailed), reason); err != nil {
		return false, fmt.Errorf("MarkFailure: update status to failed: %w", err)
	}

	m.logger.Error("delivery permanently failed",
		"delivery_id", deliveryID,
		"attempt", attemptCount,
		"reason", reason,
	)

	return false, nil
}

// MarkSkipped sets the delivery status to 'skipped' with the given reason.
// Used for TestMode or when policy permanently suppresses the notification.
func (m *DeliveryManagerImpl) MarkSkipped(ctx context.Context, deliveryID string, reason string) error {
	if err := m.repo.UpdateDeliveryStatus(ctx, deliveryID, string(types.DeliveryStatusSkipped), reason); err != nil {
		return fmt.Errorf("MarkSkipped: %w", err)
	}

	m.logger.Info("delivery skipped",
		"delivery_id", deliveryID,
		"reason", reason,
	)

	return nil
}

// MarkDeferred sets the delivery status to 'deferred' and records a resumeAt
// time. The RequeueDeferredNotifications scheduled job will re-enqueue this
// delivery once the resumeAt time has passed.
func (m *DeliveryManagerImpl) MarkDeferred(ctx context.Context, deliveryID string, resumeAt time.Time) error {
	if err := m.repo.SetDeliveryDeferred(ctx, deliveryID, resumeAt); err != nil {
		return fmt.Errorf("MarkDeferred: %w", err)
	}

	m.logger.Info("delivery deferred",
		"delivery_id", deliveryID,
		"resume_at", resumeAt.Format(time.RFC3339),
	)

	return nil
}

// CheckAggregateFailure determines if all sibling deliveries for a notification
// have failed. Returns true if there are no deliveries with a status other
// than 'failed'. This signals that the compensatory ResetNotificationState
// action should be taken.
func (m *DeliveryManagerImpl) CheckAggregateFailure(ctx context.Context, notificationID string) (bool, error) {
	count, err := m.repo.CountNonFailedDeliveries(ctx, notificationID)
	if err != nil {
		return false, fmt.Errorf("CheckAggregateFailure: %w", err)
	}

	allFailed := count == 0
	if allFailed {
		m.logger.Error("all deliveries failed for notification",
			"notification_id", notificationID,
		)
	}

	return allFailed, nil
}

// ResetNotificationState performs the compensatory transaction when all delivery
// channels fail. It nullifies last_notified_at and last_notified_state in the
// watchpoint_evaluation_state table, causing the evaluation engine to perceive
// the WatchPoint as "not notified" on the next cycle.
func (m *DeliveryManagerImpl) ResetNotificationState(ctx context.Context, watchpointID string) error {
	if err := m.repo.ResetEvaluationNotificationState(ctx, watchpointID); err != nil {
		return fmt.Errorf("ResetNotificationState: %w", err)
	}

	m.logger.Warn("notification state reset for retry",
		"watchpoint_id", watchpointID,
	)

	return nil
}

// CancelDeferred purges all deferred deliveries for a WatchPoint by setting
// their status to 'skipped'. Used when a WatchPoint is resumed to prevent
// flooding users with stale alerts that were parked during Quiet Hours.
func (m *DeliveryManagerImpl) CancelDeferred(ctx context.Context, watchpointID string) error {
	if err := m.repo.CancelDeferredDeliveries(ctx, watchpointID); err != nil {
		return fmt.Errorf("CancelDeferred: %w", err)
	}

	m.logger.Info("cancelled deferred deliveries",
		"watchpoint_id", watchpointID,
	)

	return nil
}
