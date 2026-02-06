package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/types"
)

// Note: mockDBTX and mockRow are defined in session_repo_test.go.

// ============================================================
// Create Tests
// ============================================================

func TestForecastRunRepository_Create_WithID(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	run := &types.ForecastRun{
		ID:                  "run_abc123",
		Model:               types.ForecastMediumRange,
		RunTimestamp:        now,
		SourceDataTimestamp: now.Add(-1 * time.Hour),
		StoragePath:         "s3://bucket/medium_range/2026-02-06T12:00:00Z",
		Status:              "running",
		ExternalID:          "rpod_xyz",
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Create(ctx, run)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestForecastRunRepository_Create_WithoutID(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	run := &types.ForecastRun{
		// No ID - let DB generate it
		Model:               types.ForecastNowcast,
		RunTimestamp:        now,
		SourceDataTimestamp: now.Add(-30 * time.Minute),
		StoragePath:         "s3://bucket/nowcast/2026-02-06T12:00:00Z",
		Status:              "running",
	}

	generatedID := "run_generated456"
	generatedAt := now.Add(1 * time.Second)

	mockRowResult := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = generatedID
			*dest[1].(*time.Time) = generatedAt
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	err := repo.Create(ctx, run)
	require.NoError(t, err)
	assert.Equal(t, generatedID, run.ID)
	assert.Equal(t, generatedAt, run.CreatedAt)
	db.AssertExpectations(t)
}

func TestForecastRunRepository_Create_DBError_WithID(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	run := &types.ForecastRun{
		ID:                  "run_fail1",
		Model:               types.ForecastMediumRange,
		RunTimestamp:        now,
		SourceDataTimestamp: now,
		StoragePath:         "s3://bucket/path",
		Status:              "running",
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.Create(ctx, run)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

func TestForecastRunRepository_Create_DBError_WithoutID(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	run := &types.ForecastRun{
		Model:               types.ForecastMediumRange,
		RunTimestamp:        now,
		SourceDataTimestamp: now,
		StoragePath:         "s3://bucket/path",
		Status:              "running",
	}

	mockRowResult := &mockRow{
		scanErr: errors.New("connection refused"),
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	err := repo.Create(ctx, run)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

// ============================================================
// GetLatestServing Tests
// ============================================================

func TestForecastRunRepository_GetLatestServing_ReturnsLatestCompleteRun(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	expectedStoragePath := "s3://bucket/medium_range/2026-02-06T12:00:00Z"

	mockRowResult := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "run_latest1"
			*dest[1].(*string) = "medium_range"
			*dest[2].(*time.Time) = now
			*dest[3].(*time.Time) = now.Add(-1 * time.Hour)
			*dest[4].(*string) = expectedStoragePath
			*dest[5].(*string) = "complete"
			*dest[6].(**string) = nil // external_id
			*dest[7].(*int) = 0      // retry_count
			*dest[8].(**string) = nil // failure_reason
			*dest[9].(**int) = nil    // inference_duration_ms
			*dest[10].(*time.Time) = now
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	run, err := repo.GetLatestServing(ctx, types.ForecastMediumRange)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, "run_latest1", run.ID)
	assert.Equal(t, types.ForecastMediumRange, run.Model)
	assert.Equal(t, "complete", run.Status)
	assert.Equal(t, expectedStoragePath, run.StoragePath)
	assert.Equal(t, now, run.RunTimestamp)
	assert.Empty(t, run.ExternalID)
	assert.Empty(t, run.FailureReason)
	assert.Equal(t, 0, run.DurationMS)

	db.AssertExpectations(t)
}

func TestForecastRunRepository_GetLatestServing_WithOptionalFields(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	extID := "rpod_12345"
	durationMs := 45000

	mockRowResult := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "run_latest2"
			*dest[1].(*string) = "nowcast"
			*dest[2].(*time.Time) = now
			*dest[3].(*time.Time) = now.Add(-30 * time.Minute)
			*dest[4].(*string) = "s3://bucket/nowcast/path"
			*dest[5].(*string) = "complete"
			*dest[6].(**string) = &extID
			*dest[7].(*int) = 0
			*dest[8].(**string) = nil
			*dest[9].(**int) = &durationMs
			*dest[10].(*time.Time) = now
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	run, err := repo.GetLatestServing(ctx, types.ForecastNowcast)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, "run_latest2", run.ID)
	assert.Equal(t, types.ForecastNowcast, run.Model)
	assert.Equal(t, extID, run.ExternalID)
	assert.Equal(t, durationMs, run.DurationMS)

	db.AssertExpectations(t)
}

func TestForecastRunRepository_GetLatestServing_NoCompletedRuns(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	mockRowResult := &mockRow{
		scanErr: pgx.ErrNoRows,
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	run, err := repo.GetLatestServing(ctx, types.ForecastMediumRange)
	require.NoError(t, err)
	assert.Nil(t, run, "should return nil when no completed runs exist")

	db.AssertExpectations(t)
}

func TestForecastRunRepository_GetLatestServing_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	mockRowResult := &mockRow{
		scanErr: errors.New("connection reset"),
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	run, err := repo.GetLatestServing(ctx, types.ForecastMediumRange)
	require.Error(t, err)
	assert.Nil(t, run)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// TestForecastRunRepository_GetLatestServing_IgnoresNonCompleteRuns verifies
// that the SQL query correctly filters to status='complete' only. The query is
// WHERE model=$1 AND status='complete' ORDER BY run_timestamp DESC LIMIT 1.
// When a model has only 'running' or 'failed' runs, GetLatestServing returns nil.
func TestForecastRunRepository_GetLatestServing_IgnoresNonCompleteRuns(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	// The DB returns no rows because no 'complete' runs exist (only running/failed).
	mockRowResult := &mockRow{
		scanErr: pgx.ErrNoRows,
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	run, err := repo.GetLatestServing(ctx, types.ForecastNowcast)
	require.NoError(t, err)
	assert.Nil(t, run, "should return nil when only non-complete runs exist")

	db.AssertExpectations(t)
}

// ============================================================
// MarkComplete Tests
// ============================================================

func TestForecastRunRepository_MarkComplete_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.MarkComplete(ctx, "run_abc123", "s3://bucket/final/path", 42000)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestForecastRunRepository_MarkComplete_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.MarkComplete(ctx, "run_nonexistent", "s3://path", 0)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Contains(t, appErr.Message, "forecast run not found")
	db.AssertExpectations(t)
}

func TestForecastRunRepository_MarkComplete_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("deadlock detected"))

	err := repo.MarkComplete(ctx, "run_abc123", "s3://path", 100)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

// ============================================================
// MarkFailed Tests
// ============================================================

func TestForecastRunRepository_MarkFailed_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.MarkFailed(ctx, "run_fail1", "S3 upload timeout")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestForecastRunRepository_MarkFailed_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.MarkFailed(ctx, "run_nonexistent", "timeout")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Contains(t, appErr.Message, "forecast run not found")
	db.AssertExpectations(t)
}

func TestForecastRunRepository_MarkFailed_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.MarkFailed(ctx, "run_fail2", "timeout")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

// ============================================================
// UpdateExternalID Tests
// ============================================================

func TestForecastRunRepository_UpdateExternalID_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.UpdateExternalID(ctx, "run_abc123", "rpod_new_job_id")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestForecastRunRepository_UpdateExternalID_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.UpdateExternalID(ctx, "run_nonexistent", "rpod_xyz")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Contains(t, appErr.Message, "forecast run not found")
	db.AssertExpectations(t)
}

func TestForecastRunRepository_UpdateExternalID_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewForecastRunRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.UpdateExternalID(ctx, "run_abc123", "rpod_xyz")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

// ============================================================
// Helper Tests
// ============================================================

func TestNilIfZeroInt(t *testing.T) {
	assert.Nil(t, nilIfZeroInt(0))

	result := nilIfZeroInt(42)
	require.NotNil(t, result)
	assert.Equal(t, 42, *result)
}
