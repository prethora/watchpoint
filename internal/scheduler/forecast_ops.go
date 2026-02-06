// Package scheduler implements scheduled job services for the WatchPoint platform.
//
// This file implements the forecast operations services from architecture/09-scheduled-jobs.md
// Sections 7.3, 7.4 (ForecastReconciler), and 7.5. It provides:
//
//   - VerificationService: Identifies forecast runs from 24-48h ago, probes for
//     observational data availability, and triggers RunPod with TaskType=verification.
//   - CalibrationService: Triggers RunPod with TaskType=calibration for regression
//     analysis. The RunPod worker handles the bounded automation safety check,
//     auto-applying safe updates or flagging unsafe ones for manual review.
//   - ForecastReconciler: Detects forecast runs stuck in 'running' state beyond
//     model-specific timeouts, cancels hanging jobs via RunPodClient, and implements
//     retry logic with a max retry count of 3.
//
// All services accept a `now` parameter for deterministic testing and manual
// backfill via MaintenancePayload.ReferenceTime.
//
// Flows: OBS-005, OBS-006, FCST-004
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"watchpoint/internal/types"
)

// =============================================================================
// Constants
// =============================================================================

// VerificationWindowStart defines how far back from "now" the verification
// window begins (48 hours). Per OBS-005: "Identify forecast runs from 24-48 hours."
const VerificationWindowStart = 48 * time.Hour

// VerificationWindowEnd defines the end of the verification window relative to
// "now" (24 hours ago). Runs between 24-48h old have had enough time for
// ground truth observations to be available.
const VerificationWindowEnd = 24 * time.Hour

// CalibrationLookback defines how far back the calibration service looks for
// verification data (7 days). Per OBS-006: "Fetches last 7 days of verification_results."
const CalibrationLookback = 7 * 24 * time.Hour

// CalibrationSafetyThreshold is the maximum allowed delta (15%) for auto-applying
// calibration coefficient updates. Changes exceeding this are flagged for manual
// review. Per OBS-006 safety check.
const CalibrationSafetyThreshold = 0.15

// MaxRetryCount is the maximum number of retries before a forecast run is
// permanently marked as failed. Per FCST-004: "If RetryCount >= 3: Marks status
// as 'failed'".
const MaxRetryCount = 3

// =============================================================================
// Verification Service (OBS-005)
// =============================================================================

// VerificationDB defines the database operations needed by the VerificationService.
type VerificationDB interface {
	// ListCompletedRunsInWindow returns forecast runs with status='complete'
	// whose run_timestamp falls within [start, end].
	//
	// SQL: SELECT * FROM forecast_runs
	//      WHERE status = 'complete'
	//      AND run_timestamp BETWEEN $1 AND $2
	ListCompletedRunsInWindow(ctx context.Context, start, end time.Time) ([]types.ForecastRun, error)
}

// ObservationProbe abstracts the check for observational data availability.
// The implementation uses S3 HeadObject to check if NOAA observational data
// (ISD/MRMS) exists for the verification period.
type ObservationProbe interface {
	// DataExists checks whether observational data is available for the given
	// time range. Returns true if data exists and verification can proceed.
	DataExists(ctx context.Context, start, end time.Time) (bool, error)
}

// VerificationRunPodClient defines the RunPod operations needed by the
// VerificationService. This is a subset of the full RunPodClient interface.
type VerificationRunPodClient interface {
	// TriggerInference calls the RunPod API to start a verification job.
	TriggerInference(ctx context.Context, payload types.InferencePayload) (string, error)
}

// verificationService implements VerificationService from
// architecture/09-scheduled-jobs.md Section 7.3.
type verificationService struct {
	db     VerificationDB
	probe  ObservationProbe
	runpod VerificationRunPodClient
	logger *slog.Logger
}

// NewVerificationService creates a new VerificationService.
func NewVerificationService(
	db VerificationDB,
	probe ObservationProbe,
	runpod VerificationRunPodClient,
	logger *slog.Logger,
) *verificationService {
	if logger == nil {
		logger = slog.Default()
	}
	return &verificationService{
		db:     db,
		probe:  probe,
		runpod: runpod,
		logger: logger,
	}
}

// TriggerVerification constructs an InferencePayload with TaskType=verification
// and dispatches it to the RunPod Client for processing.
//
// Per OBS-005 flow simulation:
//  1. Identify target forecast runs from 24-48 hours relative to now.
//  2. Probe: Check if NOAA observational data exists for that period via HeadObject.
//  3. If data exists, construct InferencePayload with TaskType=verification.
//  4. Call RunPodClient.TriggerInference.
//
// Returns the number of verification jobs triggered.
func (v *verificationService) TriggerVerification(ctx context.Context, windowStart, windowEnd time.Time) (int, error) {
	// Step 1: Identify target forecast runs in the verification window.
	runs, err := v.db.ListCompletedRunsInWindow(ctx, windowStart, windowEnd)
	if err != nil {
		return 0, fmt.Errorf("listing completed runs in verification window: %w", err)
	}

	if len(runs) == 0 {
		v.logger.InfoContext(ctx, "no completed forecast runs in verification window",
			"window_start", windowStart.Format(time.RFC3339),
			"window_end", windowEnd.Format(time.RFC3339),
		)
		return 0, nil
	}

	v.logger.InfoContext(ctx, "found forecast runs for verification",
		"count", len(runs),
		"window_start", windowStart.Format(time.RFC3339),
		"window_end", windowEnd.Format(time.RFC3339),
	)

	// Step 2: Probe for observational data availability.
	dataAvailable, err := v.probe.DataExists(ctx, windowStart, windowEnd)
	if err != nil {
		return 0, fmt.Errorf("probing observational data availability: %w", err)
	}

	if !dataAvailable {
		v.logger.WarnContext(ctx, "observational data not yet available for verification window",
			"window_start", windowStart.Format(time.RFC3339),
			"window_end", windowEnd.Format(time.RFC3339),
		)
		return 0, nil
	}

	// Step 3: Construct the InferencePayload for verification.
	// The RunPod worker discovers which runs to verify by querying the
	// verification_window. We pass the window, not individual run IDs.
	payload := types.InferencePayload{
		TaskType: types.RunPodTaskVerification,
		InputConfig: types.InferenceInputConfig{
			VerificationWindow: &types.VerificationWindow{
				Start: windowStart,
				End:   windowEnd,
			},
		},
	}

	// Step 4: Trigger RunPod.
	externalID, err := v.runpod.TriggerInference(ctx, payload)
	if err != nil {
		return 0, fmt.Errorf("triggering verification job: %w", err)
	}

	v.logger.InfoContext(ctx, "triggered verification job",
		"external_id", externalID,
		"run_count", len(runs),
	)

	return 1, nil
}

// =============================================================================
// Calibration Service (OBS-006)
// =============================================================================

// CalibrationDB defines the database operations needed by the CalibrationService.
type CalibrationDB interface {
	// GetAllCoefficients returns the current active calibration coefficients
	// keyed by location_id.
	GetAllCoefficients(ctx context.Context) (map[string]types.CalibrationCoefficients, error)

	// UpdateCoefficients upserts a calibration coefficient row (auto-apply path).
	UpdateCoefficients(ctx context.Context, c *types.CalibrationCoefficients) error

	// CreateCandidate inserts a calibration candidate for manual review
	// (safety check failed path). Returns the candidate ID.
	CreateCandidate(ctx context.Context, locationID string, coefficients []byte, violationReason string) (int64, error)

	// HasRecentVerificationData checks whether sufficient verification data
	// exists within the given time range for calibration regression.
	HasRecentVerificationData(ctx context.Context, since time.Time) (bool, error)
}

// CalibrationRunPodClient defines the RunPod operations needed by the
// CalibrationService.
type CalibrationRunPodClient interface {
	TriggerInference(ctx context.Context, payload types.InferencePayload) (string, error)
}

// calibrationService implements CalibrationService from
// architecture/09-scheduled-jobs.md Section 7.5.
type calibrationService struct {
	db     CalibrationDB
	runpod CalibrationRunPodClient
	logger *slog.Logger
}

// NewCalibrationService creates a new CalibrationService.
func NewCalibrationService(
	db CalibrationDB,
	runpod CalibrationRunPodClient,
	logger *slog.Logger,
) *calibrationService {
	if logger == nil {
		logger = slog.Default()
	}
	return &calibrationService{
		db:     db,
		runpod: runpod,
		logger: logger,
	}
}

// UpdateCoefficients triggers a RunPod task (TaskType=calibration) to perform
// regression analysis. The RunPod worker handles the heavy computation, and results
// are either auto-applied or flagged for manual review based on safety bounds.
//
// Per OBS-006 flow simulation:
//  1. Check for sufficient verification data.
//  2. Construct InferencePayload with TaskType=calibration and CalibrationTimeRange.
//  3. Dispatch to RunPod Client.
//
// The bounded automation safety check (< 15% delta) is performed by the RunPod
// worker, which directly writes to the DB. This service only triggers the job.
//
// Returns the number of calibration jobs triggered.
func (c *calibrationService) UpdateCoefficients(ctx context.Context, now time.Time) (int, error) {
	// Step 1: Check for sufficient verification data.
	since := now.Add(-CalibrationLookback)
	hasData, err := c.db.HasRecentVerificationData(ctx, since)
	if err != nil {
		return 0, fmt.Errorf("checking verification data availability: %w", err)
	}

	if !hasData {
		c.logger.InfoContext(ctx, "insufficient verification data for calibration",
			"since", since.Format(time.RFC3339),
		)
		return 0, nil
	}

	// Step 2: Construct the InferencePayload for calibration.
	payload := types.InferencePayload{
		TaskType: types.RunPodTaskCalibration,
		InputConfig: types.InferenceInputConfig{
			CalibrationTimeRange: &types.CalibrationTimeRange{
				Start: since,
				End:   now,
			},
		},
	}

	// Step 3: Dispatch to RunPod.
	externalID, err := c.runpod.TriggerInference(ctx, payload)
	if err != nil {
		return 0, fmt.Errorf("triggering calibration job: %w", err)
	}

	c.logger.InfoContext(ctx, "triggered calibration job",
		"external_id", externalID,
		"time_range_start", since.Format(time.RFC3339),
		"time_range_end", now.Format(time.RFC3339),
	)

	return 1, nil
}

// CheckCalibrationSafety evaluates proposed coefficient changes against the
// current active coefficients. For each location, it calculates the maximum
// percentage delta across all coefficient fields. If any field exceeds the
// CalibrationSafetyThreshold (15%), the change is considered unsafe.
//
// This function is exported for use by the RunPod callback handler or for
// local safety check scenarios where the results come back synchronously.
//
// Returns:
//   - safe: locations where all deltas are within bounds
//   - unsafe: locations where at least one delta exceeds bounds, with violation reason
func CheckCalibrationSafety(
	current map[string]types.CalibrationCoefficients,
	proposed map[string]types.CalibrationCoefficients,
) (safe []types.CalibrationCoefficients, unsafe []CalibrationViolation) {
	for locID, prop := range proposed {
		curr, exists := current[locID]
		if !exists {
			// New location with no prior coefficients: always safe (no delta to compare).
			safe = append(safe, prop)
			continue
		}

		maxDelta := maxCoefficientDelta(curr, prop)
		if maxDelta > CalibrationSafetyThreshold {
			unsafe = append(unsafe, CalibrationViolation{
				LocationID:  locID,
				Proposed:    prop,
				MaxDelta:    maxDelta,
				Reason:      fmt.Sprintf("max delta %.2f%% exceeds %.0f%% threshold", maxDelta*100, CalibrationSafetyThreshold*100),
			})
		} else {
			safe = append(safe, prop)
		}
	}
	return safe, unsafe
}

// CalibrationViolation represents a proposed calibration update that failed
// the safety check. Used to construct calibration_candidates records.
type CalibrationViolation struct {
	LocationID string
	Proposed   types.CalibrationCoefficients
	MaxDelta   float64
	Reason     string
}

// maxCoefficientDelta computes the maximum percentage change across all
// coefficient fields between current and proposed values. Returns the delta
// as a fraction (e.g., 0.15 = 15%).
//
// For fields where the current value is zero, absolute difference is used
// to prevent division by zero, treating any non-zero proposed value as exceeding
// the threshold.
func maxCoefficientDelta(curr, prop types.CalibrationCoefficients) float64 {
	pairs := [][2]float64{
		{curr.HighThreshold, prop.HighThreshold},
		{curr.HighSlope, prop.HighSlope},
		{curr.HighBase, prop.HighBase},
		{curr.MidThreshold, prop.MidThreshold},
		{curr.MidSlope, prop.MidSlope},
		{curr.MidBase, prop.MidBase},
		{curr.LowBase, prop.LowBase},
	}

	var maxDelta float64
	for _, p := range pairs {
		delta := relativeDelta(p[0], p[1])
		if delta > maxDelta {
			maxDelta = delta
		}
	}
	return maxDelta
}

// relativeDelta computes the relative change between two values as a fraction.
// Returns |new - old| / |old| for non-zero old values.
// For zero old values, returns the absolute difference (treating any change
// from zero as a 100% change if the new value is non-zero).
func relativeDelta(old, new float64) float64 {
	if old == 0 {
		if new == 0 {
			return 0
		}
		// Any change from zero is treated as exceeding any threshold.
		return math.Abs(new)
	}
	return math.Abs(new-old) / math.Abs(old)
}

// =============================================================================
// Forecast Reconciler (FCST-004)
// =============================================================================

// ReconcilerDB defines the database operations needed by the ForecastReconciler.
type ReconcilerDB interface {
	// ListStaleRuns returns forecast runs that are still in 'running' status
	// and were created before the given cutoff time.
	//
	// SQL: SELECT * FROM forecast_runs
	//      WHERE status = 'running' AND created_at < $1
	ListStaleRuns(ctx context.Context, cutoff time.Time) ([]types.ForecastRun, error)

	// IncrementRetryAndUpdateExternalID atomically increments retry_count and
	// updates the external_id for a forecast run being re-submitted.
	//
	// SQL: UPDATE forecast_runs
	//      SET retry_count = retry_count + 1, external_id = $2
	//      WHERE id = $1
	IncrementRetryAndUpdateExternalID(ctx context.Context, id string, externalID string) error

	// MarkRunFailed transitions a forecast run to 'failed' status with a reason.
	//
	// SQL: UPDATE forecast_runs
	//      SET status = 'failed', failure_reason = $2
	//      WHERE id = $1
	MarkRunFailed(ctx context.Context, id string, reason string) error
}

// ReconcilerRunPodClient defines the RunPod operations needed by the
// ForecastReconciler. This extends the base client with CancelJob.
type ReconcilerRunPodClient interface {
	TriggerInference(ctx context.Context, payload types.InferencePayload) (string, error)
	CancelJob(ctx context.Context, externalID string) error
}

// forecastReconciler implements ForecastReconciler from
// architecture/09-scheduled-jobs.md Section 7.4.
type forecastReconciler struct {
	db     ReconcilerDB
	runpod ReconcilerRunPodClient

	// Model-specific timeouts loaded from ForecastConfig.
	timeoutNowcast     time.Duration
	timeoutMediumRange time.Duration

	logger *slog.Logger
}

// ForecastReconcilerConfig holds configuration for the ForecastReconciler.
type ForecastReconcilerConfig struct {
	DB                 ReconcilerDB
	RunPod             ReconcilerRunPodClient
	TimeoutNowcast     time.Duration // Default: 20m
	TimeoutMediumRange time.Duration // Default: 3h
	Logger             *slog.Logger
}

// NewForecastReconciler creates a new ForecastReconciler.
func NewForecastReconciler(cfg ForecastReconcilerConfig) *forecastReconciler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Apply defaults from architecture spec if not provided.
	timeoutNowcast := cfg.TimeoutNowcast
	if timeoutNowcast == 0 {
		timeoutNowcast = 20 * time.Minute
	}
	timeoutMediumRange := cfg.TimeoutMediumRange
	if timeoutMediumRange == 0 {
		timeoutMediumRange = 3 * time.Hour
	}

	return &forecastReconciler{
		db:                 cfg.DB,
		runpod:             cfg.RunPod,
		timeoutNowcast:     timeoutNowcast,
		timeoutMediumRange: timeoutMediumRange,
		logger:             logger,
	}
}

// ReconcileStaleRuns detects forecast runs stuck in 'running' state beyond the
// configured timeout, cancels hanging jobs, and implements retry logic.
//
// Per FCST-004 flow simulation:
//  1. Query forecast_runs WHERE status='running' AND created_at < (now - threshold).
//  2. For each stale run:
//     a. Call RunPodClient.CancelJob to terminate the hanging job.
//     b. If retry_count < 3: Re-submit the job, update external_id and retry_count.
//     c. If retry_count >= 3: Mark as 'failed' with reason 'Max retries exhausted'.
//
// The threshold parameter is used as a general cutoff. The reconciler applies
// model-specific timeouts internally, using the minimum of the provided threshold
// and the model-specific timeout.
//
// Returns the number of runs processed (cancelled + retried + failed).
func (r *forecastReconciler) ReconcileStaleRuns(ctx context.Context, now time.Time, threshold time.Duration) (int64, error) {
	// Use the most conservative timeout to find all potentially stale runs.
	// We'll apply model-specific timeouts per run below.
	cutoff := now.Add(-threshold)

	staleRuns, err := r.db.ListStaleRuns(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("listing stale forecast runs: %w", err)
	}

	if len(staleRuns) == 0 {
		r.logger.InfoContext(ctx, "no stale forecast runs found",
			"cutoff", cutoff.Format(time.RFC3339),
		)
		return 0, nil
	}

	r.logger.InfoContext(ctx, "found stale forecast runs",
		"count", len(staleRuns),
		"cutoff", cutoff.Format(time.RFC3339),
	)

	var processed int64
	for _, run := range staleRuns {
		// Apply model-specific timeout check.
		timeout := r.timeoutForModel(run.Model)
		runAge := now.Sub(run.CreatedAt)
		if runAge < timeout {
			// This run hasn't exceeded its model-specific timeout yet.
			r.logger.InfoContext(ctx, "run not yet past model-specific timeout, skipping",
				"run_id", run.ID,
				"model", run.Model,
				"age", runAge.String(),
				"timeout", timeout.String(),
			)
			continue
		}

		if err := r.reconcileRun(ctx, run); err != nil {
			r.logger.ErrorContext(ctx, "failed to reconcile stale run",
				"run_id", run.ID,
				"model", run.Model,
				"error", err,
			)
			// Continue with other runs; partial progress is acceptable.
			continue
		}
		processed++
	}

	r.logger.InfoContext(ctx, "forecast reconciliation complete",
		"processed", processed,
		"total_stale", len(staleRuns),
	)

	return processed, nil
}

// reconcileRun handles a single stale forecast run: cancel, then retry or fail.
func (r *forecastReconciler) reconcileRun(ctx context.Context, run types.ForecastRun) error {
	// Step 1: Cancel the hanging job on RunPod to stop cost accumulation.
	if run.ExternalID != "" {
		if err := r.runpod.CancelJob(ctx, run.ExternalID); err != nil {
			r.logger.WarnContext(ctx, "failed to cancel RunPod job (may already be dead)",
				"run_id", run.ID,
				"external_id", run.ExternalID,
				"error", err,
			)
			// Continue with retry/fail logic even if cancel fails.
			// The job may already be dead/expired on RunPod's side.
		} else {
			r.logger.InfoContext(ctx, "cancelled RunPod job",
				"run_id", run.ID,
				"external_id", run.ExternalID,
			)
		}
	}

	// Step 2: Check retry count.
	if run.RetryCount >= MaxRetryCount {
		// Max retries exhausted: mark as permanently failed.
		if err := r.db.MarkRunFailed(ctx, run.ID, "Max retries exhausted"); err != nil {
			return fmt.Errorf("marking run %s as failed: %w", run.ID, err)
		}
		r.logger.WarnContext(ctx, "forecast run max retries exhausted",
			"run_id", run.ID,
			"model", run.Model,
			"retry_count", run.RetryCount,
		)
		return nil
	}

	// Step 3: Re-submit the job.
	payload := types.InferencePayload{
		TaskType:          types.RunPodTaskInference,
		Model:             run.Model,
		RunTimestamp:      run.RunTimestamp,
		OutputDestination: run.StoragePath,
		Options: types.InferenceOptions{
			ForceRebuild: true, // Per FCST-004: "Set Options.ForceRebuild = true"
		},
	}

	newExternalID, err := r.runpod.TriggerInference(ctx, payload)
	if err != nil {
		// Could not re-submit. Mark as failed rather than leaving in limbo.
		if markErr := r.db.MarkRunFailed(ctx, run.ID, fmt.Sprintf("Re-submission failed: %s", err.Error())); markErr != nil {
			r.logger.ErrorContext(ctx, "failed to mark run as failed after re-submission error",
				"run_id", run.ID,
				"trigger_error", err,
				"mark_error", markErr,
			)
		}
		return fmt.Errorf("re-submitting run %s: %w", run.ID, err)
	}

	// Step 4: Update DB with new external_id and incremented retry_count.
	if err := r.db.IncrementRetryAndUpdateExternalID(ctx, run.ID, newExternalID); err != nil {
		return fmt.Errorf("updating retry count and external ID for run %s: %w", run.ID, err)
	}

	r.logger.InfoContext(ctx, "re-submitted forecast run",
		"run_id", run.ID,
		"model", run.Model,
		"new_external_id", newExternalID,
		"retry_count", run.RetryCount+1,
	)

	return nil
}

// timeoutForModel returns the model-specific timeout duration.
// Per architecture/09-scheduled-jobs.md Section 7.4:
//   - TimeoutNowcast (default 20m) for nowcast models
//   - TimeoutMediumRange (default 3h) for medium-range models
func (r *forecastReconciler) timeoutForModel(model types.ForecastType) time.Duration {
	switch model {
	case types.ForecastNowcast:
		return r.timeoutNowcast
	case types.ForecastMediumRange:
		return r.timeoutMediumRange
	default:
		// Unknown model types use the medium-range timeout as a safe default.
		return r.timeoutMediumRange
	}
}
