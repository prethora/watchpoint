// Package scheduler implements scheduled job services for the WatchPoint platform.
//
// This file implements the UsageAggregator service from
// architecture/09-scheduled-jobs.md Section 7.2. It snapshots daily API call
// and WatchPoint counts from rate_limits into usage_history, checks for usage
// overage conditions, and resets the rate_limits counters for the new day.
//
// The aggregator serves as both the scheduled catch-all (daily at 01:00 UTC) and
// supports lazy reset triggered by middleware. Each organization is processed in
// its own transaction with SELECT FOR UPDATE to prevent race conditions with
// concurrent API traffic.
//
// Flows: SCHED-001, SCHED-002, VERT-001, BILL-010 (composite key processing, overage warning)
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"watchpoint/internal/types"
)

// StaleRateLimitRow represents a single rate_limits row that has expired and
// needs to be snapshotted and reset. The composite primary key
// (organization_id, source) is preserved for per-source attribution (VERT-001).
type StaleRateLimitRow struct {
	OrganizationID  string
	Source          string
	APICallsCount   int
	WatchPointsCount int
	PeriodStart     time.Time
	PeriodEnd       time.Time
}

// UsageAggregatorDB defines the database operations needed by the UsageAggregator.
// Using an interface allows clean testing without database dependencies.
//
// The transactional flow is:
//  1. ListStaleRateLimits identifies candidates outside any transaction.
//  2. For each org, BeginTx starts a transaction.
//  3. LockRateLimitRows acquires FOR UPDATE locks within the transaction.
//  4. InsertUsageHistory persists the snapshot.
//  5. GetOrgOverageInfo fetches plan limits and current overage state.
//  6. UpdateOrgOverage sets or clears overage_started_at.
//  7. ResetRateLimitRow zeroes out the counters.
//  8. CommitTx / RollbackTx finalizes the transaction.
type UsageAggregatorDB interface {
	// ListStaleRateLimits returns distinct organization IDs with rate_limits
	// rows where period_end < now. The batch limit prevents processing too
	// many orgs in a single Lambda invocation.
	//
	// SQL: SELECT DISTINCT organization_id FROM rate_limits
	//      WHERE period_end < $1 LIMIT $2
	ListStaleRateLimits(ctx context.Context, now time.Time, limit int) ([]string, error)

	// BeginTx starts a new database transaction. The returned UsageAggregatorTx
	// must be committed or rolled back by the caller.
	BeginTx(ctx context.Context) (UsageAggregatorTx, error)
}

// UsageAggregatorTx defines the transactional operations for processing a
// single organization's usage snapshot. All methods operate within the
// transaction started by UsageAggregatorDB.BeginTx.
type UsageAggregatorTx interface {
	// LockRateLimitRows acquires FOR UPDATE locks on all rate_limits rows
	// for the given organization and returns the locked rows.
	//
	// SQL: SELECT organization_id, source, api_calls_count, watchpoints_count,
	//             period_start, period_end
	//      FROM rate_limits WHERE organization_id = $1 FOR UPDATE
	LockRateLimitRows(ctx context.Context, orgID string) ([]StaleRateLimitRow, error)

	// InsertUsageHistory inserts a usage_history row for the given org/date/source.
	// Uses ON CONFLICT DO UPDATE to handle re-runs idempotently by accumulating
	// counts rather than silently dropping data.
	//
	// SQL: INSERT INTO usage_history (organization_id, date, source, api_calls, watchpoints, notifications)
	//      VALUES ($1, $2, $3, $4, $5, 0)
	//      ON CONFLICT (organization_id, date, source)
	//      DO UPDATE SET api_calls = usage_history.api_calls + EXCLUDED.api_calls,
	//                    watchpoints = GREATEST(usage_history.watchpoints, EXCLUDED.watchpoints)
	InsertUsageHistory(ctx context.Context, orgID string, date time.Time, source string, apiCalls, watchpoints int) error

	// GetOrgOverageInfo fetches the plan_limits and overage_started_at for
	// the given organization. Used for the compliance check logic.
	//
	// SQL: SELECT plan_limits, overage_started_at FROM organizations
	//      WHERE id = $1 AND deleted_at IS NULL
	GetOrgOverageInfo(ctx context.Context, orgID string) (types.PlanLimits, *time.Time, error)

	// UpdateOrgOverage sets or clears the overage_started_at timestamp.
	// Pass nil to clear (cure), or a non-nil time to set (new overage).
	//
	// SQL: UPDATE organizations SET overage_started_at = $1, updated_at = NOW()
	//      WHERE id = $2
	UpdateOrgOverage(ctx context.Context, orgID string, overageStartedAt *time.Time) error

	// ResetRateLimitRow resets counters for a specific (org, source) row.
	//
	// SQL: UPDATE rate_limits
	//      SET api_calls_count = 0, watchpoints_count = 0,
	//          period_start = $1, period_end = $2,
	//          warning_sent_at = NULL, last_reset_at = NOW()
	//      WHERE organization_id = $3 AND source = $4
	ResetRateLimitRow(ctx context.Context, orgID, source string, periodStart, periodEnd time.Time) error

	// Commit commits the transaction.
	Commit(ctx context.Context) error

	// Rollback rolls back the transaction. Safe to call after Commit (no-op).
	Rollback(ctx context.Context) error
}

// DefaultBatchLimit is the maximum number of organizations processed per
// invocation to prevent Lambda timeouts during large backlogs.
const DefaultBatchLimit = 50

// usageAggregator implements the UsageAggregator interface from
// architecture/09-scheduled-jobs.md Section 7.2.
type usageAggregator struct {
	db     UsageAggregatorDB
	logger *slog.Logger
}

// NewUsageAggregator creates a new UsageAggregator service.
func NewUsageAggregator(db UsageAggregatorDB, logger *slog.Logger) *usageAggregator {
	if logger == nil {
		logger = slog.Default()
	}
	return &usageAggregator{
		db:     db,
		logger: logger,
	}
}

// SnapshotDailyUsage implements the UsageAggregator interface.
//
// It processes all organizations with stale rate_limits (period_end < targetDate)
// in batches, performing an atomic snapshot-and-reset for each organization.
//
// The targetDate parameter is the reference time used to:
//   - Identify stale rate_limits rows (period_end < targetDate).
//   - Compute the usage_history date (from the rate_limits period_start).
//   - Set the new period_start and period_end after reset.
//
// Returns the total number of organizations processed.
//
// Flow: SCHED-001 / SCHED-002
func (u *usageAggregator) SnapshotDailyUsage(ctx context.Context, targetDate time.Time) (int, error) {
	totalProcessed := 0

	for {
		// Step 1: Find organizations with stale rate_limits.
		orgIDs, err := u.db.ListStaleRateLimits(ctx, targetDate, DefaultBatchLimit)
		if err != nil {
			return totalProcessed, fmt.Errorf("listing stale rate limits: %w", err)
		}

		if len(orgIDs) == 0 {
			break
		}

		u.logger.InfoContext(ctx, "processing stale rate limits batch",
			"batch_size", len(orgIDs),
			"total_so_far", totalProcessed,
		)

		// Step 2: Process each organization in its own transaction.
		for _, orgID := range orgIDs {
			if err := u.processOrg(ctx, orgID, targetDate); err != nil {
				u.logger.ErrorContext(ctx, "failed to process org usage snapshot",
					"org_id", orgID,
					"error", err,
				)
				// Continue processing other orgs; don't let one failure block all.
				// The failed org will be retried on the next run since its
				// rate_limits row remains stale.
				continue
			}
			totalProcessed++
		}
	}

	u.logger.InfoContext(ctx, "usage snapshot complete",
		"total_processed", totalProcessed,
	)

	return totalProcessed, nil
}

// processOrg handles the atomic snapshot-and-reset for a single organization.
// Each call runs in its own transaction with SELECT FOR UPDATE locking.
//
// Per SCHED-001/SCHED-002 flow simulation:
//  1. Begin transaction.
//  2. Lock all rate_limits rows for this org (FOR UPDATE).
//  3. For each (org, source) row, insert into usage_history.
//  4. Aggregate usage across sources for overage check.
//  5. Check/update overage state on the organization.
//  6. Reset each rate_limits row.
//  7. Commit.
func (u *usageAggregator) processOrg(ctx context.Context, orgID string, now time.Time) error {
	tx, err := u.db.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction for org %s: %w", orgID, err)
	}
	// Ensure rollback on any error path. Rollback after Commit is a no-op.
	defer tx.Rollback(ctx) //nolint:errcheck

	// Step 2: Lock all rate_limits rows for this org within the transaction.
	rows, err := tx.LockRateLimitRows(ctx, orgID)
	if err != nil {
		return fmt.Errorf("locking rate limits for org %s: %w", orgID, err)
	}

	if len(rows) == 0 {
		// No rows to process (may have been reset by another process).
		return nil
	}

	// Step 3: Insert usage_history for each (org, source) row.
	// Also accumulate totals across sources for the overage check.
	var totalAPICalls int
	var totalWatchPoints int

	for i := range rows {
		row := &rows[i]

		// Derive the history date from the row's period_start (UTC date).
		historyDate := time.Date(
			row.PeriodStart.Year(),
			row.PeriodStart.Month(),
			row.PeriodStart.Day(),
			0, 0, 0, 0,
			time.UTC,
		)

		if err := tx.InsertUsageHistory(
			ctx,
			orgID,
			historyDate,
			row.Source,
			row.APICallsCount,
			row.WatchPointsCount,
		); err != nil {
			return fmt.Errorf("inserting usage history for org %s source %s: %w", orgID, row.Source, err)
		}

		totalAPICalls += row.APICallsCount
		totalWatchPoints += row.WatchPointsCount
	}

	// Step 4: Overage compliance check.
	// Fetch plan limits and current overage state.
	planLimits, currentOverage, err := tx.GetOrgOverageInfo(ctx, orgID)
	if err != nil {
		return fmt.Errorf("getting overage info for org %s: %w", orgID, err)
	}

	// Determine if the org is in overage or compliant.
	isOverage := isUsageOverLimit(totalAPICalls, totalWatchPoints, planLimits)

	// Step 5: Update overage state if needed.
	if isOverage && currentOverage == nil {
		// New overage: set overage_started_at to now.
		overageTime := now
		if err := tx.UpdateOrgOverage(ctx, orgID, &overageTime); err != nil {
			return fmt.Errorf("setting overage for org %s: %w", orgID, err)
		}
		u.logger.WarnContext(ctx, "org entered overage",
			"org_id", orgID,
			"api_calls", totalAPICalls,
			"watchpoints", totalWatchPoints,
			"limit_api_calls", planLimits.MaxAPICallsDaily,
			"limit_watchpoints", planLimits.MaxWatchPoints,
		)
	} else if !isOverage && currentOverage != nil {
		// Cure: clear overage_started_at.
		if err := tx.UpdateOrgOverage(ctx, orgID, nil); err != nil {
			return fmt.Errorf("clearing overage for org %s: %w", orgID, err)
		}
		u.logger.InfoContext(ctx, "org overage cured",
			"org_id", orgID,
		)
	}

	// Step 6: Reset each rate_limits row.
	nextMidnight := computeNextMidnight(now)
	for i := range rows {
		row := &rows[i]
		if err := tx.ResetRateLimitRow(ctx, orgID, row.Source, now, nextMidnight); err != nil {
			return fmt.Errorf("resetting rate limit for org %s source %s: %w", orgID, row.Source, err)
		}
	}

	// Step 7: Commit transaction.
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction for org %s: %w", orgID, err)
	}

	return nil
}

// isUsageOverLimit checks if the aggregated usage exceeds plan limits.
// A limit value of 0 means unlimited (Enterprise tier), so it is never exceeded.
//
// Per SCHED-001/SCHED-002 flow simulation:
//
//	"If api_count > limit OR wp_count > limit"
func isUsageOverLimit(apiCalls, watchpoints int, limits types.PlanLimits) bool {
	apiOverLimit := limits.MaxAPICallsDaily > 0 && apiCalls > limits.MaxAPICallsDaily
	wpOverLimit := limits.MaxWatchPoints > 0 && watchpoints > limits.MaxWatchPoints
	return apiOverLimit || wpOverLimit
}

// computeNextMidnight returns the next UTC midnight after the given time.
// This is used to set the period_end for the new rate_limits period.
func computeNextMidnight(t time.Time) time.Time {
	next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
	return next
}
