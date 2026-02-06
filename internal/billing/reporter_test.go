package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/types"
)

// --- Mock implementations ---

type mockOrgLookup struct {
	mock.Mock
}

func (m *mockOrgLookup) GetPlanAndLimits(ctx context.Context, orgID string) (types.PlanTier, types.PlanLimits, error) {
	args := m.Called(ctx, orgID)
	return args.Get(0).(types.PlanTier), args.Get(1).(types.PlanLimits), args.Error(2)
}

type mockUsageDB struct {
	mock.Mock
}

func (m *mockUsageDB) CountActiveWatchPoints(ctx context.Context, orgID string) (int, error) {
	args := m.Called(ctx, orgID)
	return args.Int(0), args.Error(1)
}

func (m *mockUsageDB) GetAPICallsCount(ctx context.Context, orgID string) (int, error) {
	args := m.Called(ctx, orgID)
	return args.Int(0), args.Error(1)
}

func (m *mockUsageDB) GetRateLimitPeriodEnd(ctx context.Context, orgID string) (*time.Time, error) {
	args := m.Called(ctx, orgID)
	if t := args.Get(0); t != nil {
		return t.(*time.Time), args.Error(1)
	}
	return nil, args.Error(1)
}

type mockHistoryRepo struct {
	mock.Mock
}

func (m *mockHistoryRepo) Query(
	ctx context.Context,
	orgID string,
	start, end time.Time,
	granularity types.TimeGranularity,
) ([]types.DailyUsageStat, error) {
	args := m.Called(ctx, orgID, start, end, granularity)
	if s := args.Get(0); s != nil {
		return s.([]types.DailyUsageStat), args.Error(1)
	}
	return nil, args.Error(1)
}

// --- Helper ---

func setupReporter() (*usageReporterImpl, *mockOrgLookup, *mockUsageDB, *mockHistoryRepo) {
	orgLookup := new(mockOrgLookup)
	usageDB := new(mockUsageDB)
	historyRepo := new(mockHistoryRepo)
	planRegistry := NewStaticPlanRegistry()

	reporter := NewUsageReporter(orgLookup, usageDB, historyRepo, planRegistry)
	return reporter, orgLookup, usageDB, historyRepo
}

// --- GetCurrentUsage Tests ---

func TestGetCurrentUsage_Success(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	periodEnd := time.Date(2026, 2, 7, 0, 0, 0, 0, time.UTC)

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanPro, types.PlanLimits{
			MaxWatchPoints:   100,
			MaxAPICallsDaily: 1000,
			AllowNowcast:     true,
		}, nil)

	usageDB.On("GetAPICallsCount", mock.Anything, "org_1").Return(350, nil)
	usageDB.On("CountActiveWatchPoints", mock.Anything, "org_1").Return(42, nil)
	usageDB.On("GetRateLimitPeriodEnd", mock.Anything, "org_1").Return(&periodEnd, nil)

	snapshot, err := reporter.GetCurrentUsage(context.Background(), "org_1")
	require.NoError(t, err)
	require.NotNil(t, snapshot)

	// Verify resource usage
	assert.Equal(t, 42, snapshot.ResourceUsage[types.ResourceWatchPoints])
	assert.Equal(t, 350, snapshot.ResourceUsage[types.ResourceAPICalls])

	// Verify limit details
	wpDetail := snapshot.LimitDetails[types.ResourceWatchPoints]
	assert.Equal(t, 100, wpDetail.Limit)
	assert.Equal(t, 42, wpDetail.Used)
	assert.Equal(t, types.ResetNever, wpDetail.ResetType)
	assert.Nil(t, wpDetail.NextReset)

	apiDetail := snapshot.LimitDetails[types.ResourceAPICalls]
	assert.Equal(t, 1000, apiDetail.Limit)
	assert.Equal(t, 350, apiDetail.Used)
	assert.Equal(t, types.ResetDaily, apiDetail.ResetType)
	require.NotNil(t, apiDetail.NextReset)
	assert.Equal(t, periodEnd, *apiDetail.NextReset)

	orgLookup.AssertExpectations(t)
	usageDB.AssertExpectations(t)
}

func TestGetCurrentUsage_OrgNotFound(t *testing.T) {
	reporter, orgLookup, _, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_nonexistent").
		Return(types.PlanTier(""), types.PlanLimits{},
			types.NewAppError(types.ErrCodeNotFoundOrg, "org not found", nil))

	snapshot, err := reporter.GetCurrentUsage(context.Background(), "org_nonexistent")
	require.Error(t, err)
	assert.Nil(t, snapshot)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundOrg, appErr.Code)
}

func TestGetCurrentUsage_APICallsError(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanPro, types.PlanLimits{
			MaxWatchPoints:   100,
			MaxAPICallsDaily: 1000,
			AllowNowcast:     true,
		}, nil)

	usageDB.On("GetAPICallsCount", mock.Anything, "org_1").
		Return(0, types.NewAppError(types.ErrCodeInternalDB, "db error", nil))

	snapshot, err := reporter.GetCurrentUsage(context.Background(), "org_1")
	require.Error(t, err)
	assert.Nil(t, snapshot)
}

func TestGetCurrentUsage_WatchPointCountError(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanPro, types.PlanLimits{
			MaxWatchPoints:   100,
			MaxAPICallsDaily: 1000,
			AllowNowcast:     true,
		}, nil)

	usageDB.On("GetAPICallsCount", mock.Anything, "org_1").Return(350, nil)
	usageDB.On("CountActiveWatchPoints", mock.Anything, "org_1").
		Return(0, types.NewAppError(types.ErrCodeInternalDB, "db error", nil))

	snapshot, err := reporter.GetCurrentUsage(context.Background(), "org_1")
	require.Error(t, err)
	assert.Nil(t, snapshot)
}

func TestGetCurrentUsage_NoPeriodEnd(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanFree, types.PlanLimits{
			MaxWatchPoints:   3,
			MaxAPICallsDaily: 0,
			AllowNowcast:     false,
		}, nil)

	usageDB.On("GetAPICallsCount", mock.Anything, "org_1").Return(0, nil)
	usageDB.On("CountActiveWatchPoints", mock.Anything, "org_1").Return(1, nil)
	usageDB.On("GetRateLimitPeriodEnd", mock.Anything, "org_1").Return((*time.Time)(nil), nil)

	snapshot, err := reporter.GetCurrentUsage(context.Background(), "org_1")
	require.NoError(t, err)
	require.NotNil(t, snapshot)

	assert.Equal(t, 1, snapshot.ResourceUsage[types.ResourceWatchPoints])
	assert.Equal(t, 0, snapshot.ResourceUsage[types.ResourceAPICalls])
	assert.Nil(t, snapshot.LimitDetails[types.ResourceAPICalls].NextReset)
}

func TestGetCurrentUsage_FallsBackToPlanRegistry(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	// Simulate DB returning zero-value PlanLimits (e.g., plan_limits column is empty/null)
	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanPro, types.PlanLimits{}, nil)

	usageDB.On("GetAPICallsCount", mock.Anything, "org_1").Return(50, nil)
	usageDB.On("CountActiveWatchPoints", mock.Anything, "org_1").Return(10, nil)
	usageDB.On("GetRateLimitPeriodEnd", mock.Anything, "org_1").Return((*time.Time)(nil), nil)

	snapshot, err := reporter.GetCurrentUsage(context.Background(), "org_1")
	require.NoError(t, err)
	require.NotNil(t, snapshot)

	// Should have fallen back to PlanRegistry's Pro limits
	assert.Equal(t, 100, snapshot.LimitDetails[types.ResourceWatchPoints].Limit)
	assert.Equal(t, 1000, snapshot.LimitDetails[types.ResourceAPICalls].Limit)
}

// --- GetUsageHistory Tests ---

func TestGetUsageHistory_Success(t *testing.T) {
	reporter, _, _, historyRepo := setupReporter()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

	historicalData := []types.DailyUsageStat{
		{Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), APICalls: 100, ActiveWatchPoints: 10, NotificationsSent: 5},
		{Date: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), APICalls: 200, ActiveWatchPoints: 12, NotificationsSent: 8},
	}

	historyRepo.On("Query", mock.Anything, "org_1", start, end, types.GranularityDaily).
		Return(historicalData, nil)

	// Current day's live data won't be fetched since the range doesn't cover "today"
	// (2026-02-06 is outside Jan 1-31)

	result, err := reporter.GetUsageHistory(context.Background(), "org_1", start, end, types.GranularityDaily)
	require.NoError(t, err)
	require.Len(t, result, 2)

	assert.Equal(t, 100, result[0].Value)
	assert.Equal(t, 200, result[1].Value)

	historyRepo.AssertExpectations(t)
}

func TestGetUsageHistory_WithLiveDataOverlay(t *testing.T) {
	reporter, _, usageDB, historyRepo := setupReporter()

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	start := today.AddDate(0, 0, -7)
	end := today

	yesterday := today.AddDate(0, 0, -1)
	historicalData := []types.DailyUsageStat{
		{Date: yesterday, APICalls: 100, ActiveWatchPoints: 10, NotificationsSent: 5},
	}

	historyRepo.On("Query", mock.Anything, "org_1", start, end, types.GranularityDaily).
		Return(historicalData, nil)

	// Should fetch live data for today
	usageDB.On("GetAPICallsCount", mock.Anything, "org_1").Return(75, nil)

	result, err := reporter.GetUsageHistory(context.Background(), "org_1", start, end, types.GranularityDaily)
	require.NoError(t, err)
	require.Len(t, result, 2) // yesterday's historical + today's live

	assert.Equal(t, 100, result[0].Value) // historical
	assert.Equal(t, 75, result[1].Value)  // live

	historyRepo.AssertExpectations(t)
	usageDB.AssertExpectations(t)
}

func TestGetUsageHistory_HistoryRepoError(t *testing.T) {
	reporter, _, _, historyRepo := setupReporter()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

	historyRepo.On("Query", mock.Anything, "org_1", start, end, types.GranularityDaily).
		Return(nil, types.NewAppError(types.ErrCodeInternalDB, "db error", nil))

	result, err := reporter.GetUsageHistory(context.Background(), "org_1", start, end, types.GranularityDaily)
	require.Error(t, err)
	assert.Nil(t, result)
}

func TestGetUsageHistory_EmptyHistory(t *testing.T) {
	reporter, _, _, historyRepo := setupReporter()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

	historyRepo.On("Query", mock.Anything, "org_1", start, end, types.GranularityMonthly).
		Return([]types.DailyUsageStat{}, nil)

	result, err := reporter.GetUsageHistory(context.Background(), "org_1", start, end, types.GranularityMonthly)
	require.NoError(t, err)
	assert.Empty(t, result)
}

// --- CheckLimit Tests ---

func TestCheckLimit_WatchPointsAllowed(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanPro, types.PlanLimits{MaxWatchPoints: 100}, nil)

	usageDB.On("CountActiveWatchPoints", mock.Anything, "org_1").Return(50, nil)

	err := reporter.CheckLimit(context.Background(), "org_1", types.ResourceWatchPoints, 1)
	require.NoError(t, err)

	orgLookup.AssertExpectations(t)
	usageDB.AssertExpectations(t)
}

func TestCheckLimit_WatchPointsExceeded(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanFree, types.PlanLimits{MaxWatchPoints: 3}, nil)

	usageDB.On("CountActiveWatchPoints", mock.Anything, "org_1").Return(3, nil)

	err := reporter.CheckLimit(context.Background(), "org_1", types.ResourceWatchPoints, 1)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeLimitWatchpoints, appErr.Code)
	assert.Equal(t, 3, appErr.Details["current"])
	assert.Equal(t, 3, appErr.Details["limit"])
	assert.Equal(t, "free", appErr.Details["plan"])
}

func TestCheckLimit_WatchPointsExactlyAtLimit(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	// At exactly the limit (3 out of 3), adding 1 more should fail
	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanFree, types.PlanLimits{MaxWatchPoints: 3}, nil)

	usageDB.On("CountActiveWatchPoints", mock.Anything, "org_1").Return(3, nil)

	err := reporter.CheckLimit(context.Background(), "org_1", types.ResourceWatchPoints, 1)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeLimitWatchpoints, appErr.Code)
}

func TestCheckLimit_WatchPointsBulkExceeded(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	// 50 existing + 55 new = 105 > 100
	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanPro, types.PlanLimits{MaxWatchPoints: 100}, nil)

	usageDB.On("CountActiveWatchPoints", mock.Anything, "org_1").Return(50, nil)

	err := reporter.CheckLimit(context.Background(), "org_1", types.ResourceWatchPoints, 55)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeLimitWatchpoints, appErr.Code)
}

func TestCheckLimit_WatchPointsUnlimited(t *testing.T) {
	reporter, orgLookup, _, _ := setupReporter()

	// Enterprise plan: MaxWatchPoints = 0 means unlimited
	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanEnterprise, types.PlanLimits{MaxWatchPoints: 0}, nil)

	err := reporter.CheckLimit(context.Background(), "org_1", types.ResourceWatchPoints, 1000)
	require.NoError(t, err)

	// CountActiveWatchPoints should NOT be called for unlimited plans
	orgLookup.AssertExpectations(t)
}

func TestCheckLimit_APICallsAllowed(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanPro, types.PlanLimits{MaxAPICallsDaily: 1000}, nil)

	usageDB.On("GetAPICallsCount", mock.Anything, "org_1").Return(500, nil)

	err := reporter.CheckLimit(context.Background(), "org_1", types.ResourceAPICalls, 1)
	require.NoError(t, err)
}

func TestCheckLimit_APICallsExceeded(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanStarter, types.PlanLimits{MaxAPICallsDaily: 100}, nil)

	usageDB.On("GetAPICallsCount", mock.Anything, "org_1").Return(100, nil)

	err := reporter.CheckLimit(context.Background(), "org_1", types.ResourceAPICalls, 1)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeLimitAPICalls, appErr.Code)
}

func TestCheckLimit_APICallsUnlimited(t *testing.T) {
	reporter, orgLookup, _, _ := setupReporter()

	// Enterprise plan: MaxAPICallsDaily = 0 means unlimited
	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanEnterprise, types.PlanLimits{MaxAPICallsDaily: 0}, nil)

	err := reporter.CheckLimit(context.Background(), "org_1", types.ResourceAPICalls, 1)
	require.NoError(t, err)
}

func TestCheckLimit_UnknownResource(t *testing.T) {
	reporter, orgLookup, _, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanPro, types.PlanLimits{}, nil)

	err := reporter.CheckLimit(context.Background(), "org_1", types.ResourceType("unknown"), 1)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalUnexpected, appErr.Code)
}

func TestCheckLimit_OrgLookupError(t *testing.T) {
	reporter, orgLookup, _, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_bad").
		Return(types.PlanTier(""), types.PlanLimits{},
			types.NewAppError(types.ErrCodeNotFoundOrg, "not found", nil))

	err := reporter.CheckLimit(context.Background(), "org_bad", types.ResourceWatchPoints, 1)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundOrg, appErr.Code)
}

func TestCheckLimit_CountDBError(t *testing.T) {
	reporter, orgLookup, usageDB, _ := setupReporter()

	orgLookup.On("GetPlanAndLimits", mock.Anything, "org_1").
		Return(types.PlanPro, types.PlanLimits{MaxWatchPoints: 100}, nil)

	usageDB.On("CountActiveWatchPoints", mock.Anything, "org_1").
		Return(0, types.NewAppError(types.ErrCodeInternalDB, "db timeout", nil))

	err := reporter.CheckLimit(context.Background(), "org_1", types.ResourceWatchPoints, 1)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

// --- Interface Compliance Tests ---

func TestUsageReporterInterface(t *testing.T) {
	var _ UsageReporter = (*usageReporterImpl)(nil)
}

func TestUsageEnforcerInterface(t *testing.T) {
	var _ UsageEnforcer = (*usageReporterImpl)(nil)
}
