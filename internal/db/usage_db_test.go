package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/types"
)

// --- UsageDBImpl Tests ---

func TestUsageDBImpl_CountActiveWatchPoints_Success(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanFn: func(dest ...any) error {
				p := dest[0].(*int)
				*p = 42
				return nil
			},
		})

	count, err := impl.CountActiveWatchPoints(context.Background(), "org_1")
	require.NoError(t, err)
	assert.Equal(t, 42, count)
	db.AssertExpectations(t)
}

func TestUsageDBImpl_CountActiveWatchPoints_DBError(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanErr: errors.New("connection refused"),
		})

	count, err := impl.CountActiveWatchPoints(context.Background(), "org_1")
	require.Error(t, err)
	assert.Equal(t, 0, count)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestUsageDBImpl_GetAPICallsCount_Success(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanFn: func(dest ...any) error {
				p := dest[0].(*int)
				*p = 350
				return nil
			},
		})

	count, err := impl.GetAPICallsCount(context.Background(), "org_1")
	require.NoError(t, err)
	assert.Equal(t, 350, count)
	db.AssertExpectations(t)
}

func TestUsageDBImpl_GetAPICallsCount_NoRows(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	// COALESCE ensures we get 0 even with no rows, but scan should still succeed
	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanFn: func(dest ...any) error {
				p := dest[0].(*int)
				*p = 0
				return nil
			},
		})

	count, err := impl.GetAPICallsCount(context.Background(), "org_1")
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestUsageDBImpl_GetAPICallsCount_DBError(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanErr: errors.New("timeout"),
		})

	count, err := impl.GetAPICallsCount(context.Background(), "org_1")
	require.Error(t, err)
	assert.Equal(t, 0, count)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestUsageDBImpl_GetRateLimitPeriodEnd_Success(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	expectedTime := time.Date(2026, 2, 7, 0, 0, 0, 0, time.UTC)

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanFn: func(dest ...any) error {
				p := dest[0].(**time.Time)
				*p = &expectedTime
				return nil
			},
		})

	result, err := impl.GetRateLimitPeriodEnd(context.Background(), "org_1")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, expectedTime, *result)
	db.AssertExpectations(t)
}

func TestUsageDBImpl_GetRateLimitPeriodEnd_Nil(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanFn: func(dest ...any) error {
				p := dest[0].(**time.Time)
				*p = nil
				return nil
			},
		})

	result, err := impl.GetRateLimitPeriodEnd(context.Background(), "org_1")
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestUsageDBImpl_GetRateLimitPeriodEnd_NoRows(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanErr: pgx.ErrNoRows,
		})

	result, err := impl.GetRateLimitPeriodEnd(context.Background(), "org_nonexistent")
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestUsageDBImpl_GetRateLimitPeriodEnd_DBError(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanErr: errors.New("connection error"),
		})

	result, err := impl.GetRateLimitPeriodEnd(context.Background(), "org_1")
	require.Error(t, err)
	assert.Nil(t, result)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestUsageDBImpl_GetPlanAndLimits_Success(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	expectedLimits := types.PlanLimits{
		MaxWatchPoints:   100,
		MaxAPICallsDaily: 1000,
		AllowNowcast:     true,
	}

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanFn: func(dest ...any) error {
				p := dest[0].(*types.PlanTier)
				*p = types.PlanPro
				l := dest[1].(*types.PlanLimits)
				*l = expectedLimits
				return nil
			},
		})

	plan, limits, err := impl.GetPlanAndLimits(context.Background(), "org_1")
	require.NoError(t, err)
	assert.Equal(t, types.PlanPro, plan)
	assert.Equal(t, expectedLimits, limits)
	db.AssertExpectations(t)
}

func TestUsageDBImpl_GetPlanAndLimits_NotFound(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanErr: pgx.ErrNoRows,
		})

	plan, limits, err := impl.GetPlanAndLimits(context.Background(), "org_nonexistent")
	require.Error(t, err)
	assert.Equal(t, types.PlanTier(""), plan)
	assert.Equal(t, types.PlanLimits{}, limits)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundOrg, appErr.Code)
}

func TestUsageDBImpl_GetPlanAndLimits_DBError(t *testing.T) {
	db := new(mockDBTX)
	impl := NewUsageDBImpl(db)

	db.On("QueryRow", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(&mockRow{
			scanErr: errors.New("timeout"),
		})

	plan, limits, err := impl.GetPlanAndLimits(context.Background(), "org_1")
	require.Error(t, err)
	assert.Equal(t, types.PlanTier(""), plan)
	assert.Equal(t, types.PlanLimits{}, limits)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}
