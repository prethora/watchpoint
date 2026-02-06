package db

import (
	"context"
	"fmt"
	"time"

	"watchpoint/internal/types"
)

// UsageHistoryRepo provides data access for the usage_history table.
// It implements the UsageHistoryRepository interface defined in
// 05e-api-billing.md Section 4.4.
//
// The usage_history table has a composite primary key (organization_id, date, source)
// to support per-source usage attribution (VERT-001). All queries in this repository
// aggregate across sources to provide the organization-wide view.
type UsageHistoryRepo struct {
	db DBTX
}

// NewUsageHistoryRepo creates a new UsageHistoryRepo backed by the given
// database connection (pool or transaction).
func NewUsageHistoryRepo(db DBTX) *UsageHistoryRepo {
	return &UsageHistoryRepo{db: db}
}

// Query retrieves usage statistics for the given time range, aggregated at the
// specified granularity level. It handles the composite key by aggregating across
// all sources for the organization (VERT-001).
//
// For daily granularity, rows are returned as-is (grouped by date across sources).
// For monthly granularity, rows are aggregated using date_trunc('month', date).
//
// The query uses SUM for api_calls and notifications (additive metrics) and
// MAX for watchpoints (point-in-time gauge metric, not additive across days within
// a month -- we take the peak value).
func (r *UsageHistoryRepo) Query(
	ctx context.Context,
	orgID string,
	start, end time.Time,
	granularity types.TimeGranularity,
) ([]types.DailyUsageStat, error) {
	// Determine the date_trunc unit based on granularity.
	// Default to "day" for safety if an unknown granularity is provided.
	truncUnit := granularityToTruncUnit(granularity)

	query := fmt.Sprintf(`
		SELECT date_trunc('%s', date) AS period,
		       SUM(api_calls),
		       MAX(watchpoints),
		       SUM(notifications)
		FROM usage_history
		WHERE organization_id = $1
		  AND date >= $2
		  AND date <= $3
		GROUP BY period
		ORDER BY period ASC`, truncUnit)

	rows, err := r.db.Query(ctx, query, orgID, start, end)
	if err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to query usage history", err)
	}
	defer rows.Close()

	var results []types.DailyUsageStat
	for rows.Next() {
		var stat types.DailyUsageStat
		if err := rows.Scan(
			&stat.Date,
			&stat.APICalls,
			&stat.ActiveWatchPoints,
			&stat.NotificationsSent,
		); err != nil {
			return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to scan usage history row", err)
		}
		results = append(results, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "error iterating usage history rows", err)
	}

	return results, nil
}

// granularityToTruncUnit maps a TimeGranularity to a PostgreSQL date_trunc unit.
// Defaults to "day" for unknown values to fail safely.
func granularityToTruncUnit(g types.TimeGranularity) string {
	switch g {
	case types.GranularityMonthly:
		return "month"
	case types.GranularityDaily:
		return "day"
	default:
		return "day"
	}
}
