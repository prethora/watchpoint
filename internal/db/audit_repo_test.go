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

// Note: mockDBTX and mockRow are defined in session_repo_test.go.
// mockRows is defined in usage_repo_test.go but uses type-specific scanning.
// We define auditMockRows here for audit-specific scanning.

// auditMockRows implements pgx.Rows for testing audit log Query results.
// It supports the column types needed by scanAuditEvent: string, []byte, time.Time.
type auditMockRows struct {
	data    []auditRowData
	idx     int
	closed  bool
	scanErr error
	errVal  error
}

type auditRowData struct {
	id             string
	orgID          string
	actorID        string
	actorType      string
	action         string
	resourceType   string
	resourceID     string
	oldValue       []byte
	newValue       []byte
	createdAt      time.Time
}

func (r *auditMockRows) Next() bool {
	if r.closed {
		return false
	}
	r.idx++
	return r.idx < len(r.data)
}

func (r *auditMockRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.data[r.idx]
	*dest[0].(*string) = row.id
	*dest[1].(*string) = row.orgID
	*dest[2].(*string) = row.actorID
	*dest[3].(*string) = row.actorType
	*dest[4].(*string) = row.action
	*dest[5].(*string) = row.resourceType
	*dest[6].(*string) = row.resourceID
	*dest[7].(*[]byte) = row.oldValue
	*dest[8].(*[]byte) = row.newValue
	*dest[9].(*time.Time) = row.createdAt
	return nil
}

func (r *auditMockRows) Close()                                       { r.closed = true }
func (r *auditMockRows) Err() error                                   { return r.errVal }
func (r *auditMockRows) CommandTag() pgconn.CommandTag                 { return pgconn.CommandTag{} }
func (r *auditMockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *auditMockRows) RawValues() [][]byte                          { return nil }
func (r *auditMockRows) Values() ([]any, error)                       { return nil, nil }
func (r *auditMockRows) Conn() *pgx.Conn                              { return nil }

// ============================================================
// Log Tests
// ============================================================

func TestAuditRepository_Log_Success_WithID(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	entry := &types.AuditEvent{
		ID: "audit_abc123",
		Actor: types.Actor{
			ID:             "user_1",
			Type:           types.ActorTypeUser,
			OrganizationID: "org_1",
		},
		Action:       types.AuditActionWatchPointCreated,
		ResourceType: "watchpoint",
		ResourceID:   "wp_1",
		NewValue:     json.RawMessage(`{"name":"My WP"}`),
		Timestamp:    now,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Log(ctx, entry)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestAuditRepository_Log_Success_WithoutID(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	entry := &types.AuditEvent{
		// No ID - let DB generate it
		Actor: types.Actor{
			ID:             "user_2",
			Type:           types.ActorTypeUser,
			OrganizationID: "org_1",
		},
		Action:       types.AuditActionWatchPointUpdated,
		ResourceType: "watchpoint",
		ResourceID:   "wp_2",
		OldValue:     json.RawMessage(`{"name":"Old Name"}`),
		NewValue:     json.RawMessage(`{"name":"New Name"}`),
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Log(ctx, entry)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestAuditRepository_Log_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	entry := &types.AuditEvent{
		Actor: types.Actor{
			ID:             "user_1",
			Type:           types.ActorTypeUser,
			OrganizationID: "org_1",
		},
		Action:       "watchpoint.created",
		ResourceType: "watchpoint",
		ResourceID:   "wp_1",
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.Log(ctx, entry)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

func TestAuditRepository_Log_SystemActor(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	entry := &types.AuditEvent{
		ID: "audit_system1",
		Actor: types.Actor{
			ID:             "system",
			Type:           types.ActorTypeSystem,
			OrganizationID: "org_1",
		},
		Action:       "organization.deleted",
		ResourceType: "organization",
		ResourceID:   "org_1",
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Log(ctx, entry)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

// ============================================================
// List Tests
// ============================================================

func TestAuditRepository_List_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-1 * time.Hour)

	rows := &auditMockRows{
		data: []auditRowData{
			{
				id:           "audit_1",
				orgID:        "org_1",
				actorID:      "user_1",
				actorType:    "user",
				action:       "watchpoint.created",
				resourceType: "watchpoint",
				resourceID:   "wp_1",
				oldValue:     nil,
				newValue:     []byte(`{"name":"WP1"}`),
				createdAt:    now,
			},
			{
				id:           "audit_2",
				orgID:        "org_1",
				actorID:      "user_2",
				actorType:    "user",
				action:       "watchpoint.updated",
				resourceType: "watchpoint",
				resourceID:   "wp_2",
				oldValue:     []byte(`{"name":"Old"}`),
				newValue:     []byte(`{"name":"New"}`),
				createdAt:    earlier,
			},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	params := types.AuditQueryFilters{
		OrganizationID: "org_1",
		ResourceType:   "watchpoint",
		Limit:          20,
	}

	results, pageInfo, err := repo.List(ctx, params)
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.False(t, pageInfo.HasMore)
	assert.Empty(t, pageInfo.NextCursor)

	// Verify first result
	assert.Equal(t, "audit_1", results[0].ID)
	assert.Equal(t, "user_1", results[0].Actor.ID)
	assert.Equal(t, types.ActorTypeUser, results[0].Actor.Type)
	assert.Equal(t, "org_1", results[0].Actor.OrganizationID)
	assert.Equal(t, "watchpoint.created", results[0].Action)
	assert.Equal(t, "watchpoint", results[0].ResourceType)
	assert.Equal(t, "wp_1", results[0].ResourceID)
	assert.Nil(t, results[0].OldValue)
	assert.NotNil(t, results[0].NewValue)

	// Verify second result
	assert.Equal(t, "audit_2", results[1].ID)
	assert.NotNil(t, results[1].OldValue)

	db.AssertExpectations(t)
}

func TestAuditRepository_List_WithPagination(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	// Create 3 rows (limit is 2, so we should get 2 results + HasMore=true)
	data := make([]auditRowData, 3)
	for i := 0; i < 3; i++ {
		data[i] = auditRowData{
			id:           "audit_" + string(rune('a'+i)),
			orgID:        "org_1",
			actorID:      "user_1",
			actorType:    "user",
			action:       "watchpoint.created",
			resourceType: "watchpoint",
			resourceID:   "wp_" + string(rune('1'+i)),
			createdAt:    now.Add(-time.Duration(i) * time.Hour),
		}
	}

	rows := &auditMockRows{data: data, idx: -1}
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	params := types.AuditQueryFilters{
		OrganizationID: "org_1",
		Limit:          2,
	}

	results, pageInfo, err := repo.List(ctx, params)
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.True(t, pageInfo.HasMore)
	assert.NotEmpty(t, pageInfo.NextCursor)

	db.AssertExpectations(t)
}

func TestAuditRepository_List_WithCursor(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	rows := &auditMockRows{data: []auditRowData{}, idx: -1}
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	cursorTime := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	params := types.AuditQueryFilters{
		OrganizationID: "org_1",
		Cursor:         cursorTime.Format(time.RFC3339Nano),
		Limit:          20,
	}

	results, pageInfo, err := repo.List(ctx, params)
	require.NoError(t, err)
	assert.Empty(t, results)
	assert.False(t, pageInfo.HasMore)

	db.AssertExpectations(t)
}

func TestAuditRepository_List_InvalidCursor(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	params := types.AuditQueryFilters{
		OrganizationID: "org_1",
		Cursor:         "not-a-valid-timestamp",
		Limit:          20,
	}

	_, _, err := repo.List(ctx, params)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeValidationMissingField, appErr.Code)
}

func TestAuditRepository_List_WithAllFilters(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	rows := &auditMockRows{data: []auditRowData{}, idx: -1}
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	params := types.AuditQueryFilters{
		OrganizationID: "org_1",
		ActorID:        "user_1",
		ResourceType:   "watchpoint",
		StartTime:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EndTime:        time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Limit:          10,
	}

	results, _, err := repo.List(ctx, params)
	require.NoError(t, err)
	assert.Empty(t, results)

	db.AssertExpectations(t)
}

func TestAuditRepository_List_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return((*auditMockRows)(nil), errors.New("connection refused"))

	params := types.AuditQueryFilters{
		OrganizationID: "org_1",
		Limit:          20,
	}

	_, _, err := repo.List(ctx, params)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

func TestAuditRepository_List_DefaultLimit(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	rows := &auditMockRows{data: []auditRowData{}, idx: -1}
	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	params := types.AuditQueryFilters{
		OrganizationID: "org_1",
		// Limit is 0, should default to 20
	}

	_, _, err := repo.List(ctx, params)
	require.NoError(t, err)

	db.AssertExpectations(t)
}

// ============================================================
// ListOlderThan Tests
// ============================================================

func TestAuditRepository_ListOlderThan_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldEntry := time.Date(2025, 12, 15, 10, 0, 0, 0, time.UTC)

	rows := &auditMockRows{
		data: []auditRowData{
			{
				id:           "audit_old1",
				orgID:        "org_1",
				actorID:      "user_1",
				actorType:    "user",
				action:       "watchpoint.created",
				resourceType: "watchpoint",
				resourceID:   "wp_1",
				createdAt:    oldEntry,
			},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(rows, nil)

	results, err := repo.ListOlderThan(ctx, cutoff, 100)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "audit_old1", results[0].ID)
	assert.True(t, results[0].Timestamp.Before(cutoff))

	db.AssertExpectations(t)
}

func TestAuditRepository_ListOlderThan_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return((*auditMockRows)(nil), errors.New("db error"))

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := repo.ListOlderThan(ctx, cutoff, 100)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// DeleteIDs Tests
// ============================================================

func TestAuditRepository_DeleteIDs_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	ids := []string{"audit_1", "audit_2", "audit_3"}

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{ids}).
		Return(pgconn.NewCommandTag("DELETE 3"), nil)

	err := repo.DeleteIDs(ctx, ids)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestAuditRepository_DeleteIDs_EmptySlice(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	// Should return immediately without making a DB call
	err := repo.DeleteIDs(ctx, []string{})
	require.NoError(t, err)

	// Verify no DB calls were made
	db.AssertNotCalled(t, "Exec")
}

func TestAuditRepository_DeleteIDs_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewAuditRepository(db)
	ctx := context.Background()

	ids := []string{"audit_1"}

	db.On("Exec", ctx, mock.AnythingOfType("string"), []any{ids}).
		Return(pgconn.CommandTag{}, errors.New("db error"))

	err := repo.DeleteIDs(ctx, ids)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}
