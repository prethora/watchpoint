// Package scheduler implements scheduled job services for the WatchPoint platform.
//
// This file implements the billing and system operations services from
// architecture/09-scheduled-jobs.md Sections 7.7, 7.10, and the canary
// verification logic from flow simulations OBS-008 / SCHED-004. It provides:
//
//   - StripeSyncer: Daily reconciliation of local billing state with Stripe,
//     including "Headless Repair" for organizations missing a Stripe customer ID
//     and "State Sync" for comparing plan/status between Stripe and local DB.
//   - SubscriptionEnforcer: Manages negative lifecycle events. EnforcePaymentFailure
//     pauses all WatchPoints for orgs delinquent > 7 days. EnforceOverage pauses
//     excess WatchPoints using LIFO strategy for orgs in overage > 14 days.
//   - CanaryVerifier: Queries canary WatchPoints (metadata.system_role=canary),
//     compares actual trigger state against expected state, and emits CloudWatch
//     metrics for system health monitoring.
//
// All services accept a `now` parameter for deterministic testing and manual
// backfill via MaintenancePayload.ReferenceTime.
//
// Flows: SCHED-003, BILL-005, BILL-006, BILL-011, OBS-008, SCHED-004
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"watchpoint/internal/types"
)

// =============================================================================
// Constants
// =============================================================================

// PaymentGracePeriod is the duration after a payment failure before WatchPoints
// are paused. Per BILL-006: "payment_failed_at > 7 days".
const PaymentGracePeriod = 7 * 24 * time.Hour

// OverageGracePeriod is the duration after an overage is detected before excess
// WatchPoints are paused. Per BILL-011: "overage_started_at > 14 days".
const OverageGracePeriod = 14 * 24 * time.Hour

// DefaultStripeSyncBatchLimit is the maximum number of organizations processed
// per StripeSyncer invocation.
const DefaultStripeSyncBatchLimit = 50

// =============================================================================
// Stripe Syncer (SCHED-003)
// =============================================================================

// StripeSyncerDB defines the database operations needed by the StripeSyncer.
type StripeSyncerDB interface {
	// ListOrgsForBillingSync returns organizations needing billing sync.
	// This includes orgs where last_billing_sync_at is stale (older than
	// stalenessThreshold) OR where stripe_customer_id is NULL.
	//
	// SQL: SELECT id, stripe_customer_id, plan, subscription_status
	//      FROM organizations
	//      WHERE (last_billing_sync_at < $now - $threshold OR stripe_customer_id IS NULL)
	//      AND deleted_at IS NULL
	//      LIMIT $limit
	ListOrgsForBillingSync(ctx context.Context, now time.Time, stalenessThreshold time.Duration, limit int) ([]BillingSyncOrg, error)

	// UpdateLastBillingSync sets last_billing_sync_at for the given organization.
	//
	// SQL: UPDATE organizations SET last_billing_sync_at = $1, updated_at = NOW()
	//      WHERE id = $2
	UpdateLastBillingSync(ctx context.Context, orgID string, syncedAt time.Time) error

	// UpdateOrgBillingState updates the plan and subscription status from Stripe.
	// Uses optimistic locking via last_subscription_event_at to prevent overwriting
	// more recent webhook data.
	//
	// SQL: UPDATE organizations SET plan = $1, subscription_status = $2,
	//      last_subscription_event_at = $3, updated_at = NOW()
	//      WHERE id = $4 AND deleted_at IS NULL
	//      AND (last_subscription_event_at IS NULL OR last_subscription_event_at < $3)
	UpdateOrgBillingState(ctx context.Context, orgID string, plan types.PlanTier, status types.SubscriptionStatus, eventTimestamp time.Time) error

	// UpdateStripeCustomerID backfills the stripe_customer_id for an organization.
	//
	// SQL: UPDATE organizations SET stripe_customer_id = $1, updated_at = NOW()
	//      WHERE id = $2
	UpdateStripeCustomerID(ctx context.Context, orgID string, customerID string) error
}

// BillingSyncOrg is the minimal data needed for billing reconciliation.
type BillingSyncOrg struct {
	ID                 string
	StripeCustomerID   string // Empty if NULL in DB
	Plan               types.PlanTier
	SubscriptionStatus types.SubscriptionStatus
}

// BillingServiceClient abstracts the Stripe integration for the syncer.
// The implementation calls the Stripe API to fetch subscription details.
type BillingServiceClient interface {
	// EnsureCustomer creates or retrieves a Stripe customer for the organization.
	// Returns the Stripe customer ID. Used for "Headless Repair".
	EnsureCustomer(ctx context.Context, orgID string) (string, error)

	// GetSubscription fetches the current subscription details from Stripe.
	// Returns nil if the organization has no active subscription.
	GetSubscription(ctx context.Context, orgID string) (*types.SubscriptionDetails, error)
}

// StripeSyncerMetrics abstracts CloudWatch metric emission for billing drift.
type StripeSyncerMetrics interface {
	// RecordBillingDrift emits a metric when local billing state differs from Stripe.
	RecordBillingDrift(ctx context.Context, orgID string)
}

// stripeSyncer implements StripeSyncer from architecture/09-scheduled-jobs.md
// Section 7.7.
type stripeSyncer struct {
	db      StripeSyncerDB
	billing BillingServiceClient
	metrics StripeSyncerMetrics
	logger  *slog.Logger
}

// NewStripeSyncer creates a new StripeSyncer service.
// The metrics parameter may be nil if metric emission is not configured.
func NewStripeSyncer(
	db StripeSyncerDB,
	billing BillingServiceClient,
	metrics StripeSyncerMetrics,
	logger *slog.Logger,
) *stripeSyncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &stripeSyncer{
		db:      db,
		billing: billing,
		metrics: metrics,
		logger:  logger,
	}
}

// SyncAtRisk reconciles local billing state with Stripe for organizations
// that haven't been synced within the staleness threshold, and repairs
// organizations missing a Stripe customer ID.
//
// Per SCHED-003 flow simulation:
//  1. Query organizations where last_billing_sync_at is stale OR stripe_customer_id IS NULL.
//  2. For each org:
//     a. Headless Repair: if stripe_customer_id is NULL, call EnsureCustomer.
//     b. State Sync: if ID exists, fetch subscription from Stripe, compare with local.
//     c. If mismatch: update local state, emit BillingStateDrift metric.
//  3. Update last_billing_sync_at.
//
// Returns the number of organizations processed.
func (s *stripeSyncer) SyncAtRisk(ctx context.Context, now time.Time, stalenessThreshold time.Duration, limit int) (int, error) {
	orgs, err := s.db.ListOrgsForBillingSync(ctx, now, stalenessThreshold, limit)
	if err != nil {
		return 0, fmt.Errorf("listing orgs for billing sync: %w", err)
	}

	if len(orgs) == 0 {
		s.logger.InfoContext(ctx, "no organizations need billing sync")
		return 0, nil
	}

	s.logger.InfoContext(ctx, "syncing at-risk organizations",
		"count", len(orgs),
		"staleness_threshold", stalenessThreshold.String(),
	)

	synced := 0
	for _, org := range orgs {
		if err := s.syncOrg(ctx, org, now); err != nil {
			s.logger.ErrorContext(ctx, "failed to sync org billing",
				"org_id", org.ID,
				"error", err,
			)
			// Continue with other orgs; this one will be retried next run.
			continue
		}
		synced++
	}

	s.logger.InfoContext(ctx, "billing sync complete",
		"synced", synced,
		"total_candidates", len(orgs),
	)

	return synced, nil
}

// syncOrg handles the sync for a single organization.
func (s *stripeSyncer) syncOrg(ctx context.Context, org BillingSyncOrg, now time.Time) error {
	// Branch - Headless Repair: stripe_customer_id is NULL.
	if org.StripeCustomerID == "" {
		customerID, err := s.billing.EnsureCustomer(ctx, org.ID)
		if err != nil {
			return fmt.Errorf("headless repair (EnsureCustomer) for org %s: %w", org.ID, err)
		}

		if err := s.db.UpdateStripeCustomerID(ctx, org.ID, customerID); err != nil {
			return fmt.Errorf("updating stripe_customer_id for org %s: %w", org.ID, err)
		}

		s.logger.InfoContext(ctx, "headless repair: backfilled stripe_customer_id",
			"org_id", org.ID,
			"customer_id", customerID,
		)

		// After repair, set the customer ID so State Sync can proceed.
		org.StripeCustomerID = customerID
	}

	// Branch - State Sync: fetch from Stripe and compare.
	sub, err := s.billing.GetSubscription(ctx, org.ID)
	if err != nil {
		return fmt.Errorf("fetching Stripe subscription for org %s: %w", org.ID, err)
	}

	if sub != nil {
		// Compare local vs remote state.
		planMismatch := sub.Plan != org.Plan
		statusMismatch := sub.Status != org.SubscriptionStatus

		if planMismatch || statusMismatch {
			s.logger.WarnContext(ctx, "billing state drift detected",
				"org_id", org.ID,
				"local_plan", string(org.Plan),
				"remote_plan", string(sub.Plan),
				"local_status", string(org.SubscriptionStatus),
				"remote_status", string(sub.Status),
			)

			// Update local state to match Stripe.
			if err := s.db.UpdateOrgBillingState(ctx, org.ID, sub.Plan, sub.Status, now); err != nil {
				return fmt.Errorf("updating billing state for org %s: %w", org.ID, err)
			}

			// Emit drift metric.
			if s.metrics != nil {
				s.metrics.RecordBillingDrift(ctx, org.ID)
			}
		}
	}

	// Always update last_billing_sync_at regardless of drift.
	if err := s.db.UpdateLastBillingSync(ctx, org.ID, now); err != nil {
		return fmt.Errorf("updating last_billing_sync_at for org %s: %w", org.ID, err)
	}

	return nil
}

// =============================================================================
// Subscription Enforcer (BILL-005, BILL-006, BILL-011)
// =============================================================================

// EnforcerDB defines the database operations needed by the SubscriptionEnforcer.
type EnforcerDB interface {
	// ListDelinquentOrgs returns organization IDs where payment_failed_at is
	// older than the grace period cutoff.
	//
	// SQL: SELECT id FROM organizations
	//      WHERE payment_failed_at IS NOT NULL
	//      AND payment_failed_at < $cutoff
	//      AND deleted_at IS NULL
	ListDelinquentOrgs(ctx context.Context, cutoff time.Time) ([]string, error)

	// PauseAllByOrgID sets status='paused' for all active WatchPoints belonging
	// to the organization with the specified pause reason.
	//
	// SQL: UPDATE watchpoints SET status='paused', paused_reason=$1, updated_at=NOW()
	//      WHERE organization_id=$2 AND status='active' AND deleted_at IS NULL
	PauseAllByOrgID(ctx context.Context, orgID string, reason types.PausedReason) error

	// ListOverageOrgs returns organizations where overage_started_at is older
	// than the grace period cutoff. Returns the org ID and plan limits.
	//
	// SQL: SELECT id, plan_limits FROM organizations
	//      WHERE overage_started_at IS NOT NULL
	//      AND overage_started_at < $cutoff
	//      AND deleted_at IS NULL
	ListOverageOrgs(ctx context.Context, cutoff time.Time) ([]OverageOrg, error)

	// CountActiveWatchPoints returns the count of active WatchPoints for the org.
	//
	// SQL: SELECT COUNT(*) FROM watchpoints
	//      WHERE organization_id = $1 AND status = 'active' AND deleted_at IS NULL
	CountActiveWatchPoints(ctx context.Context, orgID string) (int, error)

	// PauseExcessByOrgID pauses the most recently created WatchPoints using
	// LIFO strategy until the active count is at or below the limit.
	//
	// SQL: UPDATE watchpoints SET status='paused', paused_reason='billing_delinquency', updated_at=NOW()
	//      WHERE id IN (
	//          SELECT id FROM watchpoints
	//          WHERE organization_id=$1 AND status='active' AND deleted_at IS NULL
	//          ORDER BY created_at DESC
	//          OFFSET $limit
	//      )
	PauseExcessByOrgID(ctx context.Context, orgID string, limit int) error
}

// OverageOrg holds the data needed for overage enforcement.
type OverageOrg struct {
	ID         string
	PlanLimits types.PlanLimits
}

// EnforcerMetrics abstracts CloudWatch metric emission for billing enforcement.
type EnforcerMetrics interface {
	// RecordBillingEnforcement emits a metric when WatchPoints are paused
	// due to billing enforcement.
	RecordBillingEnforcement(ctx context.Context, orgID string, reason string, count int)
}

// subscriptionEnforcer implements SubscriptionEnforcer from
// architecture/09-scheduled-jobs.md Section 7.10.
type subscriptionEnforcer struct {
	db      EnforcerDB
	metrics EnforcerMetrics
	logger  *slog.Logger
}

// NewSubscriptionEnforcer creates a new SubscriptionEnforcer service.
// The metrics parameter may be nil if metric emission is not configured.
func NewSubscriptionEnforcer(
	db EnforcerDB,
	metrics EnforcerMetrics,
	logger *slog.Logger,
) *subscriptionEnforcer {
	if logger == nil {
		logger = slog.Default()
	}
	return &subscriptionEnforcer{
		db:      db,
		metrics: metrics,
		logger:  logger,
	}
}

// EnforcePaymentFailure checks for organizations with payment_failed_at older
// than 7 days and pauses all active WatchPoints.
//
// Per BILL-006 flow simulation:
//  1. Query organizations WHERE payment_failed_at < (now - 7 days).
//  2. For each org: PauseAllByOrgID with reason "billing_delinquency".
//  3. Log metrics.
//
// Returns the number of organizations where enforcement was applied.
func (e *subscriptionEnforcer) EnforcePaymentFailure(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Add(-PaymentGracePeriod)

	orgs, err := e.db.ListDelinquentOrgs(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("listing delinquent orgs: %w", err)
	}

	if len(orgs) == 0 {
		e.logger.InfoContext(ctx, "no delinquent organizations to enforce")
		return 0, nil
	}

	e.logger.InfoContext(ctx, "enforcing payment failure",
		"delinquent_count", len(orgs),
		"cutoff", cutoff.Format(time.RFC3339),
	)

	enforced := 0
	for _, orgID := range orgs {
		if err := e.db.PauseAllByOrgID(ctx, orgID, types.PausedReasonBillingDelinquency); err != nil {
			e.logger.ErrorContext(ctx, "failed to pause watchpoints for delinquent org",
				"org_id", orgID,
				"error", err,
			)
			// Continue with other orgs; this one will be retried next run.
			continue
		}

		e.logger.WarnContext(ctx, "paused all watchpoints for delinquent org",
			"org_id", orgID,
		)

		if e.metrics != nil {
			e.metrics.RecordBillingEnforcement(ctx, orgID, "payment_failure", 0)
		}

		enforced++
	}

	e.logger.InfoContext(ctx, "payment failure enforcement complete",
		"enforced", enforced,
		"total_delinquent", len(orgs),
	)

	return enforced, nil
}

// EnforceOverage checks for organizations with overage_started_at older than
// 14 days and pauses excess WatchPoints using LIFO strategy.
//
// Per BILL-011 flow simulation:
//  1. Query organizations WHERE overage_started_at < (now - 14 days).
//  2. For each org:
//     a. Get current active_count.
//     b. Calculate excess = active_count - plan limit.
//     c. If excess > 0: PauseExcessByOrgID (LIFO pausing).
//  3. Log metrics.
//
// Returns the number of organizations where enforcement was applied.
func (e *subscriptionEnforcer) EnforceOverage(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Add(-OverageGracePeriod)

	orgs, err := e.db.ListOverageOrgs(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("listing overage orgs: %w", err)
	}

	if len(orgs) == 0 {
		e.logger.InfoContext(ctx, "no overage organizations to enforce")
		return 0, nil
	}

	e.logger.InfoContext(ctx, "enforcing overage",
		"overage_count", len(orgs),
		"cutoff", cutoff.Format(time.RFC3339),
	)

	enforced := 0
	for _, org := range orgs {
		// Enterprise plan (limit=0) means unlimited; skip enforcement.
		if org.PlanLimits.MaxWatchPoints == 0 {
			e.logger.InfoContext(ctx, "skipping overage enforcement for unlimited plan",
				"org_id", org.ID,
			)
			continue
		}

		activeCount, err := e.db.CountActiveWatchPoints(ctx, org.ID)
		if err != nil {
			e.logger.ErrorContext(ctx, "failed to count active watchpoints for overage org",
				"org_id", org.ID,
				"error", err,
			)
			continue
		}

		excess := activeCount - org.PlanLimits.MaxWatchPoints
		if excess <= 0 {
			// Org has resolved the overage (e.g., deleted WPs or upgraded).
			// The overage_started_at will be cleared by the UsageAggregator's
			// compliance check on its next run.
			e.logger.InfoContext(ctx, "overage resolved, no excess to pause",
				"org_id", org.ID,
				"active_count", activeCount,
				"limit", org.PlanLimits.MaxWatchPoints,
			)
			continue
		}

		// LIFO pause: pause excess WPs, keeping the oldest.
		if err := e.db.PauseExcessByOrgID(ctx, org.ID, org.PlanLimits.MaxWatchPoints); err != nil {
			e.logger.ErrorContext(ctx, "failed to pause excess watchpoints for overage org",
				"org_id", org.ID,
				"excess", excess,
				"error", err,
			)
			continue
		}

		e.logger.WarnContext(ctx, "paused excess watchpoints for overage org",
			"org_id", org.ID,
			"excess_paused", excess,
			"limit", org.PlanLimits.MaxWatchPoints,
		)

		if e.metrics != nil {
			e.metrics.RecordBillingEnforcement(ctx, org.ID, "overage", excess)
		}

		enforced++
	}

	e.logger.InfoContext(ctx, "overage enforcement complete",
		"enforced", enforced,
		"total_overage", len(orgs),
	)

	return enforced, nil
}

// =============================================================================
// Canary Verifier (OBS-008 / SCHED-004)
// =============================================================================

// CanaryVerifierDB defines the database operations needed by the CanaryVerifier.
type CanaryVerifierDB interface {
	// ListCanaryWatchPoints returns active WatchPoints with system_role=canary
	// and their evaluation state.
	//
	// SQL: SELECT wp.id, wp.metadata, wes.previous_trigger_state
	//      FROM watchpoints wp
	//      LEFT JOIN watchpoint_evaluation_state wes ON wp.id = wes.watchpoint_id
	//      WHERE wp.metadata->>'system_role' = 'canary' AND wp.status = 'active'
	ListCanaryWatchPoints(ctx context.Context) ([]CanaryWatchPoint, error)
}

// CanaryWatchPoint holds the minimal data needed for canary verification.
type CanaryWatchPoint struct {
	ID       string
	Metadata json.RawMessage // Contains {"system_role": "canary", "canary_expect_trigger": bool}

	// PreviousTriggerState is the actual trigger state from evaluation.
	// Nil if no evaluation state exists yet (LEFT JOIN produces NULL).
	PreviousTriggerState *bool
}

// canaryMetadata is the expected structure of the metadata JSONB for canary WPs.
type canaryMetadata struct {
	SystemRole         string `json:"system_role"`
	CanaryExpectTrigger bool   `json:"canary_expect_trigger"`
}

// CanaryVerifierMetrics abstracts CloudWatch metric emission for canary results.
type CanaryVerifierMetrics interface {
	// RecordCanarySuccess emits a CanarySuccess metric.
	RecordCanarySuccess(ctx context.Context, watchpointID string)
	// RecordCanaryFailure emits a CanaryFailure metric.
	RecordCanaryFailure(ctx context.Context, watchpointID string)
}

// canaryVerifier implements the canary verification logic from
// OBS-008 / SCHED-004 flow simulations.
type canaryVerifier struct {
	db      CanaryVerifierDB
	metrics CanaryVerifierMetrics
	logger  *slog.Logger
}

// NewCanaryVerifier creates a new CanaryVerifier service.
func NewCanaryVerifier(
	db CanaryVerifierDB,
	metrics CanaryVerifierMetrics,
	logger *slog.Logger,
) *canaryVerifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &canaryVerifier{
		db:      db,
		metrics: metrics,
		logger:  logger,
	}
}

// Verify queries all canary WatchPoints and compares their actual trigger state
// against the expected state stored in metadata.
//
// Per OBS-008 / SCHED-004 flow simulation:
//  1. Query WatchPoints WHERE metadata->>'system_role' = 'canary' AND status = 'active'.
//  2. For each canary:
//     a. Parse metadata to extract canary_expect_trigger.
//     b. Read previous_trigger_state from evaluation_state (defaults to false if NULL).
//     c. Compare: if actual != expected, emit CanaryFailure metric and log error.
//     d. If match, emit CanarySuccess metric.
//
// Returns the total number of canaries verified.
func (v *canaryVerifier) Verify(ctx context.Context) (int, error) {
	canaries, err := v.db.ListCanaryWatchPoints(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing canary watchpoints: %w", err)
	}

	if len(canaries) == 0 {
		v.logger.InfoContext(ctx, "no canary watchpoints found")
		return 0, nil
	}

	v.logger.InfoContext(ctx, "verifying canary watchpoints",
		"count", len(canaries),
	)

	verified := 0
	for _, canary := range canaries {
		// Parse metadata to extract expected trigger state.
		var meta canaryMetadata
		if err := json.Unmarshal(canary.Metadata, &meta); err != nil {
			v.logger.ErrorContext(ctx, "failed to parse canary metadata",
				"watchpoint_id", canary.ID,
				"error", err,
			)
			// Skip this canary but continue with others.
			continue
		}

		// Determine actual trigger state.
		// Per SCHED-004: "Defaults to FALSE if NULL/missing."
		actual := false
		if canary.PreviousTriggerState != nil {
			actual = *canary.PreviousTriggerState
		}

		expected := meta.CanaryExpectTrigger

		if actual != expected {
			// Canary mismatch - something is wrong with the evaluation pipeline.
			v.logger.ErrorContext(ctx, "canary mismatch detected",
				"watchpoint_id", canary.ID,
				"expected", expected,
				"actual", actual,
			)

			if v.metrics != nil {
				v.metrics.RecordCanaryFailure(ctx, canary.ID)
			}
		} else {
			// Canary match - evaluation pipeline is healthy.
			v.logger.InfoContext(ctx, "canary verified successfully",
				"watchpoint_id", canary.ID,
				"trigger_state", actual,
			)

			if v.metrics != nil {
				v.metrics.RecordCanarySuccess(ctx, canary.ID)
			}
		}

		verified++
	}

	v.logger.InfoContext(ctx, "canary verification complete",
		"verified", verified,
		"total_canaries", len(canaries),
	)

	return verified, nil
}
