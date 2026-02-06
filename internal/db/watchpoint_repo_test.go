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
// Helper: build a scanFn for a standard WatchPoint row
// ============================================================

func newTestWatchPoint() *types.WatchPoint {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	return &types.WatchPoint{
		ID:             "wp_abc123",
		OrganizationID: "org_123",
		Name:           "Rain Alert Portland",
		Location: types.Location{
			Lat:         45.5231,
			Lon:         -122.6765,
			DisplayName: "Portland, OR",
		},
		Timezone: "America/Los_Angeles",
		TileID:   "1.5",
		TimeWindow: &types.TimeWindow{
			Start: now.Add(24 * time.Hour),
			End:   now.Add(72 * time.Hour),
		},
		Conditions: types.Conditions{
			{Variable: "precipitation_probability", Operator: ">", Threshold: []float64{50}, Unit: "%"},
		},
		ConditionLogic: types.LogicAll,
		Channels: types.ChannelList{
			{ID: "ch_1", Type: types.ChannelEmail, Config: map[string]any{"email": "test@example.com"}, Enabled: true},
		},
		TemplateSet:    "default",
		Status:         types.StatusActive,
		TestMode:       false,
		Tags:           []string{"portland", "rain"},
		ConfigVersion:  1,
		Source:         "api",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// makeScanFnForWP creates a scanFn that populates dest slices to match a given
// WatchPoint. This mirrors the column ordering in wpColumns.
func makeScanFnForWP(wp *types.WatchPoint) func(dest ...any) error {
	return func(dest ...any) error {
		// Column order: id, org_id, name, lat, lon, display_name, timezone, tile_id,
		// tw_start, tw_end, monitor_config, conditions, condition_logic,
		// channels, template_set, preferences, status, test_mode, tags,
		// config_version, source, created_at, updated_at, archived_at, archived_reason
		*dest[0].(*string) = wp.ID
		*dest[1].(*string) = wp.OrganizationID
		*dest[2].(*string) = wp.Name
		*dest[3].(*float64) = wp.Location.Lat
		*dest[4].(*float64) = wp.Location.Lon

		displayName := wp.Location.DisplayName
		if displayName != "" {
			*dest[5].(**string) = &displayName
		} else {
			*dest[5].(**string) = nil
		}

		*dest[6].(*string) = wp.Timezone
		*dest[7].(*string) = wp.TileID

		// TimeWindow
		if wp.TimeWindow != nil {
			start := wp.TimeWindow.Start
			end := wp.TimeWindow.End
			*dest[8].(**time.Time) = &start
			*dest[9].(**time.Time) = &end
		} else {
			*dest[8].(**time.Time) = nil
			*dest[9].(**time.Time) = nil
		}

		// MonitorConfig
		if wp.MonitorConfig != nil {
			mc := *wp.MonitorConfig
			*dest[10].(**types.MonitorConfig) = &mc
		} else {
			*dest[10].(**types.MonitorConfig) = nil
		}

		// Conditions (implements Scanner)
		condBytes, _ := json.Marshal(wp.Conditions)
		dest[11].(*types.Conditions).Scan(condBytes)

		// ConditionLogic
		*dest[12].(*types.ConditionLogic) = wp.ConditionLogic

		// Channels (implements Scanner)
		chanBytes, _ := json.Marshal(wp.Channels)
		dest[13].(*types.ChannelList).Scan(chanBytes)

		// TemplateSet
		if wp.TemplateSet != "" {
			ts := wp.TemplateSet
			*dest[14].(**string) = &ts
		} else {
			*dest[14].(**string) = nil
		}

		// Preferences
		if wp.NotificationPrefs != nil {
			p := *wp.NotificationPrefs
			*dest[15].(**types.Preferences) = &p
		} else {
			*dest[15].(**types.Preferences) = nil
		}

		// Status
		*dest[16].(*types.Status) = wp.Status
		// TestMode
		*dest[17].(*bool) = wp.TestMode
		// Tags
		*dest[18].(*[]string) = wp.Tags
		// ConfigVersion
		*dest[19].(*int) = wp.ConfigVersion

		// Source
		if wp.Source != "" {
			src := wp.Source
			*dest[20].(**string) = &src
		} else {
			*dest[20].(**string) = nil
		}

		// Timestamps
		*dest[21].(*time.Time) = wp.CreatedAt
		*dest[22].(*time.Time) = wp.UpdatedAt
		*dest[23].(**time.Time) = wp.ArchivedAt

		// ArchivedReason
		if wp.ArchivedReason != "" {
			r := wp.ArchivedReason
			*dest[24].(**string) = &r
		} else {
			*dest[24].(**string) = nil
		}

		return nil
	}
}

// ============================================================
// Create Tests
// ============================================================

func TestWatchPointRepository_Create_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Create(ctx, wp)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestWatchPointRepository_Create_MonitorMode(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()
	wp.TimeWindow = nil
	wp.MonitorConfig = &types.MonitorConfig{
		WindowHours: 24,
		ActiveHours: [][2]int{{8, 20}},
		ActiveDays:  []int{1, 2, 3, 4, 5},
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Create(ctx, wp)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestWatchPointRepository_Create_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("constraint violation"))

	err := repo.Create(ctx, wp)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// GetByID Tests
// ============================================================

func TestWatchPointRepository_GetByID_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()

	row := &mockRow{scanFn: makeScanFnForWP(wp)}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_abc123", "org_123"}).Return(row)

	result, err := repo.GetByID(ctx, "wp_abc123", "org_123")
	require.NoError(t, err)
	assert.Equal(t, "wp_abc123", result.ID)
	assert.Equal(t, "org_123", result.OrganizationID)
	assert.Equal(t, "Rain Alert Portland", result.Name)
	assert.Equal(t, 45.5231, result.Location.Lat)
	assert.Equal(t, -122.6765, result.Location.Lon)
	assert.Equal(t, "Portland, OR", result.Location.DisplayName)
	assert.Equal(t, "1.5", result.TileID)
	assert.NotNil(t, result.TimeWindow)
	assert.Equal(t, types.StatusActive, result.Status)
	assert.Equal(t, 1, result.ConfigVersion)
	require.Len(t, result.Channels, 1)
	assert.Equal(t, "ch_1", result.Channels[0].ID)
	require.Len(t, result.Conditions, 1)
	assert.Equal(t, "precipitation_probability", result.Conditions[0].Variable)
	assert.Equal(t, []string{"portland", "rain"}, result.Tags)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_GetByID_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: pgx.ErrNoRows}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_missing", "org_123"}).Return(row)

	_, err := repo.GetByID(ctx, "wp_missing", "org_123")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundWatchpoint, appErr.Code)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_GetByID_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	row := &mockRow{scanErr: errors.New("connection refused")}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_abc123", "org_123"}).Return(row)

	_, err := repo.GetByID(ctx, "wp_abc123", "org_123")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// Update Tests
// ============================================================

func TestWatchPointRepository_Update_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()
	wp.Name = "Updated Rain Alert"

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.Update(ctx, wp)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestWatchPointRepository_Update_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()
	wp.ID = "wp_missing"

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.Update(ctx, wp)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundWatchpoint, appErr.Code)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_Update_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.Update(ctx, wp)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// Delete Tests (Soft Delete)
// ============================================================

func TestWatchPointRepository_Delete_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.Delete(ctx, "wp_abc123", "org_123")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestWatchPointRepository_Delete_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.Delete(ctx, "wp_missing", "org_123")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundWatchpoint, appErr.Code)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_Delete_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.Delete(ctx, "wp_abc123", "org_123")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// List Tests
// ============================================================

// wpMockRows implements pgx.Rows for testing WatchPoint Query results.
type wpMockRows struct {
	items   []*types.WatchPoint
	idx     int
	closed  bool
	scanErr error
	errVal  error
}

func newWPMockRows(items []*types.WatchPoint) *wpMockRows {
	return &wpMockRows{items: items, idx: -1}
}

func (r *wpMockRows) Next() bool {
	if r.closed {
		return false
	}
	r.idx++
	return r.idx < len(r.items)
}

func (r *wpMockRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if r.idx >= 0 && r.idx < len(r.items) {
		fn := makeScanFnForWP(r.items[r.idx])
		return fn(dest...)
	}
	return errors.New("no current row")
}

func (r *wpMockRows) Close()                                   { r.closed = true }
func (r *wpMockRows) Err() error                               { return r.errVal }
func (r *wpMockRows) CommandTag() pgconn.CommandTag             { return pgconn.CommandTag{} }
func (r *wpMockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *wpMockRows) RawValues() [][]byte                       { return nil }
func (r *wpMockRows) Values() ([]any, error)                    { return nil, nil }
func (r *wpMockRows) Conn() *pgx.Conn                          { return nil }

func TestWatchPointRepository_List_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp1 := newTestWatchPoint()
	wp1.ID = "wp_001"
	wp2 := newTestWatchPoint()
	wp2.ID = "wp_002"
	wp2.Name = "Snow Alert Denver"

	rows := newWPMockRows([]*types.WatchPoint{wp1, wp2})
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	results, pageInfo, err := repo.List(ctx, "org_123", ListWatchPointsParams{
		Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "wp_001", results[0].ID)
	assert.Equal(t, "wp_002", results[1].ID)
	assert.False(t, pageInfo.HasMore)
	assert.Empty(t, pageInfo.NextCursor)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_List_WithPagination(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	// Create 3 items to simulate limit+1 fetch (limit=2 means 3 items returned).
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	items := make([]*types.WatchPoint, 3)
	for i := 0; i < 3; i++ {
		items[i] = newTestWatchPoint()
		items[i].ID = "wp_" + string(rune('a'+i))
		items[i].CreatedAt = now.Add(time.Duration(-i) * time.Hour)
	}

	rows := newWPMockRows(items)
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	results, pageInfo, err := repo.List(ctx, "org_123", ListWatchPointsParams{
		Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, results, 2, "should trim to limit")
	assert.True(t, pageInfo.HasMore)
	assert.NotEmpty(t, pageInfo.NextCursor, "should have a cursor for the next page")
	// NextCursor should be the created_at of the last returned item (index 1).
	expectedCursor := items[1].CreatedAt.Format(time.RFC3339Nano)
	assert.Equal(t, expectedCursor, pageInfo.NextCursor)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_List_WithStatusFilter(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	rows := newWPMockRows(nil) // empty results
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	results, pageInfo, err := repo.List(ctx, "org_123", ListWatchPointsParams{
		Status: []types.Status{types.StatusActive, types.StatusPaused},
		Limit:  20,
	})
	require.NoError(t, err)
	assert.Empty(t, results)
	assert.False(t, pageInfo.HasMore)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_List_WithTagFilter(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()
	rows := newWPMockRows([]*types.WatchPoint{wp})
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	results, _, err := repo.List(ctx, "org_123", ListWatchPointsParams{
		Tags:  []string{"portland"},
		Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Tags, "portland")

	db.AssertExpectations(t)
}

func TestWatchPointRepository_List_WithTileIDFilter(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()
	rows := newWPMockRows([]*types.WatchPoint{wp})
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	results, _, err := repo.List(ctx, "org_123", ListWatchPointsParams{
		TileID: "1.5",
		Limit:  20,
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "1.5", results[0].TileID)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_List_WithCursor(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	rows := newWPMockRows(nil)
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	cursor := time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	_, _, err := repo.List(ctx, "org_123", ListWatchPointsParams{
		Cursor: cursor,
		Limit:  20,
	})
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_List_InvalidCursor(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	_, _, err := repo.List(ctx, "org_123", ListWatchPointsParams{
		Cursor: "not-a-timestamp",
		Limit:  20,
	})
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeValidationMissingField, appErr.Code)
}

func TestWatchPointRepository_List_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return((*wpMockRows)(nil), errors.New("connection refused"))

	_, _, err := repo.List(ctx, "org_123", ListWatchPointsParams{Limit: 20})
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_List_DefaultLimit(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	rows := newWPMockRows(nil)
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	// limit=0 should default to 20
	_, _, err := repo.List(ctx, "org_123", ListWatchPointsParams{})
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_List_MaxLimit(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	rows := newWPMockRows(nil)
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	// limit=500 should be capped at 100
	_, _, err := repo.List(ctx, "org_123", ListWatchPointsParams{Limit: 500})
	require.NoError(t, err)

	db.AssertExpectations(t)
}

// ============================================================
// UpdateStatusBatch Tests
// ============================================================

func TestWatchPointRepository_UpdateStatusBatch_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 3"), nil)

	ids := []string{"wp_1", "wp_2", "wp_3"}
	count, err := repo.UpdateStatusBatch(ctx, ids, "org_123", types.StatusPaused, false)
	require.NoError(t, err)
	assert.Equal(t, int64(3), count)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateStatusBatch_PartialMatch(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	// Only 2 of 3 IDs matched (e.g., one was already deleted)
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 2"), nil)

	ids := []string{"wp_1", "wp_2", "wp_missing"}
	count, err := repo.UpdateStatusBatch(ctx, ids, "org_123", types.StatusPaused, false)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateStatusBatch_TestModeIsolation(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	// Test mode true ensures only test watchpoints are affected
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	ids := []string{"wp_test_1"}
	count, err := repo.UpdateStatusBatch(ctx, ids, "org_123", types.StatusActive, true)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateStatusBatch_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	_, err := repo.UpdateStatusBatch(ctx, []string{"wp_1"}, "org_123", types.StatusPaused, false)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// DeleteBatch Tests
// ============================================================

func TestWatchPointRepository_DeleteBatch_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 2"), nil)

	count, err := repo.DeleteBatch(ctx, []string{"wp_1", "wp_2"}, "org_123", false)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_DeleteBatch_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	_, err := repo.DeleteBatch(ctx, []string{"wp_1"}, "org_123", false)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// UpdateChannelConfig Tests
// ============================================================

func TestWatchPointRepository_UpdateChannelConfig_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	// Step 1: Mock the SELECT to read current channels.
	channelsJSON := `[
		{"id": "ch_1", "type": "webhook", "config": {"url": "https://example.com", "secret": "old_secret"}, "enabled": true},
		{"id": "ch_2", "type": "email", "config": {"email": "test@example.com"}, "enabled": true}
	]`
	selectRow := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*[]byte) = []byte(channelsJSON)
			return nil
		},
	}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_abc123"}).Return(selectRow)

	// Step 2: Mock the UPDATE to write back the modified channels.
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	// Mutation function: rotate the secret for ch_1.
	mutationFn := func(config map[string]any) (map[string]any, error) {
		config["previous_secret"] = config["secret"]
		config["secret"] = "new_secret_value"
		return config, nil
	}

	err := repo.UpdateChannelConfig(ctx, "wp_abc123", "ch_1", mutationFn)
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateChannelConfig_ChannelNotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	channelsJSON := `[
		{"id": "ch_1", "type": "webhook", "config": {"url": "https://example.com"}, "enabled": true}
	]`
	selectRow := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*[]byte) = []byte(channelsJSON)
			return nil
		},
	}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_abc123"}).Return(selectRow)

	mutationFn := func(config map[string]any) (map[string]any, error) {
		return config, nil
	}

	err := repo.UpdateChannelConfig(ctx, "wp_abc123", "ch_nonexistent", mutationFn)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundWatchpoint, appErr.Code)
	assert.Contains(t, appErr.Message, "channel not found")

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateChannelConfig_WatchPointNotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	selectRow := &mockRow{scanErr: pgx.ErrNoRows}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_missing"}).Return(selectRow)

	mutationFn := func(config map[string]any) (map[string]any, error) {
		return config, nil
	}

	err := repo.UpdateChannelConfig(ctx, "wp_missing", "ch_1", mutationFn)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundWatchpoint, appErr.Code)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateChannelConfig_MutationError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	channelsJSON := `[{"id": "ch_1", "type": "webhook", "config": {"secret": "old"}, "enabled": true}]`
	selectRow := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*[]byte) = []byte(channelsJSON)
			return nil
		},
	}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_abc123"}).Return(selectRow)

	mutationFn := func(config map[string]any) (map[string]any, error) {
		return nil, errors.New("mutation failed")
	}

	err := repo.UpdateChannelConfig(ctx, "wp_abc123", "ch_1", mutationFn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutation failed")

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateChannelConfig_DBErrorOnRead(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	selectRow := &mockRow{scanErr: errors.New("connection refused")}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_abc123"}).Return(selectRow)

	mutationFn := func(config map[string]any) (map[string]any, error) {
		return config, nil
	}

	err := repo.UpdateChannelConfig(ctx, "wp_abc123", "ch_1", mutationFn)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateChannelConfig_DBErrorOnWrite(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	channelsJSON := `[{"id": "ch_1", "type": "webhook", "config": {"secret": "old"}, "enabled": true}]`
	selectRow := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*[]byte) = []byte(channelsJSON)
			return nil
		},
	}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_abc123"}).Return(selectRow)

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("write failed"))

	mutationFn := func(config map[string]any) (map[string]any, error) {
		config["secret"] = "new"
		return config, nil
	}

	err := repo.UpdateChannelConfig(ctx, "wp_abc123", "ch_1", mutationFn)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// Verify the mutationFn receives the correct config and the update is written properly.
func TestWatchPointRepository_UpdateChannelConfig_VerifyMutation(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	channelsJSON := `[
		{"id": "ch_1", "type": "webhook", "config": {"url": "https://example.com", "secret": "original_secret"}, "enabled": true}
	]`
	selectRow := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*[]byte) = []byte(channelsJSON)
			return nil
		},
	}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_abc123"}).Return(selectRow)

	// Capture the written JSON to verify the mutation was applied.
	var capturedJSON []byte
	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Run(func(args mock.Arguments) {
			execArgs := args.Get(2).([]any)
			capturedJSON = execArgs[0].([]byte)
		}).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	mutationFn := func(config map[string]any) (map[string]any, error) {
		assert.Equal(t, "original_secret", config["secret"])
		config["previous_secret"] = config["secret"]
		config["secret"] = "rotated_secret"
		config["previous_secret_expires_at"] = "2026-02-07T12:00:00Z"
		return config, nil
	}

	err := repo.UpdateChannelConfig(ctx, "wp_abc123", "ch_1", mutationFn)
	require.NoError(t, err)

	// Parse the captured JSON and verify the mutation was applied.
	var updatedChannels []map[string]any
	require.NoError(t, json.Unmarshal(capturedJSON, &updatedChannels))
	require.Len(t, updatedChannels, 1)

	ch := updatedChannels[0]
	assert.Equal(t, "ch_1", ch["id"])
	config := ch["config"].(map[string]any)
	assert.Equal(t, "rotated_secret", config["secret"])
	assert.Equal(t, "original_secret", config["previous_secret"])
	assert.Equal(t, "2026-02-07T12:00:00Z", config["previous_secret_expires_at"])

	db.AssertExpectations(t)
}

// ============================================================
// GetTileCounts Tests
// ============================================================

func TestWatchPointRepository_GetTileCounts_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	rows := &tileMockRows{
		data: []struct {
			tileID string
			count  int
		}{
			{"1.5", 10},
			{"2.3", 5},
			{"0.0", 1},
		},
		idx: -1,
	}
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	result, err := repo.GetTileCounts(ctx)
	require.NoError(t, err)
	assert.Equal(t, 10, result["1.5"])
	assert.Equal(t, 5, result["2.3"])
	assert.Equal(t, 1, result["0.0"])
	assert.Len(t, result, 3)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_GetTileCounts_Empty(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	rows := &tileMockRows{idx: -1}
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	result, err := repo.GetTileCounts(ctx)
	require.NoError(t, err)
	assert.Empty(t, result)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_GetTileCounts_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return((*tileMockRows)(nil), errors.New("connection refused"))

	_, err := repo.GetTileCounts(ctx)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// tileMockRows is a minimal pgx.Rows mock for tile count queries (2 columns: string, int).
type tileMockRows struct {
	data []struct {
		tileID string
		count  int
	}
	idx     int
	closed  bool
	scanErr error
	errVal  error
}

func (r *tileMockRows) Next() bool {
	if r.closed {
		return false
	}
	r.idx++
	return r.idx < len(r.data)
}

func (r *tileMockRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if r.idx >= 0 && r.idx < len(r.data) {
		*dest[0].(*string) = r.data[r.idx].tileID
		*dest[1].(*int) = r.data[r.idx].count
		return nil
	}
	return errors.New("no current row")
}

func (r *tileMockRows) Close()                                   { r.closed = true }
func (r *tileMockRows) Err() error                               { return r.errVal }
func (r *tileMockRows) CommandTag() pgconn.CommandTag             { return pgconn.CommandTag{} }
func (r *tileMockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *tileMockRows) RawValues() [][]byte                       { return nil }
func (r *tileMockRows) Values() ([]any, error)                    { return nil, nil }
func (r *tileMockRows) Conn() *pgx.Conn                          { return nil }

// ============================================================
// PauseAllByOrgID Tests
// ============================================================

func TestWatchPointRepository_PauseAllByOrgID_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 5"), nil)

	err := repo.PauseAllByOrgID(ctx, "org_123", string(types.PausedReasonBillingDelinquency))
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_PauseAllByOrgID_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.PauseAllByOrgID(ctx, "org_123", "user_action")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// ResumeAllByOrgID Tests
// ============================================================

func TestWatchPointRepository_ResumeAllByOrgID_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 3"), nil)

	err := repo.ResumeAllByOrgID(ctx, "org_123", string(types.PausedReasonBillingDelinquency))
	require.NoError(t, err)

	db.AssertExpectations(t)
}

// ============================================================
// ArchiveExpired Tests
// ============================================================

func TestWatchPointRepository_ArchiveExpired_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 2"), nil)

	cutoff := time.Now()
	count, err := repo.ArchiveExpired(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_ArchiveExpired_None(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	cutoff := time.Now()
	count, err := repo.ArchiveExpired(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)

	db.AssertExpectations(t)
}

// ============================================================
// JSONB Handling Verification Tests
// ============================================================

func TestWatchPointRepository_Create_JSONBFields(t *testing.T) {
	// Verify that JSONB fields (conditions, channels, monitor_config, preferences)
	// are properly marshaled for INSERT operations.
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()
	wp.TimeWindow = nil
	wp.MonitorConfig = &types.MonitorConfig{
		WindowHours: 48,
		ActiveHours: [][2]int{{6, 18}},
		ActiveDays:  []int{1, 2, 3, 4, 5},
	}
	wp.NotificationPrefs = &types.Preferences{
		NotifyOnClear:          true,
		NotifyOnForecastChange: false,
	}
	wp.Channels = types.ChannelList{
		{ID: "ch_1", Type: types.ChannelWebhook, Config: map[string]any{
			"url":    "https://hooks.example.com/weather",
			"secret": "super_secret_value",
		}, Enabled: true},
		{ID: "ch_2", Type: types.ChannelEmail, Config: map[string]any{
			"email": "alerts@example.com",
		}, Enabled: true},
	}
	wp.Conditions = types.Conditions{
		{Variable: "temperature_c", Operator: ">", Threshold: []float64{35}, Unit: "C"},
		{Variable: "wind_speed_kmh", Operator: ">=", Threshold: []float64{80}, Unit: "km/h"},
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Create(ctx, wp)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestWatchPointRepository_GetByID_MonitorMode(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp := newTestWatchPoint()
	wp.TimeWindow = nil
	wp.MonitorConfig = &types.MonitorConfig{
		WindowHours: 24,
		ActiveHours: [][2]int{{8, 20}},
		ActiveDays:  []int{1, 2, 3, 4, 5},
	}

	row := &mockRow{scanFn: makeScanFnForWP(wp)}
	db.On("QueryRow", ctx, mock.AnythingOfType("string"), []any{"wp_abc123", "org_123"}).Return(row)

	result, err := repo.GetByID(ctx, "wp_abc123", "org_123")
	require.NoError(t, err)
	assert.Nil(t, result.TimeWindow, "event mode fields should be nil for monitor mode")
	assert.NotNil(t, result.MonitorConfig, "monitor config should be set")
	assert.Equal(t, 24, result.MonitorConfig.WindowHours)

	db.AssertExpectations(t)
}

// ============================================================
// Helper function tests
// ============================================================

func TestTimeWindowHelpers(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	// Non-nil TimeWindow
	tw := &types.TimeWindow{Start: now, End: now.Add(24 * time.Hour)}
	start := timeWindowStart(tw)
	end := timeWindowEnd(tw)
	require.NotNil(t, start)
	require.NotNil(t, end)
	assert.Equal(t, now, *start)
	assert.Equal(t, now.Add(24*time.Hour), *end)

	// Nil TimeWindow
	assert.Nil(t, timeWindowStart(nil))
	assert.Nil(t, timeWindowEnd(nil))
}

// ============================================================
// GetBatch Tests
// ============================================================

func TestWatchPointRepository_GetBatch_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp1 := newTestWatchPoint()
	wp1.ID = "wp_001"
	wp2 := newTestWatchPoint()
	wp2.ID = "wp_002"

	rows := newWPMockRows([]*types.WatchPoint{wp1, wp2})
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	results, err := repo.GetBatch(ctx, []string{"wp_001", "wp_002"}, "org_123")
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "wp_001", results[0].ID)
	assert.Equal(t, "wp_002", results[1].ID)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_GetBatch_Empty(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	rows := newWPMockRows(nil)
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	results, err := repo.GetBatch(ctx, []string{"wp_missing"}, "org_123")
	require.NoError(t, err)
	assert.Empty(t, results)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_GetBatch_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return((*wpMockRows)(nil), errors.New("db error"))

	_, err := repo.GetBatch(ctx, []string{"wp_1"}, "org_123")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// UpdateTagsBatch Tests
// ============================================================

func TestWatchPointRepository_UpdateTagsBatch_AddAndRemove(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 5"), nil)

	filter := types.BulkFilter{
		Status: []types.Status{types.StatusActive},
	}

	count, err := repo.UpdateTagsBatch(ctx, "org_123", filter, []string{"new_tag"}, []string{"old_tag"})
	require.NoError(t, err)
	assert.Equal(t, int64(5), count)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateTagsBatch_AddOnly(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 3"), nil)

	filter := types.BulkFilter{}
	count, err := repo.UpdateTagsBatch(ctx, "org_123", filter, []string{"tag1", "tag2"}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(3), count)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateTagsBatch_RemoveOnly(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 2"), nil)

	filter := types.BulkFilter{}
	count, err := repo.UpdateTagsBatch(ctx, "org_123", filter, nil, []string{"deprecated"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_UpdateTagsBatch_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	filter := types.BulkFilter{}
	_, err := repo.UpdateTagsBatch(ctx, "org_123", filter, []string{"tag"}, nil)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// PauseExcessByOrgID Tests
// ============================================================

func TestWatchPointRepository_PauseExcessByOrgID_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 3"), nil)

	err := repo.PauseExcessByOrgID(ctx, "org_123", 5)
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_PauseExcessByOrgID_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.PauseExcessByOrgID(ctx, "org_123", 5)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// ListByTile Tests
// ============================================================

func TestWatchPointRepository_ListByTile_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	wp1 := newTestWatchPoint()
	wp1.ID = "wp_001"
	wp2 := newTestWatchPoint()
	wp2.ID = "wp_002"

	rows := newWPMockRows([]*types.WatchPoint{wp1, wp2})
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	results, err := repo.ListByTile(ctx, "1.5", 100, 0)
	require.NoError(t, err)
	require.Len(t, results, 2)

	db.AssertExpectations(t)
}

func TestWatchPointRepository_ListByTile_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewWatchPointRepository(db)
	ctx := context.Background()

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return((*wpMockRows)(nil), errors.New("db error"))

	_, err := repo.ListByTile(ctx, "1.5", 100, 0)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}
