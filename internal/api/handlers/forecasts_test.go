package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"watchpoint/internal/core"
	"watchpoint/internal/forecasts"
	"watchpoint/internal/types"
)

// --- Mock Service ---

type mockForecastService struct {
	pointResult        *forecasts.ForecastResponse
	pointErr           error
	batchResult        *forecasts.BatchForecastResult
	batchErr           error
	variablesResult    []forecasts.VariableResponseMetadata
	variablesErr       error
	statusResult       *forecasts.SystemStatus
	statusErr          error
	snapshotResult     *types.ForecastSnapshot
	snapshotErr        error
	verificationResult *forecasts.VerificationReport
	verificationErr    error
}

func (m *mockForecastService) GetPointForecast(_ context.Context, _, _ float64, _, _ time.Time) (*forecasts.ForecastResponse, error) {
	return m.pointResult, m.pointErr
}

func (m *mockForecastService) GetBatchForecast(_ context.Context, _ forecasts.BatchForecastRequest) (*forecasts.BatchForecastResult, error) {
	return m.batchResult, m.batchErr
}

func (m *mockForecastService) GetVariables(_ context.Context) ([]forecasts.VariableResponseMetadata, error) {
	return m.variablesResult, m.variablesErr
}

func (m *mockForecastService) GetStatus(_ context.Context) (*forecasts.SystemStatus, error) {
	return m.statusResult, m.statusErr
}

func (m *mockForecastService) GetSnapshot(_ context.Context, _, _ float64) (*types.ForecastSnapshot, error) {
	return m.snapshotResult, m.snapshotErr
}

func (m *mockForecastService) GetVerificationMetrics(_ context.Context, _ string, _, _ time.Time) (*forecasts.VerificationReport, error) {
	return m.verificationResult, m.verificationErr
}

// --- Helpers ---

func newTestForecastHandler(svc ForecastServiceInterface) *ForecastHandler {
	logger := slog.Default()
	validator := core.NewValidator(logger)
	return NewForecastHandler(svc, validator, logger)
}

func makeForecastRouter(h *ForecastHandler) http.Handler {
	r := chi.NewRouter()
	r.Route("/v1/forecasts", h.RegisterRoutes)
	return r
}

// --- HandleGetPoint Tests ---

func TestHandleGetPoint_Success(t *testing.T) {
	now := time.Now().UTC()
	temp := 25.0
	svc := &mockForecastService{
		pointResult: &forecasts.ForecastResponse{
			Location:    types.Location{Lat: 40.7, Lon: -74.0},
			GeneratedAt: now,
			ModelRuns: map[string]time.Time{
				"medium_range": now.Add(-1 * time.Hour),
			},
			Metadata: forecasts.ResponseMetadata{
				Status: "fresh",
			},
			Data: []forecasts.ForecastDataPoint{
				{
					ValidTime:    now,
					Source:       "medium_range",
					TemperatureC: &temp,
				},
			},
		},
	}

	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/point?lat=40.7&lon=-74.0", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	// Verify JSON structure.
	var resp core.APIResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Data == nil {
		t.Fatal("expected data in response")
	}

	// Verify cache headers.
	cacheControl := rec.Header().Get("Cache-Control")
	if cacheControl != "private, max-age=300" {
		t.Errorf("expected Cache-Control 'private, max-age=300', got '%s'", cacheControl)
	}

	lastModified := rec.Header().Get("Last-Modified")
	if lastModified == "" {
		t.Error("expected Last-Modified header to be set")
	}
}

func TestHandleGetPoint_MissingLat(t *testing.T) {
	svc := &mockForecastService{}
	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/point?lon=-74.0", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestHandleGetPoint_MissingLon(t *testing.T) {
	svc := &mockForecastService{}
	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/point?lat=40.7", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestHandleGetPoint_InvalidLat(t *testing.T) {
	svc := &mockForecastService{}
	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/point?lat=abc&lon=-74.0", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	var resp core.APIErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if resp.Error.Code != string(types.ErrCodeValidationInvalidLat) {
		t.Errorf("expected error code %s, got %s", types.ErrCodeValidationInvalidLat, resp.Error.Code)
	}
}

func TestHandleGetPoint_WithTimeRange(t *testing.T) {
	now := time.Now().UTC()
	svc := &mockForecastService{
		pointResult: &forecasts.ForecastResponse{
			Location:    types.Location{Lat: 40.7, Lon: -74.0},
			GeneratedAt: now,
			ModelRuns:   map[string]time.Time{},
			Metadata:    forecasts.ResponseMetadata{Status: "fresh"},
			Data:        []forecasts.ForecastDataPoint{},
		},
	}

	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	start := now.Format(time.RFC3339)
	end := now.Add(48 * time.Hour).Format(time.RFC3339)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/point?lat=40.7&lon=-74.0&start="+start+"&end="+end, nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestHandleGetPoint_InvalidStartTime(t *testing.T) {
	svc := &mockForecastService{}
	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/point?lat=40.7&lon=-74.0&start=not-a-date", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestHandleGetPoint_ServiceError(t *testing.T) {
	svc := &mockForecastService{
		pointErr: types.NewAppError(types.ErrCodeUpstreamForecast, "S3 unavailable", nil),
	}

	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/point?lat=40.7&lon=-74.0", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	// upstream_forecast_unavailable maps to 502 per HTTPStatus().
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d", rec.Code)
	}
}

// --- HandleGetBatch Tests ---

func TestHandleGetBatch_Success(t *testing.T) {
	now := time.Now().UTC()
	temp := 25.0
	svc := &mockForecastService{
		batchResult: &forecasts.BatchForecastResult{
			Forecasts: map[string]*forecasts.ForecastResponse{
				"loc1": {
					Location:    types.Location{Lat: 40.7, Lon: -74.0},
					GeneratedAt: now,
					ModelRuns:   map[string]time.Time{},
					Metadata:    forecasts.ResponseMetadata{Status: "fresh"},
					Data: []forecasts.ForecastDataPoint{
						{ValidTime: now, Source: "medium_range", TemperatureC: &temp},
					},
				},
			},
		},
	}

	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	body := map[string]interface{}{
		"locations": []map[string]interface{}{
			{"id": "loc1", "lat": 40.7, "lon": -74.0},
		},
	}
	bodyJSON, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/v1/forecasts/points", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// Verify JSON structure.
	var resp core.APIResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Data == nil {
		t.Fatal("expected data in response")
	}
}

func TestHandleGetBatch_ExceedsLimit(t *testing.T) {
	svc := &mockForecastService{
		batchErr: types.NewAppError(types.ErrCodeValidationBatchSize, "batch too large", nil),
	}

	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	// Create a request with 51 locations.
	locs := make([]map[string]interface{}, 51)
	for i := range locs {
		locs[i] = map[string]interface{}{"id": "loc", "lat": 40.0, "lon": -74.0}
	}

	body := map[string]interface{}{
		"locations": locs,
	}
	bodyJSON, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/v1/forecasts/points", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetBatch_InvalidJSON(t *testing.T) {
	svc := &mockForecastService{}
	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/forecasts/points", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

// --- HandleListVars Tests ---

func TestHandleListVars_Success(t *testing.T) {
	svc := &mockForecastService{
		variablesResult: []forecasts.VariableResponseMetadata{
			{
				VariableMetadata: types.VariableMetadata{
					ID:   "temperature_c",
					Unit: "celsius",
				},
				Name:            "Temperature",
				SupportedModels: []types.ForecastType{types.ForecastMediumRange},
			},
		},
	}

	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/variables", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

// --- HandleGetStatus Tests ---

func TestHandleGetStatus_Success(t *testing.T) {
	now := time.Now().UTC()
	svc := &mockForecastService{
		statusResult: &forecasts.SystemStatus{
			Timestamp: now,
			Models: map[string]forecasts.ModelStatus{
				"medium_range": {
					Status:    forecasts.StatusHealthy,
					LatestRun: now.Add(-2 * time.Hour),
					Horizon:   "240h",
					Coverage:  "Global",
				},
			},
		},
	}

	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/status", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp core.APIResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Data == nil {
		t.Fatal("expected data in response")
	}
}

// --- HandleGetVerification Tests ---

func TestHandleGetVerification_Success(t *testing.T) {
	now := time.Now().UTC()
	svc := &mockForecastService{
		verificationResult: &forecasts.VerificationReport{
			Model:     "medium_range",
			StartTime: now.Add(-7 * 24 * time.Hour),
			EndTime:   now,
			Metrics:   []types.VerificationMetric{},
		},
	}

	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/verification?model=medium_range", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestHandleGetVerification_MissingModel(t *testing.T) {
	svc := &mockForecastService{}
	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/verification", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestHandleGetVerification_WithTimeRange(t *testing.T) {
	now := time.Now().UTC()
	svc := &mockForecastService{
		verificationResult: &forecasts.VerificationReport{
			Model:   "medium_range",
			Metrics: []types.VerificationMetric{},
		},
	}

	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	start := now.Add(-14 * 24 * time.Hour).Format(time.RFC3339)
	end := now.Format(time.RFC3339)

	req := httptest.NewRequest(http.MethodGet, "/v1/forecasts/verification?model=medium_range&start="+start+"&end="+end, nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

// --- Route Registration Tests ---

func TestForecastHandler_RouteRegistration(t *testing.T) {
	svc := &mockForecastService{
		pointResult: &forecasts.ForecastResponse{
			Location:    types.Location{},
			GeneratedAt: time.Now().UTC(),
			ModelRuns:   map[string]time.Time{},
			Metadata:    forecasts.ResponseMetadata{Status: "fresh"},
			Data:        []forecasts.ForecastDataPoint{},
		},
		variablesResult: []forecasts.VariableResponseMetadata{},
		statusResult: &forecasts.SystemStatus{
			Timestamp: time.Now().UTC(),
			Models:    map[string]forecasts.ModelStatus{},
		},
		verificationResult: &forecasts.VerificationReport{
			Metrics: []types.VerificationMetric{},
		},
	}

	handler := newTestForecastHandler(svc)
	router := makeForecastRouter(handler)

	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"point forecast", http.MethodGet, "/v1/forecasts/point?lat=40.7&lon=-74.0", http.StatusOK},
		{"variables", http.MethodGet, "/v1/forecasts/variables", http.StatusOK},
		{"status", http.MethodGet, "/v1/forecasts/status", http.StatusOK},
		{"verification", http.MethodGet, "/v1/forecasts/verification?model=medium_range", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()

			router.ServeHTTP(rec, req)

			if rec.Code != tt.status {
				t.Errorf("expected status %d, got %d; body: %s", tt.status, rec.Code, rec.Body.String())
			}
		})
	}
}
