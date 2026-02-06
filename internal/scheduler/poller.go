// Package scheduler implements scheduled job services for the WatchPoint platform.
//
// This file implements the DataPoller service from architecture/09-scheduled-jobs.md
// Section 5. The poller detects new data in upstream NOAA S3 buckets and triggers
// the internal forecast pipeline via RunPod. It does NOT download the data itself;
// it passes references (timestamps and S3 paths) to RunPod for processing.
//
// Key behaviors:
//   - Loops through configured forecast models (medium_range, nowcast).
//   - For Nowcast: requires both GOES satellite and MRMS radar data to be available
//     within a 5-minute temporal tolerance window (FAIL-005 synchronization).
//   - Respects a configurable Limit to prevent Lambda timeouts during backfills.
//   - Injects calibration coefficients into Nowcast inference payloads.
//
// Flows: FCST-003, FAIL-005
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"watchpoint/internal/types"
)

// NowcastSyncTolerance is the maximum time difference between GOES and MRMS
// data timestamps that is considered "aligned" for Nowcast inference.
// Per architecture/09-scheduled-jobs.md Section 5.2 step 3 and flow FAIL-005.
const NowcastSyncTolerance = 5 * time.Minute

// DataPollerInput defines the input for manual Lambda invocation.
// Used for disaster recovery and forcing retries.
// Per architecture/09-scheduled-jobs.md Section 5 Input Payload.
type DataPollerInput struct {
	ForceRetry    bool `json:"force_retry"`
	BackfillHours int  `json:"backfill_hours"`
	Limit         int  `json:"limit"` // Limits RunPod triggers per execution to prevent timeout
}

// ForecastRunRepo abstracts the database operations the poller needs from
// the ForecastRunRepository. Using an interface allows clean testing without
// database dependencies.
type ForecastRunRepo interface {
	// GetLatest returns the most recent forecast run for a model (any status).
	GetLatest(ctx context.Context, model types.ForecastType) (*types.ForecastRun, error)
	// Create inserts a new forecast run record.
	Create(ctx context.Context, run *types.ForecastRun) error
	// UpdateExternalID sets the RunPod job ID on a forecast run.
	UpdateExternalID(ctx context.Context, id string, externalID string) error
}

// CalibrationRepo abstracts the calibration coefficients read operation.
type CalibrationRepo interface {
	// GetAll returns all calibration coefficients keyed by location_id.
	GetAll(ctx context.Context) (map[string]types.CalibrationCoefficients, error)
}

// UpstreamSource defines a remote data source (NOAA GFS/GOES/MRMS).
// Per architecture/09-scheduled-jobs.md Section 5.1.
type UpstreamSource interface {
	// Name returns the forecast model type this source provides data for.
	Name() types.ForecastType
	// CheckAvailability returns timestamps of runs available in S3 since the given time.
	CheckAvailability(ctx context.Context, since time.Time) ([]time.Time, error)
}

// RunPodClient abstracts the RunPod inference trigger.
// Per architecture/09-scheduled-jobs.md Section 5.1.
type RunPodClient interface {
	// TriggerInference calls the RunPod API and returns the external Job ID.
	TriggerInference(ctx context.Context, payload types.InferencePayload) (string, error)
}

// DataPoller detects new upstream forecast data and triggers inference jobs.
// It is the core service behind the DataPollerFunction Lambda.
type DataPoller struct {
	forecastRepo    ForecastRunRepo
	calibrationRepo CalibrationRepo
	runpod          RunPodClient

	// Upstream sources
	gfsSource  UpstreamSource // Medium-range (GFS)
	goesSource UpstreamSource // Nowcast satellite (GOES)
	mrmsSource UpstreamSource // Nowcast radar (MRMS)

	// Configuration
	forecastBucket string // S3 bucket for output (e.g., "watchpoint-forecasts")
	logger         *slog.Logger
}

// DataPollerConfig holds the configuration for creating a DataPoller.
type DataPollerConfig struct {
	ForecastRepo    ForecastRunRepo
	CalibrationRepo CalibrationRepo
	RunPod          RunPodClient
	GFSSource       UpstreamSource
	GOESSource      UpstreamSource
	MRMSSource      UpstreamSource
	ForecastBucket  string
	Logger          *slog.Logger
}

// NewDataPoller creates a new DataPoller with the given configuration.
func NewDataPoller(cfg DataPollerConfig) *DataPoller {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &DataPoller{
		forecastRepo:    cfg.ForecastRepo,
		calibrationRepo: cfg.CalibrationRepo,
		runpod:          cfg.RunPod,
		gfsSource:       cfg.GFSSource,
		goesSource:      cfg.GOESSource,
		mrmsSource:      cfg.MRMSSource,
		forecastBucket:  cfg.ForecastBucket,
		logger:          logger,
	}
}

// Poll checks for new upstream data and triggers inference for each new run found.
// It returns the total number of inference jobs triggered.
//
// The method loops through two model types:
//   - medium_range (GFS): triggered independently when new GFS data is available.
//   - nowcast (GOES+MRMS): triggered only when both sources have temporally aligned
//     data within the NowcastSyncTolerance (5 minutes).
//
// If input.Limit > 0, the poller stops triggering new jobs once the limit is reached.
// If input.BackfillHours > 0, the "since" time is shifted further into the past.
//
// Flow: FCST-003
func (p *DataPoller) Poll(ctx context.Context, input DataPollerInput) (int, error) {
	totalTriggered := 0
	remaining := input.Limit // 0 means unlimited

	// --- Medium-Range (GFS) ---
	if p.gfsSource != nil {
		triggered, err := p.pollMediumRange(ctx, input, remaining)
		if err != nil {
			p.logger.ErrorContext(ctx, "medium-range polling failed",
				"error", err,
			)
			// Continue to nowcast even if medium-range fails.
			// Each model is independent.
		} else {
			totalTriggered += triggered
			if remaining > 0 {
				remaining -= triggered
				if remaining <= 0 {
					p.logger.InfoContext(ctx, "limit reached after medium-range polling",
						"total_triggered", totalTriggered,
						"limit", input.Limit,
					)
					return totalTriggered, nil
				}
			}
		}
	}

	// --- Nowcast (GOES + MRMS) ---
	if p.goesSource != nil && p.mrmsSource != nil {
		triggered, err := p.pollNowcast(ctx, input, remaining)
		if err != nil {
			p.logger.ErrorContext(ctx, "nowcast polling failed",
				"error", err,
			)
			// Don't return error for individual model failures;
			// the poller should be resilient.
		} else {
			totalTriggered += triggered
		}
	}

	p.logger.InfoContext(ctx, "poll cycle complete",
		"total_triggered", totalTriggered,
	)

	return totalTriggered, nil
}

// pollMediumRange handles the GFS medium-range model polling.
func (p *DataPoller) pollMediumRange(ctx context.Context, input DataPollerInput, limit int) (int, error) {
	since, err := p.getSinceTime(ctx, types.ForecastMediumRange, input)
	if err != nil {
		return 0, fmt.Errorf("getting since time for medium_range: %w", err)
	}

	p.logger.InfoContext(ctx, "checking medium-range upstream",
		"since", since.Format(time.RFC3339),
	)

	timestamps, err := p.gfsSource.CheckAvailability(ctx, since)
	if err != nil {
		return 0, fmt.Errorf("checking GFS availability: %w", err)
	}

	if len(timestamps) == 0 {
		p.logger.InfoContext(ctx, "no new medium-range data")
		return 0, nil
	}

	triggered := 0
	for _, ts := range timestamps {
		if limit > 0 && triggered >= limit {
			p.logger.InfoContext(ctx, "medium-range limit reached",
				"triggered", triggered,
				"limit", limit,
			)
			break
		}

		jobID, err := p.triggerInference(ctx, types.ForecastMediumRange, ts, nil)
		if err != nil {
			p.logger.ErrorContext(ctx, "failed to trigger medium-range inference",
				"run_timestamp", ts.Format(time.RFC3339),
				"error", err,
			)
			// Continue with next timestamp; don't abort all.
			continue
		}

		p.logger.InfoContext(ctx, "triggered medium-range inference",
			"run_timestamp", ts.Format(time.RFC3339),
			"job_id", jobID,
		)
		triggered++
	}

	return triggered, nil
}

// pollNowcast handles the Nowcast model polling with GOES/MRMS synchronization.
// Per architecture/09-scheduled-jobs.md Section 5.2 step 3 and flow FAIL-005.
func (p *DataPoller) pollNowcast(ctx context.Context, input DataPollerInput, limit int) (int, error) {
	since, err := p.getSinceTime(ctx, types.ForecastNowcast, input)
	if err != nil {
		return 0, fmt.Errorf("getting since time for nowcast: %w", err)
	}

	p.logger.InfoContext(ctx, "checking nowcast upstream sources",
		"since", since.Format(time.RFC3339),
	)

	// Fetch available timestamps from both sources concurrently would be ideal,
	// but for simplicity and correctness we do them sequentially.
	goesTimestamps, err := p.goesSource.CheckAvailability(ctx, since)
	if err != nil {
		return 0, fmt.Errorf("checking GOES availability: %w", err)
	}

	mrmsTimestamps, err := p.mrmsSource.CheckAvailability(ctx, since)
	if err != nil {
		return 0, fmt.Errorf("checking MRMS availability: %w", err)
	}

	// Compute the intersection of timestamps within the 5-minute tolerance.
	aligned := IntersectTimestamps(goesTimestamps, mrmsTimestamps, NowcastSyncTolerance)

	if len(aligned) == 0 {
		p.logger.InfoContext(ctx, "no aligned nowcast data",
			"goes_count", len(goesTimestamps),
			"mrms_count", len(mrmsTimestamps),
		)
		return 0, nil
	}

	p.logger.InfoContext(ctx, "found aligned nowcast timestamps",
		"aligned_count", len(aligned),
		"goes_count", len(goesTimestamps),
		"mrms_count", len(mrmsTimestamps),
	)

	// Fetch calibration map for Nowcast.
	calibration, err := p.calibrationRepo.GetAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("fetching calibration coefficients: %w", err)
	}

	triggered := 0
	for _, ts := range aligned {
		if limit > 0 && triggered >= limit {
			p.logger.InfoContext(ctx, "nowcast limit reached",
				"triggered", triggered,
				"limit", limit,
			)
			break
		}

		jobID, err := p.triggerInference(ctx, types.ForecastNowcast, ts, calibration)
		if err != nil {
			p.logger.ErrorContext(ctx, "failed to trigger nowcast inference",
				"run_timestamp", ts.Format(time.RFC3339),
				"error", err,
			)
			continue
		}

		p.logger.InfoContext(ctx, "triggered nowcast inference",
			"run_timestamp", ts.Format(time.RFC3339),
			"job_id", jobID,
		)
		triggered++
	}

	return triggered, nil
}

// triggerInference creates a ForecastRun record, constructs the InferencePayload,
// triggers RunPod, and updates the DB with the external job ID.
//
// Per FCST-003 flow simulation steps 7-10:
//  1. Insert into forecast_runs with status='running'.
//  2. Construct InferencePayload.
//  3. Call RunPodClient.TriggerInference.
//  4. Update forecast_runs with the external_id.
func (p *DataPoller) triggerInference(
	ctx context.Context,
	model types.ForecastType,
	runTimestamp time.Time,
	calibration map[string]types.CalibrationCoefficients,
) (string, error) {
	// Build the output destination path.
	outputDest := fmt.Sprintf("s3://%s/%s/%s/",
		p.forecastBucket,
		string(model),
		runTimestamp.Format(time.RFC3339),
	)

	// Step 1: Create the forecast_runs record with status='running'.
	run := &types.ForecastRun{
		Model:               model,
		RunTimestamp:        runTimestamp,
		SourceDataTimestamp: runTimestamp,
		StoragePath:         outputDest,
		Status:              "running",
	}

	if err := p.forecastRepo.Create(ctx, run); err != nil {
		return "", fmt.Errorf("creating forecast run record: %w", err)
	}

	// Step 2: Construct InferencePayload.
	payload := types.InferencePayload{
		TaskType:          types.RunPodTaskInference,
		Model:             model,
		RunTimestamp:      runTimestamp,
		OutputDestination: outputDest,
	}

	// Inject model-specific input config.
	switch model {
	case types.ForecastNowcast:
		// Inject calibration map for nowcast.
		payload.InputConfig.Calibration = calibration
	case types.ForecastMediumRange:
		// GFS source paths are derived from the timestamp by the RunPod worker.
		// The worker knows how to locate GFS data from the run_timestamp.
		// No additional source paths needed from the poller.
	}

	// Step 3: Trigger RunPod inference.
	externalID, err := p.runpod.TriggerInference(ctx, payload)
	if err != nil {
		// The forecast_runs record was already created with status='running'.
		// The reconciler (FCST-004) will detect this as a stale run and handle
		// cleanup. We don't need to delete the record here.
		return "", fmt.Errorf("triggering RunPod inference: %w", err)
	}

	// Step 4: Update forecast_runs with the external job ID.
	if err := p.forecastRepo.UpdateExternalID(ctx, run.ID, externalID); err != nil {
		// The RunPod job is already running. Log the error but return success
		// since the reconciler can handle runs without external IDs.
		p.logger.ErrorContext(ctx, "failed to update external ID",
			"run_id", run.ID,
			"external_id", externalID,
			"error", err,
		)
	}

	return externalID, nil
}

// getSinceTime determines the "since" timestamp for checking upstream availability.
// If BackfillHours > 0, we look further back in time.
// Otherwise, we use the latest run_timestamp from the database.
// If no runs exist, we default to 24 hours ago.
func (p *DataPoller) getSinceTime(ctx context.Context, model types.ForecastType, input DataPollerInput) (time.Time, error) {
	if input.BackfillHours > 0 {
		return time.Now().UTC().Add(-time.Duration(input.BackfillHours) * time.Hour), nil
	}

	latest, err := p.forecastRepo.GetLatest(ctx, model)
	if err != nil {
		return time.Time{}, fmt.Errorf("querying latest run for %s: %w", model, err)
	}

	if latest != nil {
		return latest.RunTimestamp, nil
	}

	// No previous runs; default to 24 hours ago.
	return time.Now().UTC().Add(-24 * time.Hour), nil
}

// IntersectTimestamps computes the temporal intersection of two sorted timestamp
// slices. For each timestamp in setA, it finds the closest timestamp in setB
// that is within the given tolerance. When a match is found, the earlier of the
// two timestamps is used as the aligned timestamp (representing the data
// availability point).
//
// Both input slices must be sorted in ascending chronological order.
// The returned slice is sorted in ascending order and contains no duplicates.
//
// This implements the Nowcast synchronization logic described in
// architecture/09-scheduled-jobs.md Section 5.2 step 3 and flow FAIL-005.
func IntersectTimestamps(setA, setB []time.Time, tolerance time.Duration) []time.Time {
	if len(setA) == 0 || len(setB) == 0 {
		return nil
	}

	// Track which setB timestamps have been matched to prevent double-matching.
	matchedB := make(map[int]bool)
	var result []time.Time
	seen := make(map[time.Time]struct{})

	for _, a := range setA {
		bestIdx := -1
		bestDiff := tolerance + 1 // Initialize to just above tolerance

		for j, b := range setB {
			if matchedB[j] {
				continue
			}
			diff := absDuration(a.Sub(b))
			if diff <= tolerance && diff < bestDiff {
				bestIdx = j
				bestDiff = diff
			}
		}

		if bestIdx >= 0 {
			matchedB[bestIdx] = true
			// Use the earlier timestamp as the aligned point.
			aligned := a
			if setB[bestIdx].Before(a) {
				aligned = setB[bestIdx]
			}
			if _, exists := seen[aligned]; !exists {
				seen[aligned] = struct{}{}
				result = append(result, aligned)
			}
		}
	}

	// Sort the result chronologically.
	sort.Slice(result, func(i, j int) bool {
		return result[i].Before(result[j])
	})

	return result
}

// absDuration returns the absolute value of a time.Duration.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return time.Duration(-int64(d))
	}
	return d
}

// NowcastDataLagThreshold is the duration after which a NowcastDataLag metric
// should be emitted if no intersection is found. Per FAIL-005 step 6.
const NowcastDataLagThreshold = 30 * time.Minute
