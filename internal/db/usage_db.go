package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"watchpoint/internal/types"
)

// UsageDBImpl provides the concrete database queries needed by the UsageReporter.
// It implements the billing.UsageDB and billing.OrgLookup interfaces.
//
// These queries are intentionally separated from the standard repository pattern
// because they are read-only aggregation queries that span multiple tables and
// serve a specific domain need (billing/usage reporting).
type UsageDBImpl struct {
	db DBTX
}

// NewUsageDBImpl creates a new UsageDBImpl backed by the given database connection.
func NewUsageDBImpl(db DBTX) *UsageDBImpl {
	return &UsageDBImpl{db: db}
}

// CountActiveWatchPoints performs the Direct Count query against the watchpoints table.
// This is the authoritative count used for both the usage dashboard (INFO-003) and
// limit enforcement (BILL-008).
//
// The query filters out archived and soft-deleted WatchPoints to count only
// resources that consume plan capacity.
//
// SQL: SELECT COUNT(*) FROM watchpoints
//
//	WHERE organization_id = $1 AND status != 'archived' AND deleted_at IS NULL
func (u *UsageDBImpl) CountActiveWatchPoints(ctx context.Context, orgID string) (int, error) {
	var count int
	err := u.db.QueryRow(ctx,
		`SELECT COUNT(*)
		 FROM watchpoints
		 WHERE organization_id = $1
		   AND status != 'archived'
		   AND deleted_at IS NULL`,
		orgID,
	).Scan(&count)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to count active watchpoints", err)
	}
	return count, nil
}

// GetAPICallsCount aggregates API call counts across all sources for an organization.
// The rate_limits table has a composite primary key (organization_id, source) to support
// per-source usage attribution (VERT-001). This query sums across all sources.
//
// SQL: SELECT COALESCE(SUM(api_calls_count), 0) FROM rate_limits
//
//	WHERE organization_id = $1
func (u *UsageDBImpl) GetAPICallsCount(ctx context.Context, orgID string) (int, error) {
	var count int
	err := u.db.QueryRow(ctx,
		`SELECT COALESCE(SUM(api_calls_count), 0)
		 FROM rate_limits
		 WHERE organization_id = $1`,
		orgID,
	).Scan(&count)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to get API calls count", err)
	}
	return count, nil
}

// GetRateLimitPeriodEnd returns the latest period_end timestamp from rate_limits
// for the given organization. Returns nil if no rate_limits rows exist.
// This is used by the usage dashboard to show when the API call counter resets.
func (u *UsageDBImpl) GetRateLimitPeriodEnd(ctx context.Context, orgID string) (*time.Time, error) {
	var periodEnd *time.Time
	err := u.db.QueryRow(ctx,
		`SELECT MAX(period_end)
		 FROM rate_limits
		 WHERE organization_id = $1`,
		orgID,
	).Scan(&periodEnd)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to get rate limit period end", err)
	}
	return periodEnd, nil
}

// GetPlanAndLimits returns the plan tier and plan limits for the given organization.
// Implements the billing.OrgLookup interface.
func (u *UsageDBImpl) GetPlanAndLimits(ctx context.Context, orgID string) (types.PlanTier, types.PlanLimits, error) {
	var plan types.PlanTier
	var limits types.PlanLimits

	err := u.db.QueryRow(ctx,
		`SELECT plan, plan_limits
		 FROM organizations
		 WHERE id = $1 AND deleted_at IS NULL`,
		orgID,
	).Scan(&plan, &limits)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", types.PlanLimits{}, types.NewAppError(types.ErrCodeNotFoundOrg, "organization not found", nil)
		}
		return "", types.PlanLimits{}, types.NewAppError(types.ErrCodeInternalDB, "failed to get organization plan", err)
	}

	return plan, limits, nil
}
