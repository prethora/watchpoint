package db

import (
	"context"
	"encoding/json"
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

// Note: mockDBTX, mockRow, and mockRows are defined in session_repo_test.go
// and usage_repo_test.go and reused here.

// ============================================================
// JobLockRepository Tests
// ============================================================

func TestJobLockRepository_Acquire_Success_NewLock(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobLockRepository(db)
	ctx := context.Background()

	// INSERT succeeds (new lock row created) -> 1 row affected
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	acquired, err := repo.Acquire(ctx, "archive_watchpoints:2026-02-06T03", "lambda-req-123", 15*time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired)
	db.AssertExpectations(t)
}

func TestJobLockRepository_Acquire_Success_ExpiredLockReclaimed(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobLockRepository(db)
	ctx := context.Background()

	// ON CONFLICT DO UPDATE succeeds (expired lock reclaimed) -> 1 row affected
	// The UPDATE tag text varies by driver; pgconn uses "INSERT" even for upserts
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	acquired, err := repo.Acquire(ctx, "verification:2026-02-06T03", "lambda-req-456", 30*time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired)
	db.AssertExpectations(t)
}

func TestJobLockRepository_Acquire_AlreadyLocked(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobLockRepository(db)
	ctx := context.Background()

	// Lock exists and has not expired -> 0 rows affected
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 0"), nil)

	acquired, err := repo.Acquire(ctx, "archive_watchpoints:2026-02-06T03", "lambda-req-789", 15*time.Minute)
	require.NoError(t, err)
	assert.False(t, acquired, "should not acquire lock when another worker holds it")
	db.AssertExpectations(t)
}

func TestJobLockRepository_Acquire_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobLockRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	acquired, err := repo.Acquire(ctx, "task:key", "worker-1", 10*time.Minute)
	require.Error(t, err)
	assert.False(t, acquired)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

func TestJobLockRepository_Acquire_ExpiresAtComputedFromTTL(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobLockRepository(db)
	ctx := context.Background()

	// Verify that locked_at and expires_at are passed as time.Time values,
	// and that expires_at is approximately locked_at + TTL.
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.MatchedBy(func(args []any) bool {
		if len(args) < 4 {
			return false
		}
		lockedAt, ok1 := args[2].(time.Time)
		expiresAt, ok2 := args[3].(time.Time)
		if !ok1 || !ok2 {
			return false
		}
		// expires_at should be approximately locked_at + 1 hour (within 2 seconds tolerance)
		diff := expiresAt.Sub(lockedAt)
		return diff >= 59*time.Minute && diff <= 61*time.Minute
	})).Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	acquired, err := repo.Acquire(ctx, "digest:key", "worker-x", 1*time.Hour)
	require.NoError(t, err)
	assert.True(t, acquired)
	db.AssertExpectations(t)
}

// ============================================================
// JobHistoryRepository Tests
// ============================================================

func TestJobHistoryRepository_Start_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobHistoryRepository(db)
	ctx := context.Background()

	mockRowResult := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*int64) = 42
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	id, err := repo.Start(ctx, "archive_watchpoints")
	require.NoError(t, err)
	assert.Equal(t, int64(42), id)
	db.AssertExpectations(t)
}

func TestJobHistoryRepository_Start_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobHistoryRepository(db)
	ctx := context.Background()

	mockRowResult := &mockRow{
		scanErr: errors.New("connection reset"),
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	id, err := repo.Start(ctx, "verification")
	require.Error(t, err)
	assert.Equal(t, int64(0), id)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

func TestJobHistoryRepository_Finish_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobHistoryRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.Finish(ctx, 42, "success", 15, nil)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestJobHistoryRepository_Finish_WithError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobHistoryRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.MatchedBy(func(args []any) bool {
		// Verify the error message is passed as 4th argument (index 3)
		if len(args) < 4 {
			return false
		}
		errMsg, ok := args[3].(*string)
		return ok && errMsg != nil && *errMsg == "S3 bucket not found"
	})).Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	jobErr := errors.New("S3 bucket not found")
	err := repo.Finish(ctx, 42, "failed", 0, jobErr)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestJobHistoryRepository_Finish_NilErrorPassesNilParam(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobHistoryRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.MatchedBy(func(args []any) bool {
		if len(args) < 4 {
			return false
		}
		errMsg, ok := args[3].(*string)
		return ok && errMsg == nil
	})).Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.Finish(ctx, 99, "success", 50, nil)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestJobHistoryRepository_Finish_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobHistoryRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.Finish(ctx, 999, "success", 0, nil)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalUnexpected, appErr.Code)
	assert.Contains(t, appErr.Message, "job history entry not found")
	db.AssertExpectations(t)
}

func TestJobHistoryRepository_Finish_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewJobHistoryRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("deadlock detected"))

	err := repo.Finish(ctx, 42, "failed", 0, nil)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

// ============================================================
// CalibrationRepository Tests
// ============================================================

// calibMockRows implements pgx.Rows for testing CalibrationRepository.GetAll.
type calibMockRows struct {
	data    []types.CalibrationCoefficients
	idx     int
	closed  bool
	scanErr error
	errVal  error
}

func (r *calibMockRows) Next() bool {
	if r.closed {
		return false
	}
	r.idx++
	return r.idx < len(r.data)
}

func (r *calibMockRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.data[r.idx]
	*dest[0].(*string) = row.LocationID
	*dest[1].(*float64) = row.HighThreshold
	*dest[2].(*float64) = row.HighSlope
	*dest[3].(*float64) = row.HighBase
	*dest[4].(*float64) = row.MidThreshold
	*dest[5].(*float64) = row.MidSlope
	*dest[6].(*float64) = row.MidBase
	*dest[7].(*float64) = row.LowBase
	*dest[8].(*time.Time) = row.UpdatedAt
	return nil
}

func (r *calibMockRows) Close()                                       { r.closed = true }
func (r *calibMockRows) Err() error                                   { return r.errVal }
func (r *calibMockRows) CommandTag() pgconn.CommandTag                 { return pgconn.CommandTag{} }
func (r *calibMockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *calibMockRows) RawValues() [][]byte                           { return nil }
func (r *calibMockRows) Values() ([]any, error)                        { return nil, nil }
func (r *calibMockRows) Conn() *pgx.Conn                              { return nil }

func TestCalibrationRepository_GetAll_ReturnsMap(t *testing.T) {
	db := new(mockDBTX)
	repo := NewCalibrationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 3, 0, 0, 0, time.UTC)
	rows := &calibMockRows{
		data: []types.CalibrationCoefficients{
			{
				LocationID:    "3.2",
				HighThreshold: 0.85,
				HighSlope:     1.2,
				HighBase:      0.1,
				MidThreshold:  0.5,
				MidSlope:      0.8,
				MidBase:       0.05,
				LowBase:       0.02,
				UpdatedAt:     now,
			},
			{
				LocationID:    "4.3",
				HighThreshold: 0.9,
				HighSlope:     1.1,
				HighBase:      0.15,
				MidThreshold:  0.55,
				MidSlope:      0.75,
				MidBase:       0.06,
				LowBase:       0.03,
				UpdatedAt:     now.Add(-24 * time.Hour),
			},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	result, err := repo.GetAll(ctx)
	require.NoError(t, err)
	assert.Len(t, result, 2)

	// Verify map keys and values
	c1, ok := result["3.2"]
	require.True(t, ok, "should contain location 3.2")
	assert.Equal(t, 0.85, c1.HighThreshold)
	assert.Equal(t, 1.2, c1.HighSlope)
	assert.Equal(t, 0.1, c1.HighBase)
	assert.Equal(t, 0.5, c1.MidThreshold)
	assert.Equal(t, 0.8, c1.MidSlope)
	assert.Equal(t, 0.05, c1.MidBase)
	assert.Equal(t, 0.02, c1.LowBase)
	assert.Equal(t, now, c1.UpdatedAt)

	c2, ok := result["4.3"]
	require.True(t, ok, "should contain location 4.3")
	assert.Equal(t, 0.9, c2.HighThreshold)

	db.AssertExpectations(t)
}

func TestCalibrationRepository_GetAll_EmptyReturnsEmptyMap(t *testing.T) {
	db := new(mockDBTX)
	repo := NewCalibrationRepository(db)
	ctx := context.Background()

	rows := &calibMockRows{data: nil, idx: -1}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	result, err := repo.GetAll(ctx)
	require.NoError(t, err)
	assert.NotNil(t, result, "should return empty map, not nil")
	assert.Len(t, result, 0)
	db.AssertExpectations(t)
}

func TestCalibrationRepository_GetAll_QueryError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewCalibrationRepository(db)
	ctx := context.Background()

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(nil, errors.New("connection refused"))

	result, err := repo.GetAll(ctx)
	require.Error(t, err)
	assert.Nil(t, result)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

func TestCalibrationRepository_GetAll_ScanError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewCalibrationRepository(db)
	ctx := context.Background()

	rows := &calibMockRows{
		data:    []types.CalibrationCoefficients{{}},
		idx:     -1,
		scanErr: errors.New("type mismatch"),
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	result, err := repo.GetAll(ctx)
	require.Error(t, err)
	assert.Nil(t, result)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

func TestCalibrationRepository_GetAll_RowsError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewCalibrationRepository(db)
	ctx := context.Background()

	rows := &calibMockRows{
		data:   nil,
		idx:    -1,
		errVal: errors.New("network interrupted"),
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	result, err := repo.GetAll(ctx)
	require.Error(t, err)
	assert.Nil(t, result)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	assert.Contains(t, appErr.Message, "iterating calibration")
	db.AssertExpectations(t)
}

func TestCalibrationRepository_UpdateCoefficients_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewCalibrationRepository(db)
	ctx := context.Background()

	c := &types.CalibrationCoefficients{
		LocationID:    "3.2",
		HighThreshold: 0.85,
		HighSlope:     1.2,
		HighBase:      0.1,
		MidThreshold:  0.5,
		MidSlope:      0.8,
		MidBase:       0.05,
		LowBase:       0.02,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.UpdateCoefficients(ctx, c)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestCalibrationRepository_UpdateCoefficients_UpsertExisting(t *testing.T) {
	db := new(mockDBTX)
	repo := NewCalibrationRepository(db)
	ctx := context.Background()

	c := &types.CalibrationCoefficients{
		LocationID:    "3.2",
		HighThreshold: 0.9, // Updated value
		HighSlope:     1.3,
		HighBase:      0.12,
		MidThreshold:  0.55,
		MidSlope:      0.85,
		MidBase:       0.06,
		LowBase:       0.025,
	}

	// ON CONFLICT DO UPDATE returns UPDATE tag
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.UpdateCoefficients(ctx, c)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestCalibrationRepository_UpdateCoefficients_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewCalibrationRepository(db)
	ctx := context.Background()

	c := &types.CalibrationCoefficients{
		LocationID:    "3.2",
		HighThreshold: 0.85,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("timeout"))

	err := repo.UpdateCoefficients(ctx, c)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

func TestCalibrationRepository_CreateCandidate_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewCalibrationRepository(db)
	ctx := context.Background()

	proposed := map[string]any{
		"high_threshold": 0.95,
		"high_slope":     1.8,
	}
	coeffJSON, _ := json.Marshal(proposed)

	mockRowResult := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*int64) = 17
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	id, err := repo.CreateCandidate(ctx, "3.2", coeffJSON, "delta_exceeded_15_percent")
	require.NoError(t, err)
	assert.Equal(t, int64(17), id)
	db.AssertExpectations(t)
}

func TestCalibrationRepository_CreateCandidate_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewCalibrationRepository(db)
	ctx := context.Background()

	mockRowResult := &mockRow{
		scanErr: errors.New("constraint violation"),
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	id, err := repo.CreateCandidate(ctx, "3.2", []byte(`{}`), "delta_exceeded_15_percent")
	require.Error(t, err)
	assert.Equal(t, int64(0), id)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

// ============================================================
// VerificationRepository Tests
// ============================================================

func TestVerificationRepository_Save_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 3, 0, 0, 0, time.UTC)
	results := []types.VerificationResult{
		{
			ForecastRunID: "run_abc123",
			LocationID:    "3.2",
			MetricType:    "rmse",
			Variable:      "temperature_c",
			Value:         2.5,
			ComputedAt:    now,
		},
		{
			ForecastRunID: "run_abc123",
			LocationID:    "3.2",
			MetricType:    "bias",
			Variable:      "temperature_c",
			Value:         -0.3,
			ComputedAt:    now,
		},
		{
			ForecastRunID: "run_abc123",
			LocationID:    "4.3",
			MetricType:    "brier",
			Variable:      "precipitation_probability",
			Value:         0.12,
			ComputedAt:    now,
		},
	}

	// Expect 3 Exec calls (one per result)
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil).Times(3)

	err := repo.Save(ctx, results)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestVerificationRepository_Save_EmptySlice(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	err := repo.Save(ctx, []types.VerificationResult{})
	require.NoError(t, err)
	// No DB calls should be made
	db.AssertNotCalled(t, "Exec", mock.Anything, mock.Anything, mock.Anything)
}

func TestVerificationRepository_Save_NilSlice(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	err := repo.Save(ctx, nil)
	require.NoError(t, err)
	db.AssertNotCalled(t, "Exec", mock.Anything, mock.Anything, mock.Anything)
}

func TestVerificationRepository_Save_DBError_FirstItem(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	results := []types.VerificationResult{
		{
			ForecastRunID: "run_abc123",
			LocationID:    "3.2",
			MetricType:    "rmse",
			Variable:      "temperature_c",
			Value:         2.5,
		},
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("foreign key violation"))

	err := repo.Save(ctx, results)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

func TestVerificationRepository_Save_DBError_PartialFailure(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	results := []types.VerificationResult{
		{ForecastRunID: "run_1", LocationID: "3.2", MetricType: "rmse", Variable: "temp", Value: 1.0},
		{ForecastRunID: "run_1", LocationID: "3.2", MetricType: "bias", Variable: "temp", Value: 0.5},
	}

	// First insert succeeds, second fails
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil).Once()
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("disk full")).Once()

	err := repo.Save(ctx, results)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

func TestVerificationRepository_Save_ZeroComputedAt_UsesDefault(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	results := []types.VerificationResult{
		{
			ForecastRunID: "run_abc123",
			LocationID:    "3.2",
			MetricType:    "rmse",
			Variable:      "temperature_c",
			Value:         2.5,
			// ComputedAt is zero -> nilIfZeroTime returns nil -> DB uses NOW()
		},
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.MatchedBy(func(args []any) bool {
		// The 6th argument (index 5) should be nil for zero time
		if len(args) < 6 {
			return false
		}
		timePtr, ok := args[5].(*time.Time)
		return ok && timePtr == nil
	})).Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Save(ctx, results)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

// verificationMetricMockRows implements pgx.Rows for GetAggregatedMetrics tests.
type verificationMetricMockRows struct {
	data    []verificationMetricRowData
	idx     int
	closed  bool
	scanErr error
	errVal  error
}

type verificationMetricRowData struct {
	model      string
	variable   string
	metricType string
	avgValue   float64
	maxTime    time.Time
}

func (r *verificationMetricMockRows) Next() bool {
	if r.closed {
		return false
	}
	r.idx++
	return r.idx < len(r.data)
}

func (r *verificationMetricMockRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.data[r.idx]
	*dest[0].(*string) = row.model
	*dest[1].(*string) = row.variable
	*dest[2].(*string) = row.metricType
	*dest[3].(*float64) = row.avgValue
	*dest[4].(*time.Time) = row.maxTime
	return nil
}

func (r *verificationMetricMockRows) Close()                                       { r.closed = true }
func (r *verificationMetricMockRows) Err() error                                   { return r.errVal }
func (r *verificationMetricMockRows) CommandTag() pgconn.CommandTag                 { return pgconn.CommandTag{} }
func (r *verificationMetricMockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *verificationMetricMockRows) RawValues() [][]byte                           { return nil }
func (r *verificationMetricMockRows) Values() ([]any, error)                        { return nil, nil }
func (r *verificationMetricMockRows) Conn() *pgx.Conn                              { return nil }

func TestVerificationRepository_GetAggregatedMetrics_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 3, 0, 0, 0, time.UTC)
	start := now.Add(-48 * time.Hour)
	end := now.Add(-24 * time.Hour)

	rows := &verificationMetricMockRows{
		data: []verificationMetricRowData{
			{model: "medium_range", variable: "temperature_c", metricType: "rmse", avgValue: 2.3, maxTime: now},
			{model: "medium_range", variable: "temperature_c", metricType: "bias", avgValue: -0.15, maxTime: now},
			{model: "medium_range", variable: "precipitation_probability", metricType: "brier", avgValue: 0.08, maxTime: now},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	metrics, err := repo.GetAggregatedMetrics(ctx, types.ForecastMediumRange, start, end)
	require.NoError(t, err)
	assert.Len(t, metrics, 3)

	// Verify first metric
	assert.Equal(t, types.ForecastMediumRange, metrics[0].Model)
	assert.Equal(t, "temperature_c", metrics[0].Variable)
	assert.Equal(t, "rmse", metrics[0].MetricType)
	assert.InDelta(t, 2.3, metrics[0].Value, 0.001)
	assert.Equal(t, now, metrics[0].Timestamp)

	// Verify second metric
	assert.Equal(t, "bias", metrics[1].MetricType)
	assert.InDelta(t, -0.15, metrics[1].Value, 0.001)

	// Verify third metric
	assert.Equal(t, "brier", metrics[2].MetricType)
	assert.InDelta(t, 0.08, metrics[2].Value, 0.001)

	db.AssertExpectations(t)
}

func TestVerificationRepository_GetAggregatedMetrics_EmptyResult(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 3, 0, 0, 0, time.UTC)
	rows := &verificationMetricMockRows{data: nil, idx: -1}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	metrics, err := repo.GetAggregatedMetrics(ctx, types.ForecastNowcast, now.Add(-48*time.Hour), now)
	require.NoError(t, err)
	assert.Nil(t, metrics, "should return nil slice when no results")
	db.AssertExpectations(t)
}

func TestVerificationRepository_GetAggregatedMetrics_QueryError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 3, 0, 0, 0, time.UTC)

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(nil, errors.New("connection lost"))

	metrics, err := repo.GetAggregatedMetrics(ctx, types.ForecastMediumRange, now.Add(-48*time.Hour), now)
	require.Error(t, err)
	assert.Nil(t, metrics)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

func TestVerificationRepository_GetAggregatedMetrics_ScanError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 3, 0, 0, 0, time.UTC)
	rows := &verificationMetricMockRows{
		data:    []verificationMetricRowData{{}},
		idx:     -1,
		scanErr: errors.New("type conversion error"),
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	metrics, err := repo.GetAggregatedMetrics(ctx, types.ForecastMediumRange, now.Add(-48*time.Hour), now)
	require.Error(t, err)
	assert.Nil(t, metrics)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

func TestVerificationRepository_GetAggregatedMetrics_RowsError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewVerificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 3, 0, 0, 0, time.UTC)
	rows := &verificationMetricMockRows{
		data:   nil,
		idx:    -1,
		errVal: errors.New("stream interrupted"),
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	metrics, err := repo.GetAggregatedMetrics(ctx, types.ForecastMediumRange, now.Add(-48*time.Hour), now)
	require.Error(t, err)
	assert.Nil(t, metrics)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	assert.Contains(t, appErr.Message, "iterating verification")
	db.AssertExpectations(t)
}
