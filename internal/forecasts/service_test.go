package forecasts

import (
	"context"
	"errors"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// --- Mock Dependencies ---

// mockClock is a test clock that returns a fixed time.
type mockClock struct {
	now time.Time
}

func (c *mockClock) Now() time.Time { return c.now }

// mockRunRepo implements ForecastRunRepository for testing.
type mockRunRepo struct {
	runs map[types.ForecastType]*types.ForecastRun
	err  error
}

func (m *mockRunRepo) GetLatestServing(_ context.Context, model types.ForecastType) (*types.ForecastRun, error) {
	if m.err != nil {
		return nil, m.err
	}
	run, ok := m.runs[model]
	if !ok {
		return nil, nil
	}
	return run, nil
}

// mockReader implements ForecastReader for testing.
type mockReader struct {
	pointData map[types.ForecastType][]ForecastDataPoint
	tileData  map[string]map[string][]ForecastDataPoint
	pointErr  error
	tileErr   error
}

func (m *mockReader) ReadPoint(_ context.Context, rc RunContext, _ types.Location, _ []string) ([]ForecastDataPoint, error) {
	if m.pointErr != nil {
		return nil, m.pointErr
	}
	data, ok := m.pointData[rc.Model]
	if !ok {
		return nil, nil
	}
	return data, nil
}

func (m *mockReader) ReadTile(_ context.Context, _ RunContext, tileID string, locs []types.LocationIdent, _ []string) (map[string][]ForecastDataPoint, error) {
	if m.tileErr != nil {
		return nil, m.tileErr
	}
	data, ok := m.tileData[tileID]
	if !ok {
		return make(map[string][]ForecastDataPoint), nil
	}
	return data, nil
}

// mockVerificationRepo implements VerificationRepository for testing.
type mockVerificationRepo struct {
	metrics []types.VerificationMetric
	err     error
}

func (m *mockVerificationRepo) GetAggregatedMetrics(_ context.Context, _ string, _, _ time.Time) ([]types.VerificationMetric, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.metrics, nil
}

// --- Helper Functions ---

func makeTime(hours int) time.Time {
	return time.Date(2026, 2, 6, hours, 0, 0, 0, time.UTC)
}

func makeRun(model types.ForecastType, ts time.Time, path string) *types.ForecastRun {
	return &types.ForecastRun{
		ID:           "run_" + string(model),
		Model:        model,
		RunTimestamp: ts,
		StoragePath:  path,
		Status:       "complete",
	}
}

func makeMRDataPoints(now time.Time) []ForecastDataPoint {
	points := make([]ForecastDataPoint, 8)
	for i := range points {
		t := now.Add(time.Duration(i) * 3 * time.Hour)
		temp := 20.0 + float64(i)
		precip := 0.5
		points[i] = ForecastDataPoint{
			ValidTime:    t,
			Source:       string(types.ForecastMediumRange),
			TemperatureC: &temp,
			PrecipitationMM: &precip,
		}
	}
	return points
}

func makeNCDataPoints(now time.Time) []ForecastDataPoint {
	points := make([]ForecastDataPoint, 4)
	for i := range points {
		t := now.Add(time.Duration(i) * time.Hour)
		temp := 22.0 + float64(i)
		precip := 1.0
		points[i] = ForecastDataPoint{
			ValidTime:    t,
			Source:       string(types.ForecastNowcast),
			TemperatureC: &temp,
			PrecipitationMM: &precip,
		}
	}
	return points
}

// --- Tests ---

func TestGetPointForecast_MediumRangeOnly_OutsideCONUS(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-1*time.Hour), "mr/path")
	mrData := makeMRDataPoints(now)

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
		},
	}

	reader := &mockReader{
		pointData: map[types.ForecastType][]ForecastDataPoint{
			types.ForecastMediumRange: mrData,
		},
	}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	// London (outside CONUS) -- should use Medium-Range only.
	result, err := svc.GetPointForecast(context.Background(), 51.5, -0.1, now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Should only have Medium-Range model run.
	if _, ok := result.ModelRuns[string(types.ForecastNowcast)]; ok {
		t.Error("expected no Nowcast model run for non-CONUS location")
	}

	if _, ok := result.ModelRuns[string(types.ForecastMediumRange)]; !ok {
		t.Error("expected Medium-Range model run")
	}

	if len(result.Data) != len(mrData) {
		t.Errorf("expected %d data points, got %d", len(mrData), len(result.Data))
	}

	// No fallback warnings should be present.
	if len(result.Metadata.Warnings) != 0 {
		t.Errorf("expected no warnings for non-CONUS, got %d", len(result.Metadata.Warnings))
	}
}

func TestGetPointForecast_BlendingLogic_CONUS(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-1*time.Hour), "mr/path")
	ncRun := makeRun(types.ForecastNowcast, now.Add(-10*time.Minute), "nc/path")
	mrData := makeMRDataPoints(now)
	ncData := makeNCDataPoints(now)

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
			types.ForecastNowcast:     ncRun,
		},
	}

	reader := &mockReader{
		pointData: map[types.ForecastType][]ForecastDataPoint{
			types.ForecastMediumRange: mrData,
			types.ForecastNowcast:     ncData,
		},
	}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	// New York City (within CONUS)
	result, err := svc.GetPointForecast(context.Background(), 40.7, -74.0, now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Should have both model runs.
	if _, ok := result.ModelRuns[string(types.ForecastNowcast)]; !ok {
		t.Error("expected Nowcast model run for CONUS location")
	}
	if _, ok := result.ModelRuns[string(types.ForecastMediumRange)]; !ok {
		t.Error("expected Medium-Range model run")
	}

	// Should have blended data: Nowcast points within 6h + MR points beyond 6h.
	if len(result.Data) == 0 {
		t.Error("expected non-empty blended data")
	}

	// Check that short-range data comes from Nowcast.
	hasNowcast := false
	hasMR := false
	for _, dp := range result.Data {
		if dp.Source == "nowcast" {
			hasNowcast = true
		}
		if dp.Source == "medium_range" {
			hasMR = true
		}
	}
	if !hasNowcast {
		t.Error("expected some data points with source 'nowcast'")
	}
	if !hasMR {
		t.Error("expected some data points with source 'medium_range'")
	}
}

func TestGetPointForecast_NowcastFallback_StaleData(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-1*time.Hour), "mr/path")
	// Nowcast run is 2 hours old (exceeds 90-minute staleness threshold).
	ncRun := makeRun(types.ForecastNowcast, now.Add(-2*time.Hour), "nc/path")
	mrData := makeMRDataPoints(now)

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
			types.ForecastNowcast:     ncRun,
		},
	}

	reader := &mockReader{
		pointData: map[types.ForecastType][]ForecastDataPoint{
			types.ForecastMediumRange: mrData,
		},
	}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	// CONUS location with stale Nowcast.
	result, err := svc.GetPointForecast(context.Background(), 40.7, -74.0, now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have a fallback warning.
	if len(result.Metadata.Warnings) == 0 {
		t.Error("expected fallback warning when Nowcast is stale")
	}

	foundFallback := false
	for _, w := range result.Metadata.Warnings {
		if w.Code == "source_fallback_medium_range" {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Error("expected 'source_fallback_medium_range' warning code")
	}

	// Should only use Medium-Range data (no Nowcast in model runs).
	if _, ok := result.ModelRuns[string(types.ForecastNowcast)]; ok {
		t.Error("expected no Nowcast model run when data is stale")
	}
}

func TestGetPointForecast_NowcastFallback_NoRun(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-1*time.Hour), "mr/path")
	mrData := makeMRDataPoints(now)

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
			// No Nowcast run available.
		},
	}

	reader := &mockReader{
		pointData: map[types.ForecastType][]ForecastDataPoint{
			types.ForecastMediumRange: mrData,
		},
	}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	// CONUS location with no Nowcast run.
	result, err := svc.GetPointForecast(context.Background(), 40.7, -74.0, now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Metadata.Warnings) == 0 {
		t.Error("expected fallback warning when no Nowcast run is available")
	}

	// Should still succeed with Medium-Range data.
	if len(result.Data) != len(mrData) {
		t.Errorf("expected %d data points, got %d", len(mrData), len(result.Data))
	}
}

func TestGetPointForecast_NowcastReadFailure_GracefulDegradation(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-1*time.Hour), "mr/path")
	ncRun := makeRun(types.ForecastNowcast, now.Add(-10*time.Minute), "nc/path")
	mrData := makeMRDataPoints(now)

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
			types.ForecastNowcast:     ncRun,
		},
	}

	// Reader that fails on Nowcast reads.
	reader := &mockReader{
		pointData: map[types.ForecastType][]ForecastDataPoint{
			types.ForecastMediumRange: mrData,
			// Nowcast data not provided; the reader will return nil.
		},
	}
	// We need a reader that specifically fails for Nowcast.
	failingReader := &selectiveFailReader{
		base:       reader,
		failModel:  types.ForecastNowcast,
		failErr:    errors.New("S3 timeout"),
	}

	svc := NewForecastService(repo, failingReader, nil, nil, clock)

	// CONUS location where Nowcast read fails.
	result, err := svc.GetPointForecast(context.Background(), 40.7, -74.0, now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should gracefully degrade to Medium-Range.
	if len(result.Data) == 0 {
		t.Error("expected data despite Nowcast read failure")
	}

	foundFallback := false
	for _, w := range result.Metadata.Warnings {
		if w.Code == "source_fallback_medium_range" {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Error("expected fallback warning when Nowcast read fails")
	}
}

// selectiveFailReader wraps a ForecastReader and fails for a specific model.
type selectiveFailReader struct {
	base      ForecastReader
	failModel types.ForecastType
	failErr   error
}

func (r *selectiveFailReader) ReadPoint(ctx context.Context, rc RunContext, loc types.Location, vars []string) ([]ForecastDataPoint, error) {
	if rc.Model == r.failModel {
		return nil, r.failErr
	}
	return r.base.ReadPoint(ctx, rc, loc, vars)
}

func (r *selectiveFailReader) ReadTile(ctx context.Context, rc RunContext, tileID string, locs []types.LocationIdent, vars []string) (map[string][]ForecastDataPoint, error) {
	if rc.Model == r.failModel {
		return nil, r.failErr
	}
	return r.base.ReadTile(ctx, rc, tileID, locs, vars)
}

func TestGetPointForecast_InvalidLocation(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}
	repo := &mockRunRepo{runs: map[types.ForecastType]*types.ForecastRun{}}
	reader := &mockReader{}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	_, err := svc.GetPointForecast(context.Background(), 100.0, -74.0, now, now.Add(24*time.Hour))
	if err == nil {
		t.Fatal("expected error for invalid latitude")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeValidationInvalidLat {
		t.Errorf("expected error code %s, got %s", types.ErrCodeValidationInvalidLat, appErr.Code)
	}
}

func TestGetPointForecast_NoMediumRangeRun(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			// No Medium-Range run available.
		},
	}

	reader := &mockReader{}
	svc := NewForecastService(repo, reader, nil, nil, clock)

	_, err := svc.GetPointForecast(context.Background(), 40.7, -74.0, now, now.Add(24*time.Hour))
	if err == nil {
		t.Fatal("expected error when no medium-range run available")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeUpstreamForecast {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamForecast, appErr.Code)
	}
}

func TestGetPointForecast_StatusDegraded_StaleRun(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	// Medium-Range run is 10 hours old (between 8h and 12h = degraded).
	mrRun := makeRun(types.ForecastMediumRange, now.Add(-10*time.Hour), "mr/path")
	mrData := makeMRDataPoints(now)

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
		},
	}

	reader := &mockReader{
		pointData: map[types.ForecastType][]ForecastDataPoint{
			types.ForecastMediumRange: mrData,
		},
	}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	// Non-CONUS location to avoid Nowcast complications.
	result, err := svc.GetPointForecast(context.Background(), 51.5, -0.1, now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Metadata.Status != "degraded" {
		t.Errorf("expected status 'degraded', got '%s'", result.Metadata.Status)
	}
}

func TestGetPointForecast_StatusStale_VeryOldRun(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	// Medium-Range run is 13 hours old (> 12h = stale).
	mrRun := makeRun(types.ForecastMediumRange, now.Add(-13*time.Hour), "mr/path")
	mrData := makeMRDataPoints(now)

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
		},
	}

	reader := &mockReader{
		pointData: map[types.ForecastType][]ForecastDataPoint{
			types.ForecastMediumRange: mrData,
		},
	}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	result, err := svc.GetPointForecast(context.Background(), 51.5, -0.1, now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Metadata.Status != "stale" {
		t.Errorf("expected status 'stale', got '%s'", result.Metadata.Status)
	}
}

// --- Batch Forecast Tests ---

func TestGetBatchForecast_Success(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-1*time.Hour), "mr/path")
	mrData := makeMRDataPoints(now)

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
		},
	}

	tileID1 := CalculateTileID(40.7, -74.0)

	reader := &mockReader{
		tileData: map[string]map[string][]ForecastDataPoint{
			tileID1: {
				"loc1": mrData,
				"loc2": mrData,
			},
		},
	}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	req := BatchForecastRequest{
		Locations: []types.LocationIdent{
			{ID: "loc1", Lat: 40.7, Lon: -74.0},
			{ID: "loc2", Lat: 40.8, Lon: -74.1},
		},
		Start: now,
		End:   now.Add(24 * time.Hour),
	}

	result, err := svc.GetBatchForecast(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Forecasts) != 2 {
		t.Errorf("expected 2 forecasts, got %d", len(result.Forecasts))
	}

	if result.Forecasts["loc1"] == nil {
		t.Error("expected forecast for loc1")
	}

	if result.Forecasts["loc2"] == nil {
		t.Error("expected forecast for loc2")
	}
}

func TestGetBatchForecast_ExceedsMaxSize(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}
	repo := &mockRunRepo{runs: map[types.ForecastType]*types.ForecastRun{}}
	reader := &mockReader{}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	locs := make([]types.LocationIdent, 51)
	for i := range locs {
		locs[i] = types.LocationIdent{ID: "loc", Lat: 40.0, Lon: -74.0}
	}

	req := BatchForecastRequest{
		Locations: locs,
		Start:     now,
		End:       now.Add(24 * time.Hour),
	}

	_, err := svc.GetBatchForecast(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for batch size exceeding limit")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeValidationBatchSize {
		t.Errorf("expected error code %s, got %s", types.ErrCodeValidationBatchSize, appErr.Code)
	}
}

func TestGetBatchForecast_EmptyInput(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}
	repo := &mockRunRepo{runs: map[types.ForecastType]*types.ForecastRun{}}
	reader := &mockReader{}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	result, err := svc.GetBatchForecast(context.Background(), BatchForecastRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Forecasts) != 0 {
		t.Errorf("expected empty forecasts map, got %d", len(result.Forecasts))
	}
}

func TestGetBatchForecast_InvalidVariable(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-1*time.Hour), "mr/path")
	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
		},
	}
	reader := &mockReader{}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	req := BatchForecastRequest{
		Locations: []types.LocationIdent{
			{ID: "loc1", Lat: 40.7, Lon: -74.0},
		},
		Variables: []string{"nonexistent_variable"},
	}

	_, err := svc.GetBatchForecast(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for invalid variable")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeValidationInvalidVariable {
		t.Errorf("expected error code %s, got %s", types.ErrCodeValidationInvalidVariable, appErr.Code)
	}
}

func TestGetBatchForecast_PartialTileFailure(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-1*time.Hour), "mr/path")
	mrData := makeMRDataPoints(now)

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
		},
	}

	tile1 := CalculateTileID(40.7, -74.0)
	tile2 := CalculateTileID(51.5, -0.1)

	// Create a reader that succeeds for tile1 but fails for tile2.
	reader := &partialTileFailReader{
		successTile: tile1,
		successData: map[string][]ForecastDataPoint{
			"loc1": mrData,
		},
		failTile: tile2,
		failErr:  errors.New("S3 timeout for tile"),
	}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	req := BatchForecastRequest{
		Locations: []types.LocationIdent{
			{ID: "loc1", Lat: 40.7, Lon: -74.0},  // tile1 (succeeds)
			{ID: "loc2", Lat: 51.5, Lon: -0.1},    // tile2 (fails)
		},
		Start: now,
		End:   now.Add(24 * time.Hour),
	}

	result, err := svc.GetBatchForecast(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// loc1 should succeed.
	if result.Forecasts["loc1"] == nil {
		t.Error("expected forecast for loc1 (tile succeeded)")
	}

	// loc2 should be in errors.
	if result.Errors == nil || len(result.Errors) == 0 {
		t.Error("expected errors for loc2 (tile failed)")
	}
	if _, ok := result.Errors["loc2"]; !ok {
		t.Error("expected error entry for loc2")
	}
}

// partialTileFailReader succeeds for one tile and fails for another.
type partialTileFailReader struct {
	successTile string
	successData map[string][]ForecastDataPoint
	failTile    string
	failErr     error
}

func (r *partialTileFailReader) ReadPoint(_ context.Context, _ RunContext, _ types.Location, _ []string) ([]ForecastDataPoint, error) {
	return nil, nil
}

func (r *partialTileFailReader) ReadTile(_ context.Context, _ RunContext, tileID string, _ []types.LocationIdent, _ []string) (map[string][]ForecastDataPoint, error) {
	if tileID == r.failTile {
		return nil, r.failErr
	}
	if tileID == r.successTile {
		return r.successData, nil
	}
	return make(map[string][]ForecastDataPoint), nil
}

// --- Status Tests ---

func TestGetStatus_HealthyModels(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-2*time.Hour), "mr/path")
	ncRun := makeRun(types.ForecastNowcast, now.Add(-20*time.Minute), "nc/path")

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
			types.ForecastNowcast:     ncRun,
		},
	}

	svc := NewForecastService(repo, nil, nil, nil, clock)

	status, err := svc.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mrStatus := status.Models["medium_range"]
	if mrStatus.Status != StatusHealthy {
		t.Errorf("expected Medium-Range status 'healthy', got '%s'", mrStatus.Status)
	}

	ncStatus := status.Models["nowcast"]
	if ncStatus.Status != StatusHealthy {
		t.Errorf("expected Nowcast status 'healthy', got '%s'", ncStatus.Status)
	}
}

func TestGetStatus_DegradedModels(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-10*time.Hour), "mr/path")
	ncRun := makeRun(types.ForecastNowcast, now.Add(-60*time.Minute), "nc/path")

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
			types.ForecastNowcast:     ncRun,
		},
	}

	svc := NewForecastService(repo, nil, nil, nil, clock)

	status, err := svc.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mrStatus := status.Models["medium_range"]
	if mrStatus.Status != StatusDegraded {
		t.Errorf("expected Medium-Range status 'degraded' for 10h-old run, got '%s'", mrStatus.Status)
	}

	ncStatus := status.Models["nowcast"]
	if ncStatus.Status != StatusDegraded {
		t.Errorf("expected Nowcast status 'degraded' for 60m-old run, got '%s'", ncStatus.Status)
	}
}

func TestGetStatus_StaleModels(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-13*time.Hour), "mr/path")
	ncRun := makeRun(types.ForecastNowcast, now.Add(-2*time.Hour), "nc/path")

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
			types.ForecastNowcast:     ncRun,
		},
	}

	svc := NewForecastService(repo, nil, nil, nil, clock)

	status, err := svc.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mrStatus := status.Models["medium_range"]
	if mrStatus.Status != StatusStale {
		t.Errorf("expected Medium-Range status 'stale' for 13h-old run, got '%s'", mrStatus.Status)
	}

	ncStatus := status.Models["nowcast"]
	if ncStatus.Status != StatusStale {
		t.Errorf("expected Nowcast status 'stale' for 2h-old run, got '%s'", ncStatus.Status)
	}
}

// --- Snapshot Tests ---

func TestGetSnapshot_MediumRangeOnly(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	mrRun := makeRun(types.ForecastMediumRange, now.Add(-1*time.Hour), "mr/path")
	temp := 25.0
	precip := 2.5
	precipProb := 60.0
	wind := 15.0
	humidity := 70.0

	mrData := []ForecastDataPoint{
		{
			ValidTime:                now,
			Source:                   string(types.ForecastMediumRange),
			TemperatureC:             &temp,
			PrecipitationMM:         &precip,
			PrecipitationProbability: &precipProb,
			WindSpeedKmh:             &wind,
			Humidity:                 &humidity,
		},
	}

	repo := &mockRunRepo{
		runs: map[types.ForecastType]*types.ForecastRun{
			types.ForecastMediumRange: mrRun,
		},
	}

	reader := &mockReader{
		pointData: map[types.ForecastType][]ForecastDataPoint{
			types.ForecastMediumRange: mrData,
		},
	}

	svc := NewForecastService(repo, reader, nil, nil, clock)

	snapshot, err := svc.GetSnapshot(context.Background(), 40.7, -74.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if snapshot.TemperatureC != 25.0 {
		t.Errorf("expected temperature 25.0, got %f", snapshot.TemperatureC)
	}
	if snapshot.PrecipitationMM != 2.5 {
		t.Errorf("expected precipitation 2.5, got %f", snapshot.PrecipitationMM)
	}
	if snapshot.PrecipitationProb != 60.0 {
		t.Errorf("expected precipitation prob 60.0, got %f", snapshot.PrecipitationProb)
	}
	if snapshot.WindSpeedKmh != 15.0 {
		t.Errorf("expected wind speed 15.0, got %f", snapshot.WindSpeedKmh)
	}
	if snapshot.Humidity != 70.0 {
		t.Errorf("expected humidity 70.0, got %f", snapshot.Humidity)
	}
}

// --- Variables Tests ---

func TestGetVariables_ReturnsAllStandard(t *testing.T) {
	svc := NewForecastService(nil, nil, nil, nil, nil)

	vars, err := svc.GetVariables(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(vars) != len(types.StandardVariables) {
		t.Errorf("expected %d variables, got %d", len(types.StandardVariables), len(vars))
	}
}

// --- Verification Tests ---

func TestGetVerificationMetrics_Success(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	metrics := []types.VerificationMetric{
		{
			Model:      types.ForecastMediumRange,
			Variable:   "temperature_c",
			MetricType: "mae",
			Value:      1.5,
			Timestamp:  now,
		},
	}

	vRepo := &mockVerificationRepo{metrics: metrics}
	svc := NewForecastService(nil, nil, vRepo, nil, clock)

	report, err := svc.GetVerificationMetrics(context.Background(), "medium_range", now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if report.Model != "medium_range" {
		t.Errorf("expected model 'medium_range', got '%s'", report.Model)
	}

	if len(report.Metrics) != 1 {
		t.Errorf("expected 1 metric, got %d", len(report.Metrics))
	}
}

func TestGetVerificationMetrics_NilRepo(t *testing.T) {
	now := makeTime(12)
	clock := &mockClock{now: now}

	svc := NewForecastService(nil, nil, nil, nil, clock)

	report, err := svc.GetVerificationMetrics(context.Background(), "medium_range", now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(report.Metrics) != 0 {
		t.Errorf("expected 0 metrics with nil repo, got %d", len(report.Metrics))
	}
}
