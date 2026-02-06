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

// --- Mock Rows for Query ---

// mockRows implements pgx.Rows for testing Query results.
type mockRows struct {
	data    [][]any
	idx     int
	closed  bool
	scanErr error
	errVal  error
}

func newMockRows(data [][]any) *mockRows {
	return &mockRows{data: data, idx: -1}
}

func (r *mockRows) Next() bool {
	if r.closed {
		return false
	}
	r.idx++
	return r.idx < len(r.data)
}

func (r *mockRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.data[r.idx]
	for i, d := range dest {
		switch v := d.(type) {
		case *time.Time:
			*v = row[i].(time.Time)
		case *int:
			*v = row[i].(int)
		}
	}
	return nil
}

func (r *mockRows) Close()                        { r.closed = true }
func (r *mockRows) Err() error                    { return r.errVal }
func (r *mockRows) CommandTag() pgconn.CommandTag  { return pgconn.CommandTag{} }
func (r *mockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *mockRows) RawValues() [][]byte            { return nil }
func (r *mockRows) Values() ([]any, error)         { return nil, nil }
func (r *mockRows) Conn() *pgx.Conn               { return nil }

// --- UsageHistoryRepo Tests ---

func TestUsageHistoryRepo_Query_DailyGranularity(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUsageHistoryRepo(db)

	day1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	rows := newMockRows([][]any{
		{day1, 100, 10, 5},
		{day2, 200, 12, 8},
	})

	db.On("Query", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

	result, err := repo.Query(context.Background(), "org_1", start, end, types.GranularityDaily)
	require.NoError(t, err)
	require.Len(t, result, 2)

	assert.Equal(t, day1, result[0].Date)
	assert.Equal(t, 100, result[0].APICalls)
	assert.Equal(t, 10, result[0].ActiveWatchPoints)
	assert.Equal(t, 5, result[0].NotificationsSent)

	assert.Equal(t, day2, result[1].Date)
	assert.Equal(t, 200, result[1].APICalls)
	assert.Equal(t, 12, result[1].ActiveWatchPoints)
	assert.Equal(t, 8, result[1].NotificationsSent)

	db.AssertExpectations(t)
}

func TestUsageHistoryRepo_Query_MonthlyGranularity(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUsageHistoryRepo(db)

	month1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	rows := newMockRows([][]any{
		{month1, 3000, 15, 100},
	})

	db.On("Query", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Run(func(args mock.Arguments) {
			sql := args.Get(1).(string)
			// Verify the SQL contains 'month' for the date_trunc
			assert.Contains(t, sql, "'month'")
		}).
		Return(rows, nil)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)

	result, err := repo.Query(context.Background(), "org_1", start, end, types.GranularityMonthly)
	require.NoError(t, err)
	require.Len(t, result, 1)

	assert.Equal(t, month1, result[0].Date)
	assert.Equal(t, 3000, result[0].APICalls)
	assert.Equal(t, 15, result[0].ActiveWatchPoints)
	assert.Equal(t, 100, result[0].NotificationsSent)

	db.AssertExpectations(t)
}

func TestUsageHistoryRepo_Query_EmptyResult(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUsageHistoryRepo(db)

	rows := newMockRows([][]any{})

	db.On("Query", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

	result, err := repo.Query(context.Background(), "org_1", start, end, types.GranularityDaily)
	require.NoError(t, err)
	assert.Empty(t, result)

	db.AssertExpectations(t)
}

func TestUsageHistoryRepo_Query_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUsageHistoryRepo(db)

	db.On("Query", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return((*mockRows)(nil), errors.New("connection refused"))

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

	result, err := repo.Query(context.Background(), "org_1", start, end, types.GranularityDaily)
	require.Error(t, err)
	assert.Nil(t, result)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestUsageHistoryRepo_Query_ScanError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUsageHistoryRepo(db)

	rows := &mockRows{
		data:    [][]any{{time.Now(), 0, 0, 0}},
		idx:     -1,
		scanErr: errors.New("scan failed"),
	}

	db.On("Query", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

	result, err := repo.Query(context.Background(), "org_1", start, end, types.GranularityDaily)
	require.Error(t, err)
	assert.Nil(t, result)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestUsageHistoryRepo_Query_RowsErrPropagated(t *testing.T) {
	db := new(mockDBTX)
	repo := NewUsageHistoryRepo(db)

	rows := &mockRows{
		data:   [][]any{},
		idx:    -1,
		errVal: errors.New("rows iteration error"),
	}

	db.On("Query", mock.Anything, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

	result, err := repo.Query(context.Background(), "org_1", start, end, types.GranularityDaily)
	require.Error(t, err)
	assert.Nil(t, result)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
}

func TestGranularityToTruncUnit(t *testing.T) {
	tests := []struct {
		input    types.TimeGranularity
		expected string
	}{
		{types.GranularityDaily, "day"},
		{types.GranularityMonthly, "month"},
		{types.TimeGranularity("unknown"), "day"},
		{types.TimeGranularity(""), "day"},
	}

	for _, tc := range tests {
		t.Run(string(tc.input), func(t *testing.T) {
			result := granularityToTruncUnit(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}
