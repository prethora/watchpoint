// Package billing provides plan management, usage reporting, and billing domain logic.
package billing

import (
	"context"
	"time"

	"watchpoint/internal/types"
)

// UsageReporter aggregates usage metrics for a specific period.
// Defined in 05e-api-billing.md Section 4.2.
type UsageReporter interface {
	// GetCurrentUsage returns a snapshot of usage against limits for the current period.
	// Uses Direct Count against the watchpoints table for real-time accuracy.
	GetCurrentUsage(ctx context.Context, orgID string) (*types.UsageSnapshot, error)

	// GetUsageHistory returns timeseries data for charting.
	// Performs a Union of historical data from usage_history and current partial day
	// from rate_limits + live watchpoints count.
	GetUsageHistory(
		ctx context.Context,
		orgID string,
		rangeStart, rangeEnd time.Time,
		granularity types.TimeGranularity,
	) ([]*types.UsageDataPoint, error)
}

// UsageEnforcer checks plan limits before resource creation.
// Defined in 05b-api-watchpoints.md Section 3.2.
type UsageEnforcer interface {
	// CheckLimit verifies whether the organization can create additional resources
	// of the given type. Returns nil if allowed, ErrLimitWatchpoints if exceeded.
	//
	// For ResourceWatchPoints: uses Direct Count query against watchpoints table.
	// The count parameter indicates how many new resources the caller wants to create.
	CheckLimit(ctx context.Context, orgID string, resource types.ResourceType, count int) error
}

// OrgLookup provides the minimal organization data needed for usage reporting.
// This is a focused interface to avoid depending on the full OrganizationRepository.
type OrgLookup interface {
	// GetPlanAndLimits returns the plan tier and plan limits for the given organization.
	GetPlanAndLimits(ctx context.Context, orgID string) (types.PlanTier, types.PlanLimits, error)
}

// UsageDB provides direct database access for usage queries that bypass
// repository abstractions. This is the minimal interface for the specific
// aggregation queries that the UsageReporter needs.
type UsageDB interface {
	// CountActiveWatchPoints performs the Direct Count query:
	//   SELECT COUNT(*) FROM watchpoints
	//   WHERE organization_id = $1 AND status != 'archived' AND deleted_at IS NULL
	CountActiveWatchPoints(ctx context.Context, orgID string) (int, error)

	// GetAPICallsCount aggregates API calls across all sources (VERT-001):
	//   SELECT COALESCE(SUM(api_calls_count), 0) FROM rate_limits
	//   WHERE organization_id = $1
	GetAPICallsCount(ctx context.Context, orgID string) (int, error)

	// GetRateLimitPeriodEnd returns the period end timestamp from rate_limits.
	// Used to determine the next reset time for API call counters.
	// Returns the latest period_end across all source rows for the org.
	GetRateLimitPeriodEnd(ctx context.Context, orgID string) (*time.Time, error)
}

// UsageHistoryQuerier is the interface for querying historical usage data.
// Implemented by UsageHistoryRepo in internal/db.
type UsageHistoryQuerier interface {
	Query(
		ctx context.Context,
		orgID string,
		start, end time.Time,
		granularity types.TimeGranularity,
	) ([]types.DailyUsageStat, error)
}

// usageReporterImpl implements both UsageReporter and UsageEnforcer.
type usageReporterImpl struct {
	orgLookup    OrgLookup
	usageDB      UsageDB
	historyRepo  UsageHistoryQuerier
	planRegistry PlanRegistry
}

// NewUsageReporter creates a new implementation of UsageReporter and UsageEnforcer.
// The returned value satisfies both interfaces.
func NewUsageReporter(
	orgLookup OrgLookup,
	usageDB UsageDB,
	historyRepo UsageHistoryQuerier,
	planRegistry PlanRegistry,
) *usageReporterImpl {
	return &usageReporterImpl{
		orgLookup:    orgLookup,
		usageDB:      usageDB,
		historyRepo:  historyRepo,
		planRegistry: planRegistry,
	}
}

// Compile-time interface assertions.
var (
	_ UsageReporter = (*usageReporterImpl)(nil)
	_ UsageEnforcer = (*usageReporterImpl)(nil)
)

// GetCurrentUsage returns a snapshot of the organization's resource usage against
// plan limits for the current billing period.
//
// Implementation follows the INFO-003 flow simulation:
//  1. Fetch plan and limits for the organization.
//  2. Fetch API calls from rate_limits (SUM across all sources per VERT-001).
//  3. Fetch active WatchPoints via Direct Count against watchpoints table.
//  4. Construct UsageSnapshot with limit details and current consumption.
func (r *usageReporterImpl) GetCurrentUsage(ctx context.Context, orgID string) (*types.UsageSnapshot, error) {
	// Step 1: Get plan and limits
	plan, limits, err := r.orgLookup.GetPlanAndLimits(ctx, orgID)
	if err != nil {
		return nil, err
	}

	// If plan is unknown, use the PlanRegistry for authoritative limits
	if limits.MaxWatchPoints == 0 && limits.MaxAPICallsDaily == 0 && !limits.AllowNowcast {
		limits = r.planRegistry.GetLimits(plan)
	}

	// Step 2: Fetch API call count (aggregated across all sources)
	apiCalls, err := r.usageDB.GetAPICallsCount(ctx, orgID)
	if err != nil {
		return nil, err
	}

	// Step 3: Direct Count of active WatchPoints
	wpCount, err := r.usageDB.CountActiveWatchPoints(ctx, orgID)
	if err != nil {
		return nil, err
	}

	// Step 4: Get next reset time for API calls
	periodEnd, err := r.usageDB.GetRateLimitPeriodEnd(ctx, orgID)
	if err != nil {
		return nil, err
	}

	// Step 5: Construct the snapshot
	snapshot := &types.UsageSnapshot{
		ResourceUsage: map[types.ResourceType]int{
			types.ResourceWatchPoints: wpCount,
			types.ResourceAPICalls:    apiCalls,
		},
		LimitDetails: map[types.ResourceType]types.LimitDetail{
			types.ResourceWatchPoints: {
				Limit:     limits.MaxWatchPoints,
				Used:      wpCount,
				ResetType: types.ResetNever, // WatchPoints don't reset
			},
			types.ResourceAPICalls: {
				Limit:     limits.MaxAPICallsDaily,
				Used:      apiCalls,
				ResetType: types.ResetDaily,
				NextReset: periodEnd,
			},
		},
	}

	return snapshot, nil
}

// GetUsageHistory returns timeseries data for charting.
//
// Implementation follows the INFO-004 flow simulation:
//  1. Query historical data from usage_history via the history repo.
//  2. Fetch current partial day stats from rate_limits and live watchpoints count.
//  3. Perform in-memory union: append current partial period to historical list.
//
// The result is a list of UsageDataPoint, which is a simplified timeseries format.
// Each data point contains the sum of all metrics for that period. For more detailed
// per-resource breakdown, callers should use GetCurrentUsage for the current period.
func (r *usageReporterImpl) GetUsageHistory(
	ctx context.Context,
	orgID string,
	rangeStart, rangeEnd time.Time,
	granularity types.TimeGranularity,
) ([]*types.UsageDataPoint, error) {
	// Step 1: Get historical data
	historicalStats, err := r.historyRepo.Query(ctx, orgID, rangeStart, rangeEnd, granularity)
	if err != nil {
		return nil, err
	}

	// Step 2: Convert historical stats to data points
	var result []*types.UsageDataPoint
	for i := range historicalStats {
		stat := &historicalStats[i]
		result = append(result, &types.UsageDataPoint{
			Timestamp: stat.Date,
			Value:     stat.APICalls,
		})
	}

	// Step 3: Fetch current partial day for live data overlay
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	// Only append live data if the current day falls within the requested range
	if !today.Before(rangeStart) && !today.After(rangeEnd) {
		apiCalls, err := r.usageDB.GetAPICallsCount(ctx, orgID)
		if err != nil {
			return nil, err
		}

		// Check if today already exists in the historical data (to avoid double counting).
		// The usage_history table is populated by the daily aggregation job at end-of-day,
		// so typically today is NOT in the historical data.
		todayExists := false
		for _, dp := range result {
			if dp.Timestamp.Year() == today.Year() &&
				dp.Timestamp.Month() == today.Month() &&
				dp.Timestamp.Day() == today.Day() {
				todayExists = true
				break
			}
		}

		if !todayExists {
			result = append(result, &types.UsageDataPoint{
				Timestamp: today,
				Value:     apiCalls,
			})
		}
	}

	return result, nil
}

// CheckLimit verifies whether the organization can create additional resources
// of the given type without exceeding plan limits.
//
// Implementation follows the BILL-008 flow simulation:
//  1. Get plan limits for the organization.
//  2. For WatchPoints: execute Direct Count query.
//  3. Compare (current + count) against limit.
//  4. Return nil if allowed, ErrLimitWatchpoints if exceeded.
//
// Enterprise plans (MaxWatchPoints=0) are treated as unlimited.
func (r *usageReporterImpl) CheckLimit(
	ctx context.Context,
	orgID string,
	resource types.ResourceType,
	count int,
) error {
	// Get the organization's plan and limits
	plan, _, err := r.orgLookup.GetPlanAndLimits(ctx, orgID)
	if err != nil {
		return err
	}

	// Use PlanRegistry as the authoritative source for limits
	limits := r.planRegistry.GetLimits(plan)

	switch resource {
	case types.ResourceWatchPoints:
		// 0 means unlimited (Enterprise tier)
		if limits.MaxWatchPoints == 0 {
			return nil
		}

		// Direct Count query against watchpoints table
		currentCount, err := r.usageDB.CountActiveWatchPoints(ctx, orgID)
		if err != nil {
			return err
		}

		if currentCount+count > limits.MaxWatchPoints {
			return types.NewAppErrorWithDetails(
				types.ErrCodeLimitWatchpoints,
				"WatchPoint limit exceeded for current plan",
				nil,
				map[string]any{
					"current": currentCount,
					"limit":   limits.MaxWatchPoints,
					"plan":    string(plan),
				},
			)
		}
		return nil

	case types.ResourceAPICalls:
		// 0 means unlimited (Enterprise tier)
		if limits.MaxAPICallsDaily == 0 {
			return nil
		}

		apiCalls, err := r.usageDB.GetAPICallsCount(ctx, orgID)
		if err != nil {
			return err
		}

		if apiCalls+count > limits.MaxAPICallsDaily {
			return types.NewAppErrorWithDetails(
				types.ErrCodeLimitAPICalls,
				"API call limit exceeded for current plan",
				nil,
				map[string]any{
					"current": apiCalls,
					"limit":   limits.MaxAPICallsDaily,
					"plan":    string(plan),
				},
			)
		}
		return nil

	default:
		return types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"unknown resource type for limit check: "+string(resource),
			nil,
		)
	}
}
