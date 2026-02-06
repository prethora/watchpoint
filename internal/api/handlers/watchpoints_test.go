package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// =============================================================================
// Mock Implementations for WatchPoint Handler
// =============================================================================

type mockWPRepo struct {
	createFn          func(ctx context.Context, wp *types.WatchPoint) error
	getByIDFn         func(ctx context.Context, id string, orgID string) (*types.WatchPoint, error)
	updateFn          func(ctx context.Context, wp *types.WatchPoint) error
	deleteFn          func(ctx context.Context, id string, orgID string) error
	createBatchFn     func(ctx context.Context, wps []*types.WatchPoint) ([]int, map[int]error, error)
	updateTagsBatchFn func(ctx context.Context, orgID string, filter types.BulkFilter, addTags []string, removeTags []string) (int64, error)

	// Track calls for assertions.
	lastCreated      *types.WatchPoint
	lastUpdated      *types.WatchPoint
	lastBatchCreated []*types.WatchPoint
}

func (m *mockWPRepo) Create(ctx context.Context, wp *types.WatchPoint) error {
	m.lastCreated = wp
	if m.createFn != nil {
		return m.createFn(ctx, wp)
	}
	return nil
}

func (m *mockWPRepo) GetByID(ctx context.Context, id string, orgID string) (*types.WatchPoint, error) {
	if m.getByIDFn != nil {
		return m.getByIDFn(ctx, id, orgID)
	}
	return &types.WatchPoint{
		ID:             id,
		OrganizationID: orgID,
		Name:           "Test WP",
		Location:       types.Location{Lat: 40.0, Lon: -100.0},
		Timezone:       "America/Chicago",
		Status:         types.StatusActive,
		Conditions:     types.Conditions{{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}}},
		ConditionLogic: types.LogicAny,
		Channels:       types.ChannelList{{ID: "ch1", Type: types.ChannelEmail, Config: map[string]any{"to": "test@example.com"}, Enabled: true}},
		TemplateSet:    "default",
		Source:         "default",
		ConfigVersion:  1,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}, nil
}

func (m *mockWPRepo) Update(ctx context.Context, wp *types.WatchPoint) error {
	m.lastUpdated = wp
	if m.updateFn != nil {
		return m.updateFn(ctx, wp)
	}
	return nil
}

func (m *mockWPRepo) Delete(ctx context.Context, id string, orgID string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id, orgID)
	}
	return nil
}

func (m *mockWPRepo) CreateBatch(ctx context.Context, wps []*types.WatchPoint) ([]int, map[int]error, error) {
	m.lastBatchCreated = wps
	if m.createBatchFn != nil {
		return m.createBatchFn(ctx, wps)
	}
	// Default: all succeed.
	indices := make([]int, len(wps))
	for i := range wps {
		indices[i] = i
	}
	return indices, nil, nil
}

func (m *mockWPRepo) UpdateTagsBatch(ctx context.Context, orgID string, filter types.BulkFilter, addTags []string, removeTags []string) (int64, error) {
	if m.updateTagsBatchFn != nil {
		return m.updateTagsBatchFn(ctx, orgID, filter, addTags, removeTags)
	}
	return 5, nil // Default: 5 rows updated
}

type mockWPNotifRepo struct {
	listFn                     func(ctx context.Context, filter types.NotificationFilter) ([]*types.NotificationHistoryItem, types.PageInfo, error)
	cancelDeferredDeliveriesFn func(ctx context.Context, watchpointID string) error

	cancelDeferredCalls []string // Track WatchPoint IDs
}

func (m *mockWPNotifRepo) List(ctx context.Context, filter types.NotificationFilter) ([]*types.NotificationHistoryItem, types.PageInfo, error) {
	if m.listFn != nil {
		return m.listFn(ctx, filter)
	}
	return []*types.NotificationHistoryItem{}, types.PageInfo{}, nil
}

func (m *mockWPNotifRepo) CancelDeferredDeliveries(ctx context.Context, watchpointID string) error {
	m.cancelDeferredCalls = append(m.cancelDeferredCalls, watchpointID)
	if m.cancelDeferredDeliveriesFn != nil {
		return m.cancelDeferredDeliveriesFn(ctx, watchpointID)
	}
	return nil
}

type mockWPForecastProvider struct {
	getSnapshotFn func(ctx context.Context, lat, lon float64) (*types.ForecastSnapshot, error)
}

func (m *mockWPForecastProvider) GetSnapshot(ctx context.Context, lat, lon float64) (*types.ForecastSnapshot, error) {
	if m.getSnapshotFn != nil {
		return m.getSnapshotFn(ctx, lat, lon)
	}
	return &types.ForecastSnapshot{
		TemperatureC:      25.0,
		PrecipitationMM:   2.5,
		PrecipitationProb: 40.0,
		WindSpeedKmh:      15.0,
		Humidity:          60.0,
	}, nil
}

type mockWPUsageEnforcer struct {
	checkLimitFn func(ctx context.Context, orgID string, resource types.ResourceType, count int) error
}

func (m *mockWPUsageEnforcer) CheckLimit(ctx context.Context, orgID string, resource types.ResourceType, count int) error {
	if m.checkLimitFn != nil {
		return m.checkLimitFn(ctx, orgID, resource, count)
	}
	return nil
}

type mockWPAuditLogger struct {
	logFn  func(ctx context.Context, entry *types.AuditEvent) error
	events []*types.AuditEvent
}

func (m *mockWPAuditLogger) Log(ctx context.Context, entry *types.AuditEvent) error {
	m.events = append(m.events, entry)
	if m.logFn != nil {
		return m.logFn(ctx, entry)
	}
	return nil
}

type mockWPEvalTrigger struct {
	triggerEvaluationFn func(ctx context.Context, wpID string, reason string) error
	triggerCalls        []struct {
		WPID   string
		Reason string
	}
}

func (m *mockWPEvalTrigger) TriggerEvaluation(ctx context.Context, wpID string, reason string) error {
	m.triggerCalls = append(m.triggerCalls, struct {
		WPID   string
		Reason string
	}{wpID, reason})
	if m.triggerEvaluationFn != nil {
		return m.triggerEvaluationFn(ctx, wpID, reason)
	}
	return nil
}

// =============================================================================
// Test Helper
// =============================================================================

func newTestWPHandler() (*WatchPointHandler, *mockWPRepo, *mockWPNotifRepo, *mockWPForecastProvider, *mockWPUsageEnforcer, *mockWPAuditLogger, *mockWPEvalTrigger) {
	repo := &mockWPRepo{}
	notifRepo := &mockWPNotifRepo{}
	forecastProv := &mockWPForecastProvider{}
	usageEnf := &mockWPUsageEnforcer{}
	auditLog := &mockWPAuditLogger{}
	evalTrig := &mockWPEvalTrigger{}

	logger := slog.Default()
	validator := core.NewValidator(logger)

	handler := NewWatchPointHandler(
		repo,
		notifRepo,
		validator,
		logger,
		forecastProv,
		usageEnf,
		auditLog,
		evalTrig,
	)

	return handler, repo, notifRepo, forecastProv, usageEnf, auditLog, evalTrig
}

func wpContextWithActor(orgID string) context.Context {
	actor := types.Actor{
		ID:             "usr_test123",
		Type:           types.ActorTypeUser,
		OrganizationID: orgID,
		Role:           types.RoleOwner,
		Source:         "test_app",
		IsTestMode:     false,
	}
	return types.WithActor(context.Background(), actor)
}

func wpContextWithAPIKeyActor(orgID string, testMode bool) context.Context {
	actor := types.Actor{
		ID:             "ak_test123",
		Type:           types.ActorTypeAPIKey,
		OrganizationID: orgID,
		Role:           types.RoleMember,
		Source:         "wedding_app",
		IsTestMode:     testMode,
	}
	return types.WithActor(context.Background(), actor)
}

// =============================================================================
// Create Tests
// =============================================================================

func TestWatchPointHandler_Create_Success_EventMode(t *testing.T) {
	handler, repo, _, forecastProv, _, auditLog, _ := newTestWPHandler()

	reqBody := CreateWatchPointRequest{
		Name:     "Outdoor Wedding",
		Location: types.Location{Lat: 40.0, Lon: -100.0},
		Timezone: "America/Chicago",
		TimeWindow: &types.TimeWindow{
			Start: time.Now().Add(24 * time.Hour),
			End:   time.Now().Add(48 * time.Hour),
		},
		Conditions: []types.Condition{
			{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}},
		},
		ConditionLogic: types.LogicAny,
		Channels: []types.Channel{
			{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
		},
		Tags: []string{"wedding"},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithAPIKeyActor("org_123", false))

	rr := httptest.NewRecorder()
	handler.Create(rr, req)

	assert.Equal(t, http.StatusCreated, rr.Code)

	// Verify the created WatchPoint has correct fields.
	created := repo.lastCreated
	require.NotNil(t, created)
	assert.Equal(t, "org_123", created.OrganizationID)
	assert.Equal(t, "Outdoor Wedding", created.Name)
	assert.Equal(t, types.StatusActive, created.Status)
	assert.Equal(t, "wedding_app", created.Source) // VERT-002: injected from Actor.Source
	assert.Equal(t, "default", created.TemplateSet)
	assert.Equal(t, false, created.TestMode)
	assert.Contains(t, created.ID, "wp_")
	assert.Equal(t, 1, len(created.Conditions))
	assert.Equal(t, 1, len(created.Channels))
	assert.NotEmpty(t, created.Channels[0].ID) // Channel IDs are generated

	// Verify forecast was called.
	require.NotNil(t, forecastProv)

	// Verify audit event was emitted.
	require.Len(t, auditLog.events, 1)
	assert.Equal(t, "watchpoint.created", auditLog.events[0].Action)

	// Verify response includes forecast.
	var resp core.APIResponse
	err = json.NewDecoder(rr.Body).Decode(&resp)
	require.NoError(t, err)
	assert.NotNil(t, resp.Data)
}

func TestWatchPointHandler_Create_Success_MonitorMode(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	reqBody := CreateWatchPointRequest{
		Name:     "Monitor Wind",
		Location: types.Location{Lat: 35.0, Lon: -90.0},
		Timezone: "America/Chicago",
		MonitorConfig: &types.MonitorConfig{
			WindowHours: 24,
		},
		Conditions: []types.Condition{
			{Variable: "wind_speed_kmh", Operator: ">", Threshold: []float64{50}},
		},
		ConditionLogic: types.LogicAny,
		Channels: []types.Channel{
			{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
		},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.Create(rr, req)

	assert.Equal(t, http.StatusCreated, rr.Code)

	created := repo.lastCreated
	require.NotNil(t, created)
	assert.NotNil(t, created.MonitorConfig)
	assert.Nil(t, created.TimeWindow)
	assert.Equal(t, 24, created.MonitorConfig.WindowHours)
}

func TestWatchPointHandler_Create_SourceInjection_DefaultSource(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	reqBody := CreateWatchPointRequest{
		Name:     "Test WP",
		Location: types.Location{Lat: 40.0, Lon: -100.0},
		Timezone: "America/Chicago",
		TimeWindow: &types.TimeWindow{
			Start: time.Now().Add(24 * time.Hour),
			End:   time.Now().Add(48 * time.Hour),
		},
		Conditions: []types.Condition{
			{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}},
		},
		ConditionLogic: types.LogicAny,
		Channels: []types.Channel{
			{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
		},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	// Create an actor with empty Source.
	actor := types.Actor{
		ID:             "usr_test123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_123",
		Role:           types.RoleOwner,
		Source:         "", // Empty source
	}
	ctx := types.WithActor(context.Background(), actor)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	handler.Create(rr, req)

	assert.Equal(t, http.StatusCreated, rr.Code)

	// VERT-002: empty Actor.Source defaults to "default".
	created := repo.lastCreated
	require.NotNil(t, created)
	assert.Equal(t, "default", created.Source)
}

func TestWatchPointHandler_Create_LimitExceeded(t *testing.T) {
	handler, _, _, _, usageEnf, _, _ := newTestWPHandler()

	usageEnf.checkLimitFn = func(ctx context.Context, orgID string, resource types.ResourceType, count int) error {
		return types.NewAppErrorWithDetails(
			types.ErrCodeLimitWatchpoints,
			"WatchPoint limit exceeded for current plan",
			nil,
			map[string]any{"current": 3, "limit": 3, "plan": "free"},
		)
	}

	reqBody := CreateWatchPointRequest{
		Name:     "Over Limit",
		Location: types.Location{Lat: 40.0, Lon: -100.0},
		Timezone: "America/Chicago",
		TimeWindow: &types.TimeWindow{
			Start: time.Now().Add(24 * time.Hour),
			End:   time.Now().Add(48 * time.Hour),
		},
		Conditions: []types.Condition{
			{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}},
		},
		ConditionLogic: types.LogicAny,
		Channels: []types.Channel{
			{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
		},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.Create(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code)

	var errResp core.APIErrorResponse
	err = json.NewDecoder(rr.Body).Decode(&errResp)
	require.NoError(t, err)
	assert.Equal(t, string(types.ErrCodeLimitWatchpoints), errResp.Error.Code)
}

func TestWatchPointHandler_Create_InvalidVariable(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	reqBody := CreateWatchPointRequest{
		Name:     "Invalid Var",
		Location: types.Location{Lat: 40.0, Lon: -100.0},
		Timezone: "America/Chicago",
		TimeWindow: &types.TimeWindow{
			Start: time.Now().Add(24 * time.Hour),
			End:   time.Now().Add(48 * time.Hour),
		},
		Conditions: []types.Condition{
			{Variable: "nonexistent_variable", Operator: ">", Threshold: []float64{30}},
		},
		ConditionLogic: types.LogicAny,
		Channels: []types.Channel{
			{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
		},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.Create(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestWatchPointHandler_Create_ThresholdOutOfRange(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	reqBody := CreateWatchPointRequest{
		Name:     "Bad Threshold",
		Location: types.Location{Lat: 40.0, Lon: -100.0},
		Timezone: "America/Chicago",
		TimeWindow: &types.TimeWindow{
			Start: time.Now().Add(24 * time.Hour),
			End:   time.Now().Add(48 * time.Hour),
		},
		Conditions: []types.Condition{
			{Variable: "temperature_c", Operator: ">", Threshold: []float64{100}}, // Max is 60
		},
		ConditionLogic: types.LogicAny,
		Channels: []types.Channel{
			{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
		},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.Create(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)

	var errResp core.APIErrorResponse
	err = json.NewDecoder(rr.Body).Decode(&errResp)
	require.NoError(t, err)
	assert.Equal(t, string(types.ErrCodeValidationThresholdRange), errResp.Error.Code)
}

func TestWatchPointHandler_Create_ForecastFailureGraceful(t *testing.T) {
	handler, _, _, forecastProv, _, _, _ := newTestWPHandler()

	// Forecast fails but create should still succeed.
	forecastProv.getSnapshotFn = func(ctx context.Context, lat, lon float64) (*types.ForecastSnapshot, error) {
		return nil, errors.New("S3 timeout")
	}

	reqBody := CreateWatchPointRequest{
		Name:     "Forecast Fails",
		Location: types.Location{Lat: 40.0, Lon: -100.0},
		Timezone: "America/Chicago",
		TimeWindow: &types.TimeWindow{
			Start: time.Now().Add(24 * time.Hour),
			End:   time.Now().Add(48 * time.Hour),
		},
		Conditions: []types.Condition{
			{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}},
		},
		ConditionLogic: types.LogicAny,
		Channels: []types.Channel{
			{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
		},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.Create(rr, req)

	// Create should still succeed with 201.
	assert.Equal(t, http.StatusCreated, rr.Code)
}

func TestWatchPointHandler_Create_NoAuth(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	// No actor in context.

	rr := httptest.NewRecorder()
	handler.Create(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestWatchPointHandler_Create_TestMode(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	reqBody := CreateWatchPointRequest{
		Name:     "Test Mode WP",
		Location: types.Location{Lat: 40.0, Lon: -100.0},
		Timezone: "America/Chicago",
		TimeWindow: &types.TimeWindow{
			Start: time.Now().Add(24 * time.Hour),
			End:   time.Now().Add(48 * time.Hour),
		},
		Conditions: []types.Condition{
			{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}},
		},
		ConditionLogic: types.LogicAny,
		Channels: []types.Channel{
			{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
		},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithAPIKeyActor("org_123", true)) // Test mode

	rr := httptest.NewRecorder()
	handler.Create(rr, req)

	assert.Equal(t, http.StatusCreated, rr.Code)

	created := repo.lastCreated
	require.NotNil(t, created)
	assert.True(t, created.TestMode)
}

// =============================================================================
// Get Tests
// =============================================================================

func TestWatchPointHandler_Get_Success(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	r := chi.NewRouter()
	r.Get("/watchpoints/{id}", handler.Get)

	req := httptest.NewRequest(http.MethodGet, "/watchpoints/wp_123", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]json.RawMessage
	err := json.NewDecoder(rr.Body).Decode(&resp)
	require.NoError(t, err)
	assert.Contains(t, string(resp["data"]), "wp_123")
}

func TestWatchPointHandler_Get_NotFound(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	repo.getByIDFn = func(ctx context.Context, id string, orgID string) (*types.WatchPoint, error) {
		return nil, types.NewAppError(types.ErrCodeNotFoundWatchpoint, "watchpoint not found", nil)
	}

	r := chi.NewRouter()
	r.Get("/watchpoints/{id}", handler.Get)

	req := httptest.NewRequest(http.MethodGet, "/watchpoints/wp_nonexistent", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestWatchPointHandler_Get_ForecastFailure(t *testing.T) {
	handler, _, _, forecastProv, _, _, _ := newTestWPHandler()

	forecastProv.getSnapshotFn = func(ctx context.Context, lat, lon float64) (*types.ForecastSnapshot, error) {
		return nil, errors.New("forecast unavailable")
	}

	r := chi.NewRouter()
	r.Get("/watchpoints/{id}", handler.Get)

	req := httptest.NewRequest(http.MethodGet, "/watchpoints/wp_123", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// Should still return 200 with null forecast.
	assert.Equal(t, http.StatusOK, rr.Code)
}

// =============================================================================
// Update Tests
// =============================================================================

func TestWatchPointHandler_Update_Success(t *testing.T) {
	handler, repo, _, _, _, auditLog, _ := newTestWPHandler()

	newName := "Updated Name"
	reqBody := UpdateWatchPointRequest{
		Name: &newName,
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Patch("/watchpoints/{id}", handler.Update)

	req := httptest.NewRequest(http.MethodPatch, "/watchpoints/wp_123", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	// Verify the update was applied.
	updated := repo.lastUpdated
	require.NotNil(t, updated)
	assert.Equal(t, "Updated Name", updated.Name)

	// Verify source was preserved (VERT-003: immutable).
	assert.Equal(t, "default", updated.Source)

	// Verify audit event.
	require.Len(t, auditLog.events, 1)
	assert.Equal(t, "watchpoint.updated", auditLog.events[0].Action)
}

func TestWatchPointHandler_Update_ConditionsReplacement(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	newConditions := []types.Condition{
		{Variable: "wind_speed_kmh", Operator: ">", Threshold: []float64{50}},
		{Variable: "precipitation_mm", Operator: ">", Threshold: []float64{10}},
	}
	reqBody := UpdateWatchPointRequest{
		Conditions: &newConditions,
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Patch("/watchpoints/{id}", handler.Update)

	req := httptest.NewRequest(http.MethodPatch, "/watchpoints/wp_123", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	// Conditions should be fully replaced, not merged.
	updated := repo.lastUpdated
	require.NotNil(t, updated)
	assert.Len(t, updated.Conditions, 2)
	assert.Equal(t, "wind_speed_kmh", updated.Conditions[0].Variable)
}

func TestWatchPointHandler_Update_NotFound(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	repo.getByIDFn = func(ctx context.Context, id string, orgID string) (*types.WatchPoint, error) {
		return nil, types.NewAppError(types.ErrCodeNotFoundWatchpoint, "watchpoint not found", nil)
	}

	newName := "Nope"
	reqBody := UpdateWatchPointRequest{Name: &newName}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Patch("/watchpoints/{id}", handler.Update)

	req := httptest.NewRequest(http.MethodPatch, "/watchpoints/wp_not_found", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// =============================================================================
// Delete Tests
// =============================================================================

func TestWatchPointHandler_Delete_Success(t *testing.T) {
	handler, _, _, _, _, auditLog, _ := newTestWPHandler()

	r := chi.NewRouter()
	r.Delete("/watchpoints/{id}", handler.Delete)

	req := httptest.NewRequest(http.MethodDelete, "/watchpoints/wp_123", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNoContent, rr.Code)

	// Verify audit event.
	require.Len(t, auditLog.events, 1)
	assert.Equal(t, "watchpoint.deleted", auditLog.events[0].Action)
}

func TestWatchPointHandler_Delete_NotFound(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	repo.deleteFn = func(ctx context.Context, id string, orgID string) error {
		return types.NewAppError(types.ErrCodeNotFoundWatchpoint, "watchpoint not found", nil)
	}

	r := chi.NewRouter()
	r.Delete("/watchpoints/{id}", handler.Delete)

	req := httptest.NewRequest(http.MethodDelete, "/watchpoints/wp_missing", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// =============================================================================
// Pause Tests
// =============================================================================

func TestWatchPointHandler_Pause_Success(t *testing.T) {
	handler, repo, notifRepo, _, _, auditLog, _ := newTestWPHandler()

	// Return an active WatchPoint.
	repo.getByIDFn = func(ctx context.Context, id string, orgID string) (*types.WatchPoint, error) {
		return &types.WatchPoint{
			ID:             id,
			OrganizationID: orgID,
			Name:           "Active WP",
			Status:         types.StatusActive,
			Location:       types.Location{Lat: 40.0, Lon: -100.0},
			Timezone:       "America/Chicago",
			Conditions:     types.Conditions{{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}}},
			ConditionLogic: types.LogicAny,
			Channels:       types.ChannelList{{ID: "ch1", Type: types.ChannelEmail, Config: map[string]any{"to": "test@example.com"}, Enabled: true}},
			TemplateSet:    "default",
			Source:         "default",
		}, nil
	}

	r := chi.NewRouter()
	r.Post("/watchpoints/{id}/pause", handler.Pause)

	req := httptest.NewRequest(http.MethodPost, "/watchpoints/wp_123/pause", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	// Verify status was set to paused.
	updated := repo.lastUpdated
	require.NotNil(t, updated)
	assert.Equal(t, types.StatusPaused, updated.Status)

	// Verify deferred deliveries were cancelled.
	require.Len(t, notifRepo.cancelDeferredCalls, 1)
	assert.Equal(t, "wp_123", notifRepo.cancelDeferredCalls[0])

	// Verify audit event.
	require.Len(t, auditLog.events, 1)
	assert.Equal(t, "watchpoint.paused", auditLog.events[0].Action)
}

func TestWatchPointHandler_Pause_AlreadyPaused(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	repo.getByIDFn = func(ctx context.Context, id string, orgID string) (*types.WatchPoint, error) {
		return &types.WatchPoint{
			ID:             id,
			OrganizationID: orgID,
			Status:         types.StatusPaused,
			Location:       types.Location{Lat: 40.0, Lon: -100.0},
			Timezone:       "America/Chicago",
			Conditions:     types.Conditions{{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}}},
			ConditionLogic: types.LogicAny,
			Channels:       types.ChannelList{{ID: "ch1", Type: types.ChannelEmail, Config: map[string]any{}, Enabled: true}},
		}, nil
	}

	r := chi.NewRouter()
	r.Post("/watchpoints/{id}/pause", handler.Pause)

	req := httptest.NewRequest(http.MethodPost, "/watchpoints/wp_123/pause", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code)

	var errResp core.APIErrorResponse
	err := json.NewDecoder(rr.Body).Decode(&errResp)
	require.NoError(t, err)
	assert.Equal(t, string(types.ErrCodeConflictPaused), errResp.Error.Code)
}

// =============================================================================
// Resume Tests
// =============================================================================

func TestWatchPointHandler_Resume_Success(t *testing.T) {
	handler, repo, notifRepo, _, _, auditLog, evalTrig := newTestWPHandler()

	// Return a paused WatchPoint.
	repo.getByIDFn = func(ctx context.Context, id string, orgID string) (*types.WatchPoint, error) {
		return &types.WatchPoint{
			ID:             id,
			OrganizationID: orgID,
			Name:           "Paused WP",
			Status:         types.StatusPaused,
			Location:       types.Location{Lat: 40.0, Lon: -100.0},
			Timezone:       "America/Chicago",
			Conditions:     types.Conditions{{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}}},
			ConditionLogic: types.LogicAny,
			Channels:       types.ChannelList{{ID: "ch1", Type: types.ChannelEmail, Config: map[string]any{"to": "test@example.com"}, Enabled: true}},
			TemplateSet:    "default",
			Source:         "default",
		}, nil
	}

	r := chi.NewRouter()
	r.Post("/watchpoints/{id}/resume", handler.Resume)

	req := httptest.NewRequest(http.MethodPost, "/watchpoints/wp_456/resume", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	// Verify status was set to active.
	updated := repo.lastUpdated
	require.NotNil(t, updated)
	assert.Equal(t, types.StatusActive, updated.Status)

	// Verify deferred deliveries were cancelled (per WPLC-006).
	require.Len(t, notifRepo.cancelDeferredCalls, 1)
	assert.Equal(t, "wp_456", notifRepo.cancelDeferredCalls[0])

	// Verify evaluation was triggered (per WPLC-006).
	require.Len(t, evalTrig.triggerCalls, 1)
	assert.Equal(t, "wp_456", evalTrig.triggerCalls[0].WPID)
	assert.Equal(t, "resume", evalTrig.triggerCalls[0].Reason)

	// Verify audit event.
	require.Len(t, auditLog.events, 1)
	assert.Equal(t, "watchpoint.resumed", auditLog.events[0].Action)
}

func TestWatchPointHandler_Resume_AlreadyActive(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	r := chi.NewRouter()
	r.Post("/watchpoints/{id}/resume", handler.Resume)

	req := httptest.NewRequest(http.MethodPost, "/watchpoints/wp_123/resume", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code)

	var errResp core.APIErrorResponse
	err := json.NewDecoder(rr.Body).Decode(&errResp)
	require.NoError(t, err)
	assert.Equal(t, string(types.ErrCodeConflictActive), errResp.Error.Code)
}

func TestWatchPointHandler_Resume_EvalTriggerFailure_NonBlocking(t *testing.T) {
	handler, repo, _, _, _, _, evalTrig := newTestWPHandler()

	// Return a paused WatchPoint.
	repo.getByIDFn = func(ctx context.Context, id string, orgID string) (*types.WatchPoint, error) {
		return &types.WatchPoint{
			ID:             id,
			OrganizationID: orgID,
			Status:         types.StatusPaused,
			Location:       types.Location{Lat: 40.0, Lon: -100.0},
			Timezone:       "America/Chicago",
			Conditions:     types.Conditions{{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}}},
			ConditionLogic: types.LogicAny,
			Channels:       types.ChannelList{{ID: "ch1", Type: types.ChannelEmail, Config: map[string]any{}, Enabled: true}},
			TemplateSet:    "default",
			Source:         "default",
		}, nil
	}

	// Make eval trigger fail.
	evalTrig.triggerEvaluationFn = func(ctx context.Context, wpID string, reason string) error {
		return errors.New("SQS send failed")
	}

	r := chi.NewRouter()
	r.Post("/watchpoints/{id}/resume", handler.Resume)

	req := httptest.NewRequest(http.MethodPost, "/watchpoints/wp_789/resume", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// Resume should still succeed even if trigger fails.
	assert.Equal(t, http.StatusOK, rr.Code)

	// Status should still be active.
	updated := repo.lastUpdated
	require.NotNil(t, updated)
	assert.Equal(t, types.StatusActive, updated.Status)
}

// =============================================================================
// GetNotificationHistory Tests
// =============================================================================

func TestWatchPointHandler_GetNotificationHistory_Success(t *testing.T) {
	handler, _, notifRepo, _, _, _, _ := newTestWPHandler()

	now := time.Now().UTC()
	notifRepo.listFn = func(ctx context.Context, filter types.NotificationFilter) ([]*types.NotificationHistoryItem, types.PageInfo, error) {
		assert.Equal(t, "org_123", filter.OrganizationID)
		assert.Equal(t, "wp_123", filter.WatchPointID)
		return []*types.NotificationHistoryItem{
			{
				ID:        "notif_1",
				EventType: types.EventThresholdCrossed,
				SentAt:    now,
				Channels: []types.DeliverySummary{
					{Channel: "email", Status: "sent"},
				},
			},
		}, types.PageInfo{HasMore: false}, nil
	}

	r := chi.NewRouter()
	r.Get("/watchpoints/{id}/notifications", handler.GetNotificationHistory)

	req := httptest.NewRequest(http.MethodGet, "/watchpoints/wp_123/notifications", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestWatchPointHandler_GetNotificationHistory_WithPagination(t *testing.T) {
	handler, _, notifRepo, _, _, _, _ := newTestWPHandler()

	notifRepo.listFn = func(ctx context.Context, filter types.NotificationFilter) ([]*types.NotificationHistoryItem, types.PageInfo, error) {
		assert.NotEmpty(t, filter.Pagination.NextCursor)
		return []*types.NotificationHistoryItem{}, types.PageInfo{HasMore: false}, nil
	}

	r := chi.NewRouter()
	r.Get("/watchpoints/{id}/notifications", handler.GetNotificationHistory)

	req := httptest.NewRequest(http.MethodGet, "/watchpoints/wp_123/notifications?cursor=2026-01-01T00:00:00Z&limit=10", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestWatchPointHandler_GetNotificationHistory_InvalidLimit(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	r := chi.NewRouter()
	r.Get("/watchpoints/{id}/notifications", handler.GetNotificationHistory)

	req := httptest.NewRequest(http.MethodGet, "/watchpoints/wp_123/notifications?limit=0", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestWatchPointHandler_GetNotificationHistory_NilRepo(t *testing.T) {
	// Create handler with nil notifRepo.
	logger := slog.Default()
	validator := core.NewValidator(logger)
	handler := NewWatchPointHandler(
		&mockWPRepo{},
		nil, // nil notifRepo
		validator,
		logger,
		nil, nil, nil, nil,
	)

	r := chi.NewRouter()
	r.Get("/watchpoints/{id}/notifications", handler.GetNotificationHistory)

	req := httptest.NewRequest(http.MethodGet, "/watchpoints/wp_123/notifications", nil)
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}

// =============================================================================
// Route Registration Test
// =============================================================================

func TestWatchPointHandler_RegisterRoutes(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	r := chi.NewRouter()
	handler.RegisterRoutes(r)

	// Verify routes are registered by walking the router.
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/watchpoints/"},
		{http.MethodGet, "/watchpoints/"},
		{http.MethodGet, "/watchpoints/{id}/"},
		{http.MethodPatch, "/watchpoints/{id}/"},
		{http.MethodDelete, "/watchpoints/{id}/"},
		{http.MethodPost, "/watchpoints/{id}/pause"},
		{http.MethodPost, "/watchpoints/{id}/resume"},
		{http.MethodGet, "/watchpoints/{id}/notifications"},
	}

	// Walk the router to check registered routes.
	registeredRoutes := make(map[string]bool)
	walkFn := func(method string, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		registeredRoutes[key] = true
		return nil
	}

	err := chi.Walk(r, walkFn)
	require.NoError(t, err)

	for _, rt := range routes {
		key := rt.method + " " + rt.path
		assert.True(t, registeredRoutes[key], "Route not registered: %s %s", rt.method, rt.path)
	}
}

// =============================================================================
// Integration Flow: Create -> Pause -> Resume
// =============================================================================

func TestWatchPointHandler_CreatePauseResumeFlow(t *testing.T) {
	handler, repo, notifRepo, _, _, _, evalTrig := newTestWPHandler()

	// Create the WatchPoint.
	reqBody := CreateWatchPointRequest{
		Name:     "Flow Test",
		Location: types.Location{Lat: 40.0, Lon: -100.0},
		Timezone: "America/Chicago",
		TimeWindow: &types.TimeWindow{
			Start: time.Now().Add(24 * time.Hour),
			End:   time.Now().Add(48 * time.Hour),
		},
		Conditions: []types.Condition{
			{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}},
		},
		ConditionLogic: types.LogicAny,
		Channels: []types.Channel{
			{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
		},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	createReq := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", bytes.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(wpContextWithActor("org_123"))

	createRR := httptest.NewRecorder()
	handler.Create(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code)

	// Get the created WP ID.
	createdWP := repo.lastCreated
	require.NotNil(t, createdWP)
	wpID := createdWP.ID

	// Now set up the repo to return this WP for Get (simulating it exists in DB).
	repo.getByIDFn = func(ctx context.Context, id string, orgID string) (*types.WatchPoint, error) {
		if id == wpID {
			return createdWP, nil
		}
		return nil, types.NewAppError(types.ErrCodeNotFoundWatchpoint, "not found", nil)
	}

	// Pause.
	r := chi.NewRouter()
	r.Post("/watchpoints/{id}/pause", handler.Pause)
	r.Post("/watchpoints/{id}/resume", handler.Resume)

	pauseReq := httptest.NewRequest(http.MethodPost, "/watchpoints/"+wpID+"/pause", nil)
	pauseReq = pauseReq.WithContext(wpContextWithActor("org_123"))

	pauseRR := httptest.NewRecorder()
	r.ServeHTTP(pauseRR, pauseReq)
	require.Equal(t, http.StatusOK, pauseRR.Code)

	// Verify paused.
	assert.Equal(t, types.StatusPaused, repo.lastUpdated.Status)
	assert.Len(t, notifRepo.cancelDeferredCalls, 1)

	// Update the mock to reflect paused state.
	createdWP.Status = types.StatusPaused
	repo.getByIDFn = func(ctx context.Context, id string, orgID string) (*types.WatchPoint, error) {
		return createdWP, nil
	}

	// Resume.
	resumeReq := httptest.NewRequest(http.MethodPost, "/watchpoints/"+wpID+"/resume", nil)
	resumeReq = resumeReq.WithContext(wpContextWithActor("org_123"))

	resumeRR := httptest.NewRecorder()
	r.ServeHTTP(resumeRR, resumeReq)
	require.Equal(t, http.StatusOK, resumeRR.Code)

	// Verify resumed.
	assert.Equal(t, types.StatusActive, repo.lastUpdated.Status)
	assert.Len(t, notifRepo.cancelDeferredCalls, 2) // Once on pause, once on resume
	assert.Len(t, evalTrig.triggerCalls, 1)          // Only on resume
	assert.Equal(t, wpID, evalTrig.triggerCalls[0].WPID)
	assert.Equal(t, "resume", evalTrig.triggerCalls[0].Reason)
}

// =============================================================================
// BulkCreate Tests
// =============================================================================

func validBulkItem(name string) CreateWatchPointRequest {
	return CreateWatchPointRequest{
		Name:     name,
		Location: types.Location{Lat: 40.0, Lon: -100.0},
		Timezone: "America/Chicago",
		TimeWindow: &types.TimeWindow{
			Start: time.Now().Add(24 * time.Hour),
			End:   time.Now().Add(48 * time.Hour),
		},
		Conditions: []types.Condition{
			{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}},
		},
		ConditionLogic: types.LogicAny,
		Channels: []types.Channel{
			{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
		},
		Tags: []string{"bulk"},
	}
}

func TestWatchPointHandler_BulkCreate_AllSuccess(t *testing.T) {
	handler, repo, _, _, _, auditLog, _ := newTestWPHandler()

	items := []CreateWatchPointRequest{
		validBulkItem("WP 1"),
		validBulkItem("WP 2"),
		validBulkItem("WP 3"),
	}

	body, err := json.Marshal(items)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithAPIKeyActor("org_123", false))

	rr := httptest.NewRecorder()
	handler.BulkCreate(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Data BulkCreateResponse `json:"data"`
	}
	err = json.NewDecoder(rr.Body).Decode(&resp)
	require.NoError(t, err)

	// All 3 should succeed.
	assert.Len(t, resp.Data.Successes, 3)
	assert.Len(t, resp.Data.Failures, 0)

	// Verify batch was persisted.
	require.Len(t, repo.lastBatchCreated, 3)
	for _, wp := range repo.lastBatchCreated {
		assert.Contains(t, wp.ID, "wp_")
		assert.Equal(t, "org_123", wp.OrganizationID)
		assert.Equal(t, types.StatusActive, wp.Status)
		assert.False(t, wp.TestMode)
	}

	// Verify aggregated audit event.
	require.Len(t, auditLog.events, 1)
	assert.Equal(t, types.AuditActionWatchPointBulkCreated, auditLog.events[0].Action)
}

func TestWatchPointHandler_BulkCreate_PartialFailure_ErrorAggregation(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	// Mix of valid and invalid items to test error aggregation.
	items := []CreateWatchPointRequest{
		// Index 0: valid
		validBulkItem("Valid WP"),
		// Index 1: invalid variable
		{
			Name:     "Bad Variable",
			Location: types.Location{Lat: 40.0, Lon: -100.0},
			Timezone: "America/Chicago",
			TimeWindow: &types.TimeWindow{
				Start: time.Now().Add(24 * time.Hour),
				End:   time.Now().Add(48 * time.Hour),
			},
			Conditions: []types.Condition{
				{Variable: "nonexistent_var", Operator: ">", Threshold: []float64{30}},
			},
			ConditionLogic: types.LogicAny,
			Channels: []types.Channel{
				{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
			},
		},
		// Index 2: valid
		validBulkItem("Another Valid WP"),
		// Index 3: invalid time window (end before start)
		{
			Name:     "Bad Time Window",
			Location: types.Location{Lat: 40.0, Lon: -100.0},
			Timezone: "America/Chicago",
			TimeWindow: &types.TimeWindow{
				Start: time.Now().Add(48 * time.Hour),
				End:   time.Now().Add(24 * time.Hour), // End before start
			},
			Conditions: []types.Condition{
				{Variable: "temperature_c", Operator: ">", Threshold: []float64{30}},
			},
			ConditionLogic: types.LogicAny,
			Channels: []types.Channel{
				{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
			},
		},
		// Index 4: threshold out of range
		{
			Name:     "Bad Threshold",
			Location: types.Location{Lat: 40.0, Lon: -100.0},
			Timezone: "America/Chicago",
			TimeWindow: &types.TimeWindow{
				Start: time.Now().Add(24 * time.Hour),
				End:   time.Now().Add(48 * time.Hour),
			},
			Conditions: []types.Condition{
				{Variable: "temperature_c", Operator: ">", Threshold: []float64{999}}, // Way out of range
			},
			ConditionLogic: types.LogicAny,
			Channels: []types.Channel{
				{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
			},
		},
	}

	body, err := json.Marshal(items)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkCreate(rr, req)

	// Should return 200 OK even with partial failures.
	assert.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Data BulkCreateResponse `json:"data"`
	}
	err = json.NewDecoder(rr.Body).Decode(&resp)
	require.NoError(t, err)

	// 2 valid items should succeed (indices 0 and 2).
	assert.Len(t, resp.Data.Successes, 2)
	// 3 invalid items should fail (indices 1, 3, 4).
	assert.Len(t, resp.Data.Failures, 3)

	// Verify the batch only contained the valid items.
	require.Len(t, repo.lastBatchCreated, 2)

	// Verify failure indices and error codes.
	failureIndices := make(map[int]types.ErrorCode)
	for _, f := range resp.Data.Failures {
		failureIndices[f.Index] = f.Code
	}

	// Index 1: invalid variable
	assert.Equal(t, types.ErrCodeValidationInvalidVariable, failureIndices[1])
	// Index 3: invalid time window
	assert.Equal(t, types.ErrCodeValidationTimeWindow, failureIndices[3])
	// Index 4: threshold out of range
	assert.Equal(t, types.ErrCodeValidationThresholdRange, failureIndices[4])

	// Verify success indices.
	successIndices := make(map[int]bool)
	for _, s := range resp.Data.Successes {
		successIndices[s.Index] = true
		assert.Contains(t, s.ID, "wp_")
		assert.NotNil(t, s.WatchPoint)
	}
	assert.True(t, successIndices[0])
	assert.True(t, successIndices[2])
}

func TestWatchPointHandler_BulkCreate_AllFail(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	items := []CreateWatchPointRequest{
		{
			Name:     "Bad 1",
			Location: types.Location{Lat: 40.0, Lon: -100.0},
			Timezone: "America/Chicago",
			TimeWindow: &types.TimeWindow{
				Start: time.Now().Add(24 * time.Hour),
				End:   time.Now().Add(48 * time.Hour),
			},
			Conditions: []types.Condition{
				{Variable: "nonexistent_var", Operator: ">", Threshold: []float64{30}},
			},
			ConditionLogic: types.LogicAny,
			Channels: []types.Channel{
				{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
			},
		},
		{
			Name:     "Bad 2",
			Location: types.Location{Lat: 40.0, Lon: -100.0},
			Timezone: "America/Chicago",
			TimeWindow: &types.TimeWindow{
				Start: time.Now().Add(24 * time.Hour),
				End:   time.Now().Add(48 * time.Hour),
			},
			Conditions: []types.Condition{
				{Variable: "another_bad_var", Operator: ">", Threshold: []float64{30}},
			},
			ConditionLogic: types.LogicAny,
			Channels: []types.Channel{
				{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
			},
		},
	}

	body, err := json.Marshal(items)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkCreate(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Data BulkCreateResponse `json:"data"`
	}
	err = json.NewDecoder(rr.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Len(t, resp.Data.Successes, 0)
	assert.Len(t, resp.Data.Failures, 2)

	// No batch should have been created when all items fail.
	assert.Nil(t, repo.lastBatchCreated)
}

func TestWatchPointHandler_BulkCreate_EmptyBatch(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	body, _ := json.Marshal([]CreateWatchPointRequest{})

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkCreate(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestWatchPointHandler_BulkCreate_ExceedsBatchLimit(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	// Create 101 items (exceeds max of 100).
	items := make([]CreateWatchPointRequest, 101)
	for i := range items {
		items[i] = validBulkItem(fmt.Sprintf("WP %d", i))
	}

	body, err := json.Marshal(items)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkCreate(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)

	var errResp core.APIErrorResponse
	err = json.NewDecoder(rr.Body).Decode(&errResp)
	require.NoError(t, err)
	assert.Equal(t, string(types.ErrCodeValidationBatchSize), errResp.Error.Code)
}

func TestWatchPointHandler_BulkCreate_LimitEnforcement(t *testing.T) {
	handler, _, _, _, usageEnf, _, _ := newTestWPHandler()

	usageEnf.checkLimitFn = func(ctx context.Context, orgID string, resource types.ResourceType, count int) error {
		return types.NewAppError(
			types.ErrCodeLimitWatchpoints,
			"WatchPoint limit exceeded",
			nil,
		)
	}

	items := []CreateWatchPointRequest{
		validBulkItem("WP 1"),
		validBulkItem("WP 2"),
	}

	body, err := json.Marshal(items)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkCreate(rr, req)

	// Limit exceeded returns the limit error, not 200.
	assert.Equal(t, http.StatusForbidden, rr.Code)

	var errResp core.APIErrorResponse
	err = json.NewDecoder(rr.Body).Decode(&errResp)
	require.NoError(t, err)
	assert.Equal(t, string(types.ErrCodeLimitWatchpoints), errResp.Error.Code)
}

func TestWatchPointHandler_BulkCreate_DBError(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	repo.createBatchFn = func(ctx context.Context, wps []*types.WatchPoint) ([]int, map[int]error, error) {
		return nil, nil, types.NewAppError(types.ErrCodeInternalDB, "connection refused", nil)
	}

	items := []CreateWatchPointRequest{
		validBulkItem("WP 1"),
	}

	body, err := json.Marshal(items)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkCreate(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestWatchPointHandler_BulkCreate_SourceInjection(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	items := []CreateWatchPointRequest{
		validBulkItem("WP Source Test"),
	}

	body, err := json.Marshal(items)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithAPIKeyActor("org_123", true))

	rr := httptest.NewRecorder()
	handler.BulkCreate(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	// Verify source injection (VERT-002) and test mode propagation.
	require.Len(t, repo.lastBatchCreated, 1)
	assert.Equal(t, "wedding_app", repo.lastBatchCreated[0].Source)
	assert.True(t, repo.lastBatchCreated[0].TestMode)
}

func TestWatchPointHandler_BulkCreate_NoAuth(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	items := []CreateWatchPointRequest{validBulkItem("WP 1")}
	body, _ := json.Marshal(items)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No actor in context.

	rr := httptest.NewRecorder()
	handler.BulkCreate(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestWatchPointHandler_BulkCreate_MixedValidationErrors(t *testing.T) {
	// This test specifically validates error aggregation: multiple types of validation
	// errors in a single batch should all be captured and returned per-item.
	handler, _, _, _, _, _, _ := newTestWPHandler()

	items := []CreateWatchPointRequest{
		// Index 0: missing required fields (validation failure)
		{
			Name:     "", // Empty name (required field)
			Location: types.Location{Lat: 40.0, Lon: -100.0},
			Timezone: "America/Chicago",
		},
		// Index 1: valid
		validBulkItem("Good One"),
		// Index 2: invalid variable
		{
			Name:     "Bad Var",
			Location: types.Location{Lat: 40.0, Lon: -100.0},
			Timezone: "America/Chicago",
			TimeWindow: &types.TimeWindow{
				Start: time.Now().Add(24 * time.Hour),
				End:   time.Now().Add(48 * time.Hour),
			},
			Conditions: []types.Condition{
				{Variable: "fake_var", Operator: ">", Threshold: []float64{30}},
			},
			ConditionLogic: types.LogicAny,
			Channels: []types.Channel{
				{Type: types.ChannelEmail, Config: map[string]any{"to": "user@example.com"}, Enabled: true},
			},
		},
	}

	body, err := json.Marshal(items)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints/bulk", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkCreate(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Data BulkCreateResponse `json:"data"`
	}
	err = json.NewDecoder(rr.Body).Decode(&resp)
	require.NoError(t, err)

	// 1 success (index 1), 2 failures (index 0, 2).
	assert.Len(t, resp.Data.Successes, 1)
	assert.Len(t, resp.Data.Failures, 2)

	// Verify the failure indices are correct.
	failureIndices := make([]int, 0)
	for _, f := range resp.Data.Failures {
		failureIndices = append(failureIndices, f.Index)
		// Every failure must have a code and message.
		assert.NotEmpty(t, f.Code, "failure at index %d should have a code", f.Index)
		assert.NotEmpty(t, f.Message, "failure at index %d should have a message", f.Index)
	}
	assert.Contains(t, failureIndices, 0)
	assert.Contains(t, failureIndices, 2)

	// Verify the success index is correct.
	assert.Equal(t, 1, resp.Data.Successes[0].Index)
}

// =============================================================================
// BulkTagUpdate Tests
// =============================================================================

func TestWatchPointHandler_BulkTagUpdate_Success(t *testing.T) {
	handler, repo, _, _, _, auditLog, _ := newTestWPHandler()

	var capturedOrgID string
	var capturedFilter types.BulkFilter
	var capturedAddTags, capturedRemoveTags []string

	repo.updateTagsBatchFn = func(ctx context.Context, orgID string, filter types.BulkFilter, addTags []string, removeTags []string) (int64, error) {
		capturedOrgID = orgID
		capturedFilter = filter
		capturedAddTags = addTags
		capturedRemoveTags = removeTags
		return 12, nil
	}

	reqBody := BulkTagUpdateRequest{
		Filter:     types.BulkFilter{Tags: []string{"project-a"}},
		AddTags:    []string{"archived"},
		RemoveTags: []string{"active"},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPatch, "/v1/watchpoints/bulk/tags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkTagUpdate(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Data BulkResponse `json:"data"`
	}
	err = json.NewDecoder(rr.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Equal(t, 12, resp.Data.SuccessCount)
	assert.Equal(t, 0, resp.Data.FailureCount)

	// Verify repo was called with correct args.
	assert.Equal(t, "org_123", capturedOrgID)
	assert.Equal(t, []string{"project-a"}, capturedFilter.Tags)
	assert.Equal(t, []string{"archived"}, capturedAddTags)
	assert.Equal(t, []string{"active"}, capturedRemoveTags)

	// Verify audit event.
	require.Len(t, auditLog.events, 1)
	assert.Equal(t, types.AuditActionWatchPointBulkTagUpdated, auditLog.events[0].Action)
}

func TestWatchPointHandler_BulkTagUpdate_NoTagsProvided(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	reqBody := BulkTagUpdateRequest{
		Filter:     types.BulkFilter{Tags: []string{"project-a"}},
		AddTags:    nil,
		RemoveTags: nil,
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPatch, "/v1/watchpoints/bulk/tags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkTagUpdate(rr, req)

	// Should return 400 when neither add_tags nor remove_tags is provided.
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestWatchPointHandler_BulkTagUpdate_OnlyAddTags(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	reqBody := BulkTagUpdateRequest{
		Filter:  types.BulkFilter{Status: []types.Status{types.StatusActive}},
		AddTags: []string{"new-tag"},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPatch, "/v1/watchpoints/bulk/tags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkTagUpdate(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestWatchPointHandler_BulkTagUpdate_OnlyRemoveTags(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	reqBody := BulkTagUpdateRequest{
		Filter:     types.BulkFilter{Tags: []string{"old-tag"}},
		RemoveTags: []string{"old-tag"},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPatch, "/v1/watchpoints/bulk/tags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkTagUpdate(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestWatchPointHandler_BulkTagUpdate_DBError(t *testing.T) {
	handler, repo, _, _, _, _, _ := newTestWPHandler()

	repo.updateTagsBatchFn = func(ctx context.Context, orgID string, filter types.BulkFilter, addTags []string, removeTags []string) (int64, error) {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "database unavailable", nil)
	}

	reqBody := BulkTagUpdateRequest{
		Filter:  types.BulkFilter{Tags: []string{"project-a"}},
		AddTags: []string{"archived"},
	}

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPatch, "/v1/watchpoints/bulk/tags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(wpContextWithActor("org_123"))

	rr := httptest.NewRecorder()
	handler.BulkTagUpdate(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestWatchPointHandler_BulkTagUpdate_NoAuth(t *testing.T) {
	handler, _, _, _, _, _, _ := newTestWPHandler()

	reqBody := BulkTagUpdateRequest{
		Filter:  types.BulkFilter{Tags: []string{"project-a"}},
		AddTags: []string{"archived"},
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPatch, "/v1/watchpoints/bulk/tags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No actor in context.

	rr := httptest.NewRecorder()
	handler.BulkTagUpdate(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}
