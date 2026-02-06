package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"watchpoint/internal/types"
)

// SubscriptionStateRepo manages local billing state synchronization.
// It implements the SubscriptionStateRepository interface defined in
// 05e-api-billing.md Section 4.4.
//
// Key invariants:
//   - UpdateSubscriptionStatus uses Optimistic Locking via last_subscription_event_at
//     to handle out-of-order Stripe webhooks (Section 9.2).
//   - UpdateSubscriptionStatus checks Organization.deleted_at to prevent zombie billing
//     (Section 9.1).
type SubscriptionStateRepo struct {
	db     DBTX
	logger *slog.Logger
}

// NewSubscriptionStateRepo creates a new SubscriptionStateRepo backed by the
// given database connection (pool or transaction).
func NewSubscriptionStateRepo(db DBTX, logger *slog.Logger) *SubscriptionStateRepo {
	if logger == nil {
		logger = slog.Default()
	}
	return &SubscriptionStateRepo{db: db, logger: logger}
}

// UpdateSubscriptionStatus atomically updates the plan and subscription status.
//
// Invariants enforced (from 05e-api-billing.md Section 9.1, 9.2):
//  1. Zombie check: MUST fail if Organization.deleted_at IS NOT NULL.
//     Logs a ZC_BILLING_ALERT to signal Ops to manually cancel in Stripe.
//  2. Optimistic Locking: MUST fail if eventTimestamp < stored last_subscription_event_at.
//     Old/duplicate events are silently ignored (idempotent no-op).
//
// The query uses a single UPDATE with WHERE conditions that enforce both constraints,
// then checks rows affected to distinguish between "zombie org" and "stale event".
func (r *SubscriptionStateRepo) UpdateSubscriptionStatus(
	ctx context.Context,
	orgID string,
	newPlan types.PlanTier,
	status types.SubscriptionStatus,
	eventTimestamp time.Time,
) error {
	// First, check if the organization is deleted (zombie check).
	// We do this as a separate query so we can log the specific ZC_BILLING_ALERT.
	var deletedAt *time.Time
	err := r.db.QueryRow(ctx,
		`SELECT deleted_at FROM organizations WHERE id = $1`,
		orgID,
	).Scan(&deletedAt)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to check organization status", err)
	}

	if deletedAt != nil {
		// Zombie billing detected -- log alert for Ops
		r.logger.Error("ZC_BILLING_ALERT: webhook received for deleted organization",
			slog.String("org_id", orgID),
			slog.String("new_plan", string(newPlan)),
			slog.String("status", string(status)),
			slog.Time("event_timestamp", eventTimestamp),
		)
		return types.NewAppError(
			types.ErrCodeConflictConcurrent,
			fmt.Sprintf("organization %s is deleted; billing update rejected (ZC_BILLING_ALERT)", orgID),
			nil,
		)
	}

	// Optimistic locking update: only apply if this event is newer than
	// the last processed event.
	tag, err := r.db.Exec(ctx,
		`UPDATE organizations
		 SET plan = $1,
		     subscription_status = $2,
		     last_subscription_event_at = $3,
		     updated_at = NOW()
		 WHERE id = $4
		   AND deleted_at IS NULL
		   AND (last_subscription_event_at IS NULL OR last_subscription_event_at < $3)`,
		newPlan,
		status,
		eventTimestamp,
		orgID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update subscription status", err)
	}

	if tag.RowsAffected() == 0 {
		// Event is older than or equal to what we already have -- idempotent no-op.
		r.logger.Info("stale subscription event ignored (optimistic lock)",
			slog.String("org_id", orgID),
			slog.Time("event_timestamp", eventTimestamp),
		)
		return nil
	}

	return nil
}

// UpdatePaymentFailure records the dunning state by setting payment_failed_at.
// This is called when an invoice.payment_failed webhook is received, and is used
// to track grace periods (e.g., 7 days after failure before pausing WatchPoints).
func (r *SubscriptionStateRepo) UpdatePaymentFailure(ctx context.Context, orgID string, failedAt time.Time) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE organizations
		 SET payment_failed_at = $1,
		     updated_at = NOW()
		 WHERE id = $2
		   AND deleted_at IS NULL`,
		failedAt,
		orgID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update payment failure state", err)
	}

	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundOrg, "organization not found or deleted", nil)
	}

	return nil
}
