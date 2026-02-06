// Package forecasts implements the forecast data retrieval service and Zarr-based
// reader for the WatchPoint platform.
//
// This file contains the ForecastService implementation, which encapsulates the
// business logic for model blending (Nowcast vs. Medium-Range), batch optimization
// via tile grouping, and forecast data retrieval. It implements the ForecastService
// interface defined in architecture/05c-api-forecasts.md Section 3.
package forecasts

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"watchpoint/internal/types"
)

// Staleness thresholds for model health determination.
// Per FQRY-001 and FQRY-004 flow simulations.
const (
	// NowcastStalenessThreshold is the age beyond which Nowcast data is considered
	// stale and should fall back to Medium-Range.
	NowcastStalenessThreshold = 90 * time.Minute

	// NowcastHorizon is the time range covered by Nowcast (HRRR/StormScope).
	// Beyond this horizon, Medium-Range data is used.
	NowcastHorizon = 6 * time.Hour

	// MediumRangeHealthyAge is the maximum age for healthy Medium-Range data.
	MediumRangeHealthyAge = 8 * time.Hour
	// MediumRangeDegradedAge is the maximum age for degraded Medium-Range data.
	MediumRangeDegradedAge = 12 * time.Hour

	// NowcastHealthyAge is the maximum age for healthy Nowcast data.
	NowcastHealthyAge = 45 * time.Minute
	// NowcastDegradedAge is the maximum age for degraded Nowcast data.
	NowcastDegradedAge = 90 * time.Minute

	// BatchConcurrencyLimit is the maximum number of concurrent S3 tile fetches.
	// Per FQRY-002 flow simulation: Semaphore limit of 10.
	BatchConcurrencyLimit = 10

	// MaxBatchLocations is the maximum number of locations in a batch request.
	MaxBatchLocations = 50
)

// ForecastResponse represents a time-series forecast for a single location.
// Per architecture/05c-api-forecasts.md Section 2.1.
type ForecastResponse struct {
	Location    types.Location       `json:"location"`
	GeneratedAt time.Time            `json:"generated_at"`
	ModelRuns   map[string]time.Time `json:"model_runs"`
	Metadata    ResponseMetadata     `json:"metadata,omitempty"`
	Data        []ForecastDataPoint  `json:"forecast"`
}

// ResponseMetadata contains status and warning information for the response.
type ResponseMetadata struct {
	Status   string            `json:"status"`
	Warnings []ResponseWarning `json:"warnings,omitempty"`
}

// ResponseWarning represents a single advisory warning in the response.
type ResponseWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BatchForecastRequest allows querying multiple points in one call.
// Per architecture/05c-api-forecasts.md Section 2.3.
type BatchForecastRequest struct {
	Locations []types.LocationIdent `json:"locations" validate:"max=50"`
	Start     time.Time             `json:"start"`
	End       time.Time             `json:"end"`
	Variables []string              `json:"variables"`
}

// BatchForecastResult separates successes from failures.
// Per architecture/05c-api-forecasts.md Section 2.3.
type BatchForecastResult struct {
	Forecasts map[string]*ForecastResponse `json:"forecasts"`
	Errors    map[string]ErrorDetail       `json:"errors,omitempty"`
}

// ErrorDetail is a lightweight error structure used in batch error maps.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// VariableResponseMetadata extends the canonical VariableMetadata with API-specific fields.
// Per architecture/05c-api-forecasts.md Section 2.2.
type VariableResponseMetadata struct {
	types.VariableMetadata
	Name            string               `json:"name"`
	SupportedModels []types.ForecastType `json:"supported_models"`
}

// SystemStatus reports the health of the forecast pipeline.
// Per architecture/05c-api-forecasts.md Section 2.2.
type SystemStatus struct {
	Timestamp time.Time              `json:"timestamp"`
	Models    map[string]ModelStatus `json:"models"`
}

// ModelStatus reports the status of a single forecast model.
type ModelStatus struct {
	Status    StatusEnum `json:"status"`
	LatestRun time.Time  `json:"latest_run_at"`
	Horizon   string     `json:"horizon"`
	Coverage  string     `json:"coverage"`
}

// StatusEnum represents the health status of a model.
type StatusEnum string

const (
	StatusHealthy  StatusEnum = "healthy"
	StatusDegraded StatusEnum = "degraded"
	StatusStale    StatusEnum = "stale"
)

// VerificationReport contains aggregated verification metrics.
// Per architecture/05c-api-forecasts.md Section 2.3.
type VerificationReport struct {
	Model     string                     `json:"model"`
	StartTime time.Time                  `json:"start_time"`
	EndTime   time.Time                  `json:"end_time"`
	Metrics   []types.VerificationMetric `json:"metrics"`
}

// ForecastService defines the business logic interface for forecast retrieval.
// Per architecture/05c-api-forecasts.md Section 3.
type ForecastService interface {
	GetPointForecast(ctx context.Context, lat, lon float64, start, end time.Time) (*ForecastResponse, error)
	GetBatchForecast(ctx context.Context, req BatchForecastRequest) (*BatchForecastResult, error)
	GetVariables(ctx context.Context) ([]VariableResponseMetadata, error)
	GetStatus(ctx context.Context) (*SystemStatus, error)
	GetSnapshot(ctx context.Context, lat, lon float64) (*types.ForecastSnapshot, error)
	GetVerificationMetrics(ctx context.Context, model string, start, end time.Time) (*VerificationReport, error)
}

// ForecastRunRepository provides data access for the forecast_runs table.
// Per architecture/05c-api-forecasts.md Section 4.1.
type ForecastRunRepository interface {
	GetLatestServing(ctx context.Context, model types.ForecastType) (*types.ForecastRun, error)
}

// VerificationRepository provides access to verification metrics.
type VerificationRepository interface {
	GetAggregatedMetrics(ctx context.Context, model string, start, end time.Time) ([]types.VerificationMetric, error)
}

// forecastService is the concrete implementation of ForecastService.
type forecastService struct {
	runRepo          ForecastRunRepository
	reader           ForecastReader
	verificationRepo VerificationRepository
	logger           *slog.Logger
	clock            types.Clock
}

// NewForecastService creates a new ForecastService with the provided dependencies.
func NewForecastService(
	runRepo ForecastRunRepository,
	reader ForecastReader,
	verificationRepo VerificationRepository,
	logger *slog.Logger,
	clock types.Clock,
) ForecastService {
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = types.RealClock{}
	}
	return &forecastService{
		runRepo:          runRepo,
		reader:           reader,
		verificationRepo: verificationRepo,
		logger:           logger,
		clock:            clock,
	}
}

// defaultVars returns the default set of variables to fetch when none are specified.
func defaultVars() []string {
	return []string{
		types.ZarrVarTemperatureC,
		types.ZarrVarPrecipitationMM,
		types.ZarrVarPrecipitationProb,
		types.ZarrVarWindSpeedKmh,
		types.ZarrVarHumidityPercent,
		types.ZarrVarCloudCoverPercent,
	}
}

// GetPointForecast retrieves a forecast for a single point with model blending.
// Per FQRY-001 flow simulation:
//  1. Parse and validate inputs.
//  2. Determine required models based on time range and location.
//  3. Fetch latest serving runs from DB.
//  4. Read data from Zarr/S3 via ForecastReader.
//  5. Apply model blending: Nowcast for T<6h (CONUS only), Medium-Range for T>6h.
//  6. Construct ForecastResponse with appropriate metadata and warnings.
func (s *forecastService) GetPointForecast(ctx context.Context, lat, lon float64, start, end time.Time) (*ForecastResponse, error) {
	// Validate location.
	if err := validateLocation(lat, lon); err != nil {
		return nil, err
	}

	now := s.clock.Now()
	loc := types.Location{Lat: lat, Lon: lon}
	vars := defaultVars()

	// Determine if we need Nowcast (location in CONUS and start within nowcast horizon).
	needsNowcast := types.IsCONUS(lat, lon) && start.Before(now.Add(NowcastHorizon))

	// Always fetch Medium-Range run (required for >6h and as fallback).
	mrRun, err := s.runRepo.GetLatestServing(ctx, types.ForecastMediumRange)
	if err != nil {
		return nil, err
	}
	if mrRun == nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: "no medium-range forecast data available",
		}
	}

	modelRuns := map[string]time.Time{
		string(types.ForecastMediumRange): mrRun.RunTimestamp,
	}

	warnings := []ResponseWarning{}
	fallbackMode := false

	// Fetch Nowcast run if needed.
	var ncRun *types.ForecastRun
	if needsNowcast {
		ncRun, err = s.runRepo.GetLatestServing(ctx, types.ForecastNowcast)
		if err != nil {
			s.logger.WarnContext(ctx, "failed to fetch nowcast run, using fallback",
				"error", err,
			)
			fallbackMode = true
		} else if ncRun == nil {
			s.logger.WarnContext(ctx, "no nowcast run available, using fallback")
			fallbackMode = true
		} else {
			// Check staleness: if Nowcast is older than 90 minutes, flag fallback.
			age := now.Sub(ncRun.RunTimestamp)
			if age > NowcastStalenessThreshold {
				s.logger.WarnContext(ctx, "nowcast data is stale, using fallback",
					"age", age.String(),
					"threshold", NowcastStalenessThreshold.String(),
				)
				fallbackMode = true
			} else {
				modelRuns[string(types.ForecastNowcast)] = ncRun.RunTimestamp
			}
		}

		if fallbackMode {
			warnings = append(warnings, ResponseWarning{
				Code:    "source_fallback_medium_range",
				Message: "Nowcast data unavailable or stale; using Medium-Range fallback",
			})
		}
	}

	// Read Medium-Range data.
	mrRC := RunContext{
		Model:       types.ForecastMediumRange,
		Timestamp:   mrRun.RunTimestamp,
		StoragePath: mrRun.StoragePath,
	}
	mrData, err := s.reader.ReadPoint(ctx, mrRC, loc, vars)
	if err != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: fmt.Sprintf("failed to read medium-range forecast: %v", err),
			Err:     err,
		}
	}

	// Read Nowcast data if needed and not in fallback mode.
	var ncData []ForecastDataPoint
	if needsNowcast && !fallbackMode && ncRun != nil {
		ncRC := RunContext{
			Model:       types.ForecastNowcast,
			Timestamp:   ncRun.RunTimestamp,
			StoragePath: ncRun.StoragePath,
		}
		ncData, err = s.reader.ReadPoint(ctx, ncRC, loc, vars)
		if err != nil {
			// Graceful degradation: fall back to Medium-Range.
			s.logger.WarnContext(ctx, "nowcast read failed, falling back to medium-range",
				"error", err,
			)
			ncData = nil
			warnings = append(warnings, ResponseWarning{
				Code:    "source_fallback_medium_range",
				Message: "Nowcast read failed; using Medium-Range fallback",
			})
		}
	}

	// Apply model blending.
	blendedData := s.blendForecasts(now, mrData, ncData)

	// Determine response status.
	status := "fresh"
	mrAge := now.Sub(mrRun.RunTimestamp)
	if mrAge > MediumRangeDegradedAge {
		status = "stale"
	} else if mrAge > MediumRangeHealthyAge {
		status = "degraded"
	}
	if fallbackMode && status == "fresh" {
		status = "degraded"
	}

	response := &ForecastResponse{
		Location:    loc,
		GeneratedAt: now,
		ModelRuns:   modelRuns,
		Metadata: ResponseMetadata{
			Status:   status,
			Warnings: warnings,
		},
		Data: blendedData,
	}

	return response, nil
}

// blendForecasts merges Nowcast and Medium-Range data according to the blending
// strategy defined in architecture/05c-api-forecasts.md Section 6.1:
//   - T < 6h: Prefer Nowcast if available
//   - T >= 6h: Use Medium-Range
//   - Fallback: If Nowcast is nil (or empty), use Medium-Range for all timesteps
func (s *forecastService) blendForecasts(now time.Time, mrData, ncData []ForecastDataPoint) []ForecastDataPoint {
	if len(ncData) == 0 {
		// No Nowcast data; use Medium-Range exclusively.
		return mrData
	}

	horizon := now.Add(NowcastHorizon)
	result := make([]ForecastDataPoint, 0, len(mrData)+len(ncData))

	// Index Nowcast data by valid time for efficient lookup.
	ncByTime := make(map[time.Time]*ForecastDataPoint, len(ncData))
	for i := range ncData {
		ncByTime[ncData[i].ValidTime] = &ncData[i]
	}

	// First, add Nowcast points that are within the horizon.
	for i := range ncData {
		if ncData[i].ValidTime.Before(horizon) {
			pt := ncData[i]
			pt.Source = "nowcast"
			result = append(result, pt)
		}
	}

	// Then, add Medium-Range points that are at or beyond the horizon,
	// and also any Medium-Range points within the horizon that don't have
	// a corresponding Nowcast point (to fill gaps).
	for i := range mrData {
		if mrData[i].ValidTime.Before(horizon) {
			// Within Nowcast horizon: only use MR if no Nowcast point at this time.
			if _, hasNC := ncByTime[mrData[i].ValidTime]; !hasNC {
				pt := mrData[i]
				pt.Source = "medium_range"
				result = append(result, pt)
			}
		} else {
			// Beyond Nowcast horizon: always use Medium-Range.
			pt := mrData[i]
			pt.Source = "medium_range"
			result = append(result, pt)
		}
	}

	return result
}

// GetBatchForecast retrieves forecasts for multiple locations with tile-based optimization.
// Per FQRY-002 flow simulation:
//  1. Validate batch size.
//  2. Fetch latest runs from DB.
//  3. Group locations by TileID (scatter phase).
//  4. Fetch data concurrently per tile with semaphore (parallel fetch).
//  5. Collect results and handle partial failures (gather phase).
func (s *forecastService) GetBatchForecast(ctx context.Context, req BatchForecastRequest) (*BatchForecastResult, error) {
	if len(req.Locations) == 0 {
		return &BatchForecastResult{
			Forecasts: make(map[string]*ForecastResponse),
		}, nil
	}

	if len(req.Locations) > MaxBatchLocations {
		return nil, &types.AppError{
			Code:    types.ErrCodeValidationBatchSize,
			Message: fmt.Sprintf("batch size %d exceeds maximum of %d locations", len(req.Locations), MaxBatchLocations),
		}
	}

	vars := req.Variables
	if len(vars) == 0 {
		vars = defaultVars()
	}

	// Validate variables.
	for _, v := range vars {
		if _, ok := types.StandardVariables[v]; !ok {
			return nil, &types.AppError{
				Code:    types.ErrCodeValidationInvalidVariable,
				Message: fmt.Sprintf("unsupported variable: %s", v),
			}
		}
	}

	now := s.clock.Now()

	// Fetch latest Medium-Range run (required for all locations).
	mrRun, err := s.runRepo.GetLatestServing(ctx, types.ForecastMediumRange)
	if err != nil {
		return nil, err
	}
	if mrRun == nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: "no medium-range forecast data available",
		}
	}

	mrRC := RunContext{
		Model:       types.ForecastMediumRange,
		Timestamp:   mrRun.RunTimestamp,
		StoragePath: mrRun.StoragePath,
	}

	// Scatter phase: group locations by TileID.
	tileGroups := make(map[string][]types.LocationIdent)
	for _, loc := range req.Locations {
		tileID := CalculateTileID(loc.Lat, loc.Lon)
		tileGroups[tileID] = append(tileGroups[tileID], loc)
	}

	// Parallel fetch with semaphore.
	var mu sync.Mutex
	forecasts := make(map[string]*ForecastResponse)
	errorMap := make(map[string]ErrorDetail)

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(BatchConcurrencyLimit)

	for tileID, locs := range tileGroups {
		tileID := tileID
		locs := locs

		g.Go(func() error {
			tileResults, err := s.reader.ReadTile(gCtx, mrRC, tileID, locs, vars)
			if err != nil {
				// Error isolation: mark all locations in this tile as failed.
				mu.Lock()
				for _, loc := range locs {
					errorMap[loc.ID] = ErrorDetail{
						Code:    string(types.ErrCodeUpstreamForecast),
						Message: fmt.Sprintf("failed to read tile %s: %v", tileID, err),
					}
				}
				mu.Unlock()
				// Do not propagate error to errgroup; allow other tiles to succeed.
				return nil
			}

			mu.Lock()
			for _, loc := range locs {
				data, ok := tileResults[loc.ID]
				if !ok {
					errorMap[loc.ID] = ErrorDetail{
						Code:    string(types.ErrCodeInternalForecastCorruption),
						Message: fmt.Sprintf("no data returned for location %s in tile %s", loc.ID, tileID),
					}
					continue
				}

				forecasts[loc.ID] = &ForecastResponse{
					Location:    types.Location{Lat: loc.Lat, Lon: loc.Lon},
					GeneratedAt: now,
					ModelRuns: map[string]time.Time{
						string(types.ForecastMediumRange): mrRun.RunTimestamp,
					},
					Metadata: ResponseMetadata{
						Status: "fresh",
					},
					Data: data,
				}
			}
			mu.Unlock()

			return nil
		})
	}

	// Wait for all goroutines to finish.
	if err := g.Wait(); err != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeInternalUnexpected,
			Message: fmt.Sprintf("batch forecast error: %v", err),
			Err:     err,
		}
	}

	result := &BatchForecastResult{
		Forecasts: forecasts,
	}
	if len(errorMap) > 0 {
		result.Errors = errorMap
	}

	return result, nil
}

// GetVariables returns the list of supported forecast variables.
// Per FQRY-003 flow simulation.
func (s *forecastService) GetVariables(_ context.Context) ([]VariableResponseMetadata, error) {
	allModels := []types.ForecastType{types.ForecastMediumRange, types.ForecastNowcast}

	result := make([]VariableResponseMetadata, 0, len(types.StandardVariables))
	for _, meta := range types.StandardVariables {
		result = append(result, VariableResponseMetadata{
			VariableMetadata: meta,
			Name:             meta.Description,
			SupportedModels:  allModels,
		})
	}

	return result, nil
}

// GetStatus returns the health status of the forecast pipeline.
// Per FQRY-004 flow simulation:
//   - Medium-Range: <8h (Healthy), 8-12h (Degraded), >12h (Stale).
//   - Nowcast: <45m (Healthy), 45-90m (Degraded), >90m (Stale).
func (s *forecastService) GetStatus(ctx context.Context) (*SystemStatus, error) {
	now := s.clock.Now()

	models := map[string]ModelStatus{}

	// Medium-Range status.
	mrRun, err := s.runRepo.GetLatestServing(ctx, types.ForecastMediumRange)
	if err != nil {
		return nil, err
	}
	if mrRun != nil {
		age := now.Sub(mrRun.RunTimestamp)
		var status StatusEnum
		switch {
		case age <= MediumRangeHealthyAge:
			status = StatusHealthy
		case age <= MediumRangeDegradedAge:
			status = StatusDegraded
		default:
			status = StatusStale
		}
		models["medium_range"] = ModelStatus{
			Status:    status,
			LatestRun: mrRun.RunTimestamp,
			Horizon:   "240h",
			Coverage:  "Global",
		}
	}

	// Nowcast status.
	ncRun, err := s.runRepo.GetLatestServing(ctx, types.ForecastNowcast)
	if err != nil {
		return nil, err
	}
	if ncRun != nil {
		age := now.Sub(ncRun.RunTimestamp)
		var status StatusEnum
		switch {
		case age <= NowcastHealthyAge:
			status = StatusHealthy
		case age <= NowcastDegradedAge:
			status = StatusDegraded
		default:
			status = StatusStale
		}
		models["nowcast"] = ModelStatus{
			Status:    status,
			LatestRun: ncRun.RunTimestamp,
			Horizon:   "6h",
			Coverage:  "CONUS",
		}
	}

	return &SystemStatus{
		Timestamp: now,
		Models:    models,
	}, nil
}

// GetSnapshot retrieves a simplified forecast snapshot for WatchPoint creation context.
// Per FQRY-005 flow simulation and architecture/05c-api-forecasts.md Section 3:
// GetSnapshot MUST strictly query the Medium-Range (Atlas) model to ensure consistent
// global coverage and variable availability.
func (s *forecastService) GetSnapshot(ctx context.Context, lat, lon float64) (*types.ForecastSnapshot, error) {
	if err := validateLocation(lat, lon); err != nil {
		return nil, err
	}

	mrRun, err := s.runRepo.GetLatestServing(ctx, types.ForecastMediumRange)
	if err != nil {
		return nil, err
	}
	if mrRun == nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: "no medium-range forecast data available for snapshot",
		}
	}

	rc := RunContext{
		Model:       types.ForecastMediumRange,
		Timestamp:   mrRun.RunTimestamp,
		StoragePath: mrRun.StoragePath,
	}

	loc := types.Location{Lat: lat, Lon: lon}
	vars := []string{
		types.ZarrVarTemperatureC,
		types.ZarrVarPrecipitationMM,
		types.ZarrVarPrecipitationProb,
		types.ZarrVarWindSpeedKmh,
		types.ZarrVarHumidityPercent,
	}

	points, err := s.reader.ReadPoint(ctx, rc, loc, vars)
	if err != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: fmt.Sprintf("failed to read forecast snapshot: %v", err),
			Err:     err,
		}
	}

	// Return the first time step as the snapshot.
	if len(points) == 0 {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: "no forecast data points returned for snapshot",
		}
	}

	snapshot := &types.ForecastSnapshot{}
	pt := points[0]
	if pt.TemperatureC != nil {
		snapshot.TemperatureC = *pt.TemperatureC
	}
	if pt.PrecipitationMM != nil {
		snapshot.PrecipitationMM = *pt.PrecipitationMM
	}
	if pt.PrecipitationProbability != nil {
		snapshot.PrecipitationProb = *pt.PrecipitationProbability
	}
	if pt.WindSpeedKmh != nil {
		snapshot.WindSpeedKmh = *pt.WindSpeedKmh
	}
	if pt.Humidity != nil {
		snapshot.Humidity = *pt.Humidity
	}

	return snapshot, nil
}

// GetVerificationMetrics returns aggregated verification metrics for dashboard display.
// Per INFO-007 flow and architecture/05c-api-forecasts.md Section 3.
func (s *forecastService) GetVerificationMetrics(ctx context.Context, model string, start, end time.Time) (*VerificationReport, error) {
	if s.verificationRepo == nil {
		return &VerificationReport{
			Model:     model,
			StartTime: start,
			EndTime:   end,
			Metrics:   []types.VerificationMetric{},
		}, nil
	}

	metrics, err := s.verificationRepo.GetAggregatedMetrics(ctx, model, start, end)
	if err != nil {
		return nil, err
	}

	return &VerificationReport{
		Model:     model,
		StartTime: start,
		EndTime:   end,
		Metrics:   metrics,
	}, nil
}
