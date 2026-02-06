package db

import (
	"context"
	"time"

	"watchpoint/internal/types"
)

// ============================================================
// JobLockRepository
// ============================================================

// JobLockRepository provides distributed locking via the job_locks table.
// It implements the JobLockRepository interface defined in 09-scheduled-jobs.md
// Section 4.1. The locking mechanism uses INSERT ... ON CONFLICT DO UPDATE
// to atomically acquire a lock, ensuring only one Lambda execution processes
// a given job within a time window.
type JobLockRepository struct {
	db DBTX
}

// NewJobLockRepository creates a new JobLockRepository backed by the given
// database connection (pool or transaction).
func NewJobLockRepository(db DBTX) *JobLockRepository {
	return &JobLockRepository{db: db}
}

// Acquire attempts to insert a lock row. Returns true if acquired, false if
// the lock already exists and has not expired. The lockID is typically
// "task_type:timestamp_hour" (e.g., "archive_watchpoints:2026-02-06T03").
//
// SQL pattern:
//
//	INSERT INTO job_locks (id, worker_id, locked_at, expires_at)
//	VALUES ($1, $2, $3, $4)
//	ON CONFLICT (id) DO UPDATE
//	  SET worker_id = EXCLUDED.worker_id,
//	      locked_at = EXCLUDED.locked_at,
//	      expires_at = EXCLUDED.expires_at
//	  WHERE job_locks.expires_at < $3
//
// The locked_at ($3) and expires_at ($4) are computed as time.Time values in Go
// to avoid PostgreSQL interval parsing incompatibilities with Go's duration format.
//
// If the existing row has expired (expires_at < current time), the UPDATE succeeds
// and the caller acquires the lock. If the row is still active, the ON CONFLICT
// WHERE clause prevents the update, and zero rows are affected.
func (r *JobLockRepository) Acquire(ctx context.Context, lockID string, workerID string, ttl time.Duration) (bool, error) {
	// Compute expires_at as a concrete timestamp rather than using interval
	// arithmetic in SQL. This avoids PostgreSQL interval parsing issues with
	// Go's duration string format (e.g., "15m0s" is not valid PG interval).
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)

	tag, err := r.db.Exec(ctx,
		`INSERT INTO job_locks (id, worker_id, locked_at, expires_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (id) DO UPDATE
		   SET worker_id = EXCLUDED.worker_id,
		       locked_at = EXCLUDED.locked_at,
		       expires_at = EXCLUDED.expires_at
		   WHERE job_locks.expires_at < $3`,
		lockID,
		workerID,
		now,
		expiresAt,
	)
	if err != nil {
		return false, types.NewAppError(types.ErrCodeInternalDB, "failed to acquire job lock", err)
	}

	// RowsAffected is 1 if the INSERT succeeded (new row) or if the
	// ON CONFLICT UPDATE matched (expired lock reclaimed). It is 0 if
	// the lock exists and has not expired (another worker holds it).
	return tag.RowsAffected() > 0, nil
}

// ============================================================
// JobHistoryRepository
// ============================================================

// JobHistoryRepository provides data access for the job_history table.
// It implements the JobHistoryRepository interface defined in 09-scheduled-jobs.md
// Section 4.2. Job history entries track the execution of scheduled tasks for
// operational visibility and debugging.
type JobHistoryRepository struct {
	db DBTX
}

// NewJobHistoryRepository creates a new JobHistoryRepository backed by the
// given database connection (pool or transaction).
func NewJobHistoryRepository(db DBTX) *JobHistoryRepository {
	return &JobHistoryRepository{db: db}
}

// Start inserts a new job_history row with status 'running' and returns
// the auto-generated BIGSERIAL ID. The caller uses this ID to later call
// Finish with the outcome.
func (r *JobHistoryRepository) Start(ctx context.Context, jobType string) (int64, error) {
	var id int64
	err := r.db.QueryRow(ctx,
		`INSERT INTO job_history (job_type, started_at, status)
		 VALUES ($1, NOW(), 'running')
		 RETURNING id`,
		jobType,
	).Scan(&id)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to start job history entry", err)
	}
	return id, nil
}

// Finish updates the job_history row with the final status, item count,
// and optional error message. The status should be 'success' or 'failed'.
// If jobErr is non-nil, its message is stored in the error column.
func (r *JobHistoryRepository) Finish(ctx context.Context, id int64, status string, items int, jobErr error) error {
	var errMsg *string
	if jobErr != nil {
		s := jobErr.Error()
		errMsg = &s
	}

	tag, err := r.db.Exec(ctx,
		`UPDATE job_history
		 SET finished_at = NOW(), status = $2, items_count = $3, error = $4
		 WHERE id = $1`,
		id,
		status,
		items,
		errMsg,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to finish job history entry", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeInternalUnexpected, "job history entry not found", nil)
	}
	return nil
}

// ============================================================
// CalibrationRepository
// ============================================================

// CalibrationRepository provides data access for the calibration_coefficients
// and calibration_candidates tables. It implements the calibration data access
// patterns defined in 09-scheduled-jobs.md Section 7.5 and the schema in
// 02-foundation-db.md Section 6.
//
// The CalibrationRepository serves two primary flows:
//   - FCST-003 (Data Poller): GetAll returns the full calibration map for Nowcast
//     inference payload construction.
//   - OBS-006 (Calibration Update): UpdateCoefficients applies safe coefficient
//     updates, while CreateCandidate stores unsafe changes for manual review.
type CalibrationRepository struct {
	db DBTX
}

// NewCalibrationRepository creates a new CalibrationRepository backed by the
// given database connection (pool or transaction).
func NewCalibrationRepository(db DBTX) *CalibrationRepository {
	return &CalibrationRepository{db: db}
}

// GetAll returns all calibration coefficients as a map keyed by location_id.
// This is used by the Data Poller (FCST-003) to inject calibration data into
// the Nowcast InferencePayload.
//
// SQL: SELECT * FROM calibration_coefficients
//
// Returns an empty map (not nil) when no coefficients exist.
func (r *CalibrationRepository) GetAll(ctx context.Context) (map[string]types.CalibrationCoefficients, error) {
	rows, err := r.db.Query(ctx,
		`SELECT location_id, high_threshold, high_slope, high_base,
		        mid_threshold, mid_slope, mid_base, low_base, updated_at
		 FROM calibration_coefficients`,
	)
	if err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to query calibration coefficients", err)
	}
	defer rows.Close()

	result := make(map[string]types.CalibrationCoefficients)
	for rows.Next() {
		var c types.CalibrationCoefficients
		if err := rows.Scan(
			&c.LocationID,
			&c.HighThreshold,
			&c.HighSlope,
			&c.HighBase,
			&c.MidThreshold,
			&c.MidSlope,
			&c.MidBase,
			&c.LowBase,
			&c.UpdatedAt,
		); err != nil {
			return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to scan calibration coefficient", err)
		}
		result[c.LocationID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "error iterating calibration coefficients", err)
	}

	return result, nil
}

// UpdateCoefficients upserts a calibration coefficient row. This is used by
// the CalibrationService (OBS-006) when the proposed coefficients are within
// safe bounds (< 15% delta). Uses INSERT ... ON CONFLICT DO UPDATE to handle
// both new locations and updates to existing ones atomically.
func (r *CalibrationRepository) UpdateCoefficients(ctx context.Context, c *types.CalibrationCoefficients) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO calibration_coefficients
		 (location_id, high_threshold, high_slope, high_base,
		  mid_threshold, mid_slope, mid_base, low_base, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		 ON CONFLICT (location_id) DO UPDATE SET
		   high_threshold = EXCLUDED.high_threshold,
		   high_slope = EXCLUDED.high_slope,
		   high_base = EXCLUDED.high_base,
		   mid_threshold = EXCLUDED.mid_threshold,
		   mid_slope = EXCLUDED.mid_slope,
		   mid_base = EXCLUDED.mid_base,
		   low_base = EXCLUDED.low_base,
		   updated_at = EXCLUDED.updated_at`,
		c.LocationID,
		c.HighThreshold,
		c.HighSlope,
		c.HighBase,
		c.MidThreshold,
		c.MidSlope,
		c.MidBase,
		c.LowBase,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update calibration coefficients", err)
	}
	return nil
}

// CreateCandidate inserts a calibration candidate for manual review. This is
// used by the CalibrationService (OBS-006) when proposed coefficient changes
// exceed safe bounds (>= 15% delta). The candidate is stored with a
// violation_reason and status='pending' for Ops review.
//
// The returned int64 is the auto-generated BIGSERIAL ID.
func (r *CalibrationRepository) CreateCandidate(ctx context.Context, locationID string, coefficients []byte, violationReason string) (int64, error) {
	var id int64
	err := r.db.QueryRow(ctx,
		`INSERT INTO calibration_candidates
		 (location_id, proposed_coefficients, violation_reason, status, generated_at)
		 VALUES ($1, $2, $3, 'pending', NOW())
		 RETURNING id`,
		locationID,
		coefficients,
		violationReason,
	).Scan(&id)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to create calibration candidate", err)
	}
	return id, nil
}

// ============================================================
// VerificationRepository
// ============================================================

// VerificationRepository provides data access for the verification_results table.
// It implements the VerificationRepository interface defined in 02-foundation-db.md
// Section 9.2. Verification results store forecast accuracy metrics computed by
// the RunPod verification worker (OBS-005).
type VerificationRepository struct {
	db DBTX
}

// NewVerificationRepository creates a new VerificationRepository backed by
// the given database connection (pool or transaction).
func NewVerificationRepository(db DBTX) *VerificationRepository {
	return &VerificationRepository{db: db}
}

// Save performs a bulk insert of verification results. This is called after
// the RunPod verification worker completes and results are read from S3
// (OBS-005 step 12). Each result is inserted individually within the same
// connection context; callers should wrap in a transaction for atomicity
// if needed.
func (r *VerificationRepository) Save(ctx context.Context, results []types.VerificationResult) error {
	if len(results) == 0 {
		return nil
	}

	for i := range results {
		vr := &results[i]
		_, err := r.db.Exec(ctx,
			`INSERT INTO verification_results
			 (forecast_run_id, location_id, metric_type, variable, value, computed_at)
			 VALUES ($1, $2, $3, $4, $5, COALESCE($6, NOW()))`,
			vr.ForecastRunID,
			vr.LocationID,
			vr.MetricType,
			vr.Variable,
			vr.Value,
			nilIfZeroTime(vr.ComputedAt),
		)
		if err != nil {
			return types.NewAppError(types.ErrCodeInternalDB, "failed to save verification result", err)
		}
	}
	return nil
}

// GetAggregatedMetrics performs SQL-level aggregation (AVG, GROUP BY) on the
// verification_results table to produce metrics for the dashboard. Returns
// aggregated metrics grouped by variable and metric_type for the specified
// model and time range.
//
// The query joins verification_results with forecast_runs to filter by model
// type, then aggregates the values using AVG and groups by variable and
// metric_type.
//
// SQL:
//
//	SELECT fr.model, vr.variable, vr.metric_type, AVG(vr.value), MAX(vr.computed_at)
//	FROM verification_results vr
//	JOIN forecast_runs fr ON fr.id = vr.forecast_run_id
//	WHERE fr.model = $1 AND vr.computed_at BETWEEN $2 AND $3
//	GROUP BY fr.model, vr.variable, vr.metric_type
func (r *VerificationRepository) GetAggregatedMetrics(ctx context.Context, model types.ForecastType, start, end time.Time) ([]types.VerificationMetric, error) {
	rows, err := r.db.Query(ctx,
		`SELECT fr.model, vr.variable, vr.metric_type, AVG(vr.value), MAX(vr.computed_at)
		 FROM verification_results vr
		 JOIN forecast_runs fr ON fr.id = vr.forecast_run_id
		 WHERE fr.model = $1 AND vr.computed_at BETWEEN $2 AND $3
		 GROUP BY fr.model, vr.variable, vr.metric_type`,
		string(model),
		start,
		end,
	)
	if err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to query aggregated verification metrics", err)
	}
	defer rows.Close()

	var metrics []types.VerificationMetric
	for rows.Next() {
		var (
			m         types.VerificationMetric
			modelStr  string
		)
		if err := rows.Scan(
			&modelStr,
			&m.Variable,
			&m.MetricType,
			&m.Value,
			&m.Timestamp,
		); err != nil {
			return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to scan verification metric", err)
		}
		m.Model = types.ForecastType(modelStr)
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "error iterating verification metrics", err)
	}

	return metrics, nil
}
