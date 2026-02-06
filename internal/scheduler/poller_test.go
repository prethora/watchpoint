package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// ============================================================
// Mock Implementations
// ============================================================

// mockForecastRunRepo is an in-memory mock of ForecastRunRepo.
type mockForecastRunRepo struct {
	runs        []*types.ForecastRun
	createErr   error
	updateErr   error
	getErr      error
	createCalls int
	updateCalls []string // external IDs passed to UpdateExternalID
	nextID      int
}

func newMockForecastRunRepo() *mockForecastRunRepo {
	return &mockForecastRunRepo{nextID: 1}
}

func (m *mockForecastRunRepo) GetLatest(_ context.Context, model types.ForecastType) (*types.ForecastRun, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	var latest *types.ForecastRun
	for _, r := range m.runs {
		if r.Model == model {
			if latest == nil || r.RunTimestamp.After(latest.RunTimestamp) {
				latest = r
			}
		}
	}
	return latest, nil
}

func (m *mockForecastRunRepo) Create(_ context.Context, run *types.ForecastRun) error {
	m.createCalls++
	if m.createErr != nil {
		return m.createErr
	}
	run.ID = fmt.Sprintf("run_%d", m.nextID)
	m.nextID++
	run.CreatedAt = time.Now().UTC()
	m.runs = append(m.runs, run)
	return nil
}

func (m *mockForecastRunRepo) UpdateExternalID(_ context.Context, id string, externalID string) error {
	m.updateCalls = append(m.updateCalls, externalID)
	if m.updateErr != nil {
		return m.updateErr
	}
	for _, r := range m.runs {
		if r.ID == id {
			r.ExternalID = externalID
			return nil
		}
	}
	return errors.New("run not found")
}

// mockCalibrationRepo is a mock for CalibrationRepo.
type mockCalibrationRepo struct {
	coefficients map[string]types.CalibrationCoefficients
	err          error
	calls        int
}

func newMockCalibrationRepo() *mockCalibrationRepo {
	return &mockCalibrationRepo{
		coefficients: map[string]types.CalibrationCoefficients{
			"tile_39_-77": {
				LocationID:    "tile_39_-77",
				HighThreshold: 50.0,
				HighSlope:     0.8,
				HighBase:      10.0,
				MidThreshold:  30.0,
				MidSlope:      0.5,
				MidBase:       5.0,
				LowBase:       2.0,
			},
		},
	}
}

func (m *mockCalibrationRepo) GetAll(_ context.Context) (map[string]types.CalibrationCoefficients, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.coefficients, nil
}

// mockUpstreamSource is a mock for UpstreamSource.
type mockUpstreamSource struct {
	name       types.ForecastType
	timestamps []time.Time
	err        error
	calls      int
}

func newMockUpstreamSource(name types.ForecastType, timestamps []time.Time) *mockUpstreamSource {
	return &mockUpstreamSource{
		name:       name,
		timestamps: timestamps,
	}
}

func (m *mockUpstreamSource) Name() types.ForecastType {
	return m.name
}

func (m *mockUpstreamSource) CheckAvailability(_ context.Context, _ time.Time) ([]time.Time, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.timestamps, nil
}

// mockRunPodClient is a mock for RunPodClient.
type mockRunPodClient struct {
	triggerCalls []types.InferencePayload
	jobIDs       []string
	err          error
	nextJobIdx   int
}

func newMockRunPodClient(jobIDs ...string) *mockRunPodClient {
	return &mockRunPodClient{jobIDs: jobIDs}
}

func (m *mockRunPodClient) TriggerInference(_ context.Context, payload types.InferencePayload) (string, error) {
	m.triggerCalls = append(m.triggerCalls, payload)
	if m.err != nil {
		return "", m.err
	}
	if m.nextJobIdx < len(m.jobIDs) {
		jobID := m.jobIDs[m.nextJobIdx]
		m.nextJobIdx++
		return jobID, nil
	}
	return fmt.Sprintf("job_%d", len(m.triggerCalls)), nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// ============================================================
// Test: IntersectTimestamps (Nowcast Synchronization Logic)
// ============================================================

func TestIntersectTimestamps_ExactMatch(t *testing.T) {
	base := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	setA := []time.Time{base, base.Add(15 * time.Minute), base.Add(30 * time.Minute)}
	setB := []time.Time{base, base.Add(15 * time.Minute), base.Add(30 * time.Minute)}

	result := IntersectTimestamps(setA, setB, NowcastSyncTolerance)

	if len(result) != 3 {
		t.Fatalf("expected 3 intersections, got %d", len(result))
	}
	for i, expected := range setA {
		if !result[i].Equal(expected) {
			t.Errorf("result[%d] = %v, want %v", i, result[i], expected)
		}
	}
}

func TestIntersectTimestamps_WithinTolerance(t *testing.T) {
	base := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	// GOES at exact time, MRMS 3 minutes later (within 5-min tolerance)
	goesTS := []time.Time{base, base.Add(15 * time.Minute)}
	mrmsTS := []time.Time{base.Add(3 * time.Minute), base.Add(18 * time.Minute)}

	result := IntersectTimestamps(goesTS, mrmsTS, NowcastSyncTolerance)

	if len(result) != 2 {
		t.Fatalf("expected 2 intersections, got %d", len(result))
	}
	// Each aligned timestamp should be the earlier of the pair.
	if !result[0].Equal(base) {
		t.Errorf("result[0] = %v, want %v (earlier of pair)", result[0], base)
	}
	if !result[1].Equal(base.Add(15 * time.Minute)) {
		t.Errorf("result[1] = %v, want %v (earlier of pair)", result[1], base.Add(15*time.Minute))
	}
}

func TestIntersectTimestamps_OutsideTolerance(t *testing.T) {
	base := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	// GOES at base, MRMS 6 minutes later (outside 5-min tolerance)
	goesTS := []time.Time{base}
	mrmsTS := []time.Time{base.Add(6 * time.Minute)}

	result := IntersectTimestamps(goesTS, mrmsTS, NowcastSyncTolerance)

	if len(result) != 0 {
		t.Fatalf("expected 0 intersections, got %d", len(result))
	}
}

func TestIntersectTimestamps_PartialMatch_FAIL005(t *testing.T) {
	// This test directly models the FAIL-005 flow simulation:
	// GoesTS: [T1, T2, T3], MrmsTS: [T1] (lagging)
	// Expected: Only T1 matches.
	T1 := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	T2 := T1.Add(15 * time.Minute)
	T3 := T1.Add(30 * time.Minute)

	goesTS := []time.Time{T1, T2, T3}
	mrmsTS := []time.Time{T1}

	result := IntersectTimestamps(goesTS, mrmsTS, NowcastSyncTolerance)

	if len(result) != 1 {
		t.Fatalf("expected 1 intersection, got %d", len(result))
	}
	if !result[0].Equal(T1) {
		t.Errorf("result[0] = %v, want %v", result[0], T1)
	}
}

func TestIntersectTimestamps_EmptySetA(t *testing.T) {
	base := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	result := IntersectTimestamps(nil, []time.Time{base}, NowcastSyncTolerance)
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestIntersectTimestamps_EmptySetB(t *testing.T) {
	base := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	result := IntersectTimestamps([]time.Time{base}, nil, NowcastSyncTolerance)
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestIntersectTimestamps_BothEmpty(t *testing.T) {
	result := IntersectTimestamps(nil, nil, NowcastSyncTolerance)
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestIntersectTimestamps_NoDuplicates(t *testing.T) {
	// When multiple setA timestamps could match the same setB timestamp,
	// only the best match should be used (no double-matching).
	base := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	// Two GOES timestamps 2 minutes apart, one MRMS timestamp between them.
	goesTS := []time.Time{base, base.Add(2 * time.Minute)}
	mrmsTS := []time.Time{base.Add(1 * time.Minute)}

	result := IntersectTimestamps(goesTS, mrmsTS, NowcastSyncTolerance)

	// Only one match should occur (the MRMS timestamp can only be matched once).
	if len(result) != 1 {
		t.Fatalf("expected 1 intersection (no double-matching), got %d", len(result))
	}
}

func TestIntersectTimestamps_ExactBoundary(t *testing.T) {
	base := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	// Exactly at the 5-minute tolerance boundary.
	goesTS := []time.Time{base}
	mrmsTS := []time.Time{base.Add(5 * time.Minute)}

	result := IntersectTimestamps(goesTS, mrmsTS, NowcastSyncTolerance)

	// Exactly at tolerance should match (<=, not <).
	if len(result) != 1 {
		t.Fatalf("expected 1 intersection at exact boundary, got %d", len(result))
	}
}

func TestIntersectTimestamps_ReversedOrder(t *testing.T) {
	// MRMS timestamps before GOES timestamps.
	base := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	goesTS := []time.Time{base.Add(3 * time.Minute)}
	mrmsTS := []time.Time{base}

	result := IntersectTimestamps(goesTS, mrmsTS, NowcastSyncTolerance)

	if len(result) != 1 {
		t.Fatalf("expected 1 intersection, got %d", len(result))
	}
	// The aligned timestamp should be the earlier one (MRMS in this case).
	if !result[0].Equal(base) {
		t.Errorf("result[0] = %v, want %v (earlier of pair)", result[0], base)
	}
}

func TestIntersectTimestamps_ResultSorted(t *testing.T) {
	base := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	goesTS := []time.Time{base.Add(30 * time.Minute), base, base.Add(15 * time.Minute)}
	mrmsTS := []time.Time{base.Add(1 * time.Minute), base.Add(31 * time.Minute), base.Add(16 * time.Minute)}

	result := IntersectTimestamps(goesTS, mrmsTS, NowcastSyncTolerance)

	if len(result) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(result))
	}

	for i := 1; i < len(result); i++ {
		if result[i].Before(result[i-1]) {
			t.Errorf("result not sorted: result[%d]=%v before result[%d]=%v",
				i, result[i], i-1, result[i-1])
		}
	}
}

// ============================================================
// Test: DataPoller.Poll — Full Cycle Integration Tests
// ============================================================

func TestPoll_MediumRange_NoNewData(t *testing.T) {
	repo := newMockForecastRunRepo()
	gfsSource := newMockUpstreamSource(types.ForecastMediumRange, nil) // No new data

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          newMockRunPodClient(),
		GFSSource:       gfsSource,
		ForecastBucket:  "test-bucket",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if triggered != 0 {
		t.Errorf("expected 0 triggered, got %d", triggered)
	}
	if gfsSource.calls != 1 {
		t.Errorf("expected 1 GFS check call, got %d", gfsSource.calls)
	}
}

func TestPoll_MediumRange_NewData(t *testing.T) {
	repo := newMockForecastRunRepo()
	ts := time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC)
	gfsSource := newMockUpstreamSource(types.ForecastMediumRange, []time.Time{ts})
	runpod := newMockRunPodClient("runpod_job_001")

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GFSSource:       gfsSource,
		ForecastBucket:  "watchpoint-forecasts",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if triggered != 1 {
		t.Errorf("expected 1 triggered, got %d", triggered)
	}

	// Verify forecast run was created.
	if repo.createCalls != 1 {
		t.Errorf("expected 1 Create call, got %d", repo.createCalls)
	}

	// Verify RunPod was called with correct payload.
	if len(runpod.triggerCalls) != 1 {
		t.Fatalf("expected 1 RunPod trigger, got %d", len(runpod.triggerCalls))
	}
	payload := runpod.triggerCalls[0]
	if payload.Model != types.ForecastMediumRange {
		t.Errorf("payload.Model = %v, want medium_range", payload.Model)
	}
	if payload.TaskType != types.RunPodTaskInference {
		t.Errorf("payload.TaskType = %v, want inference", payload.TaskType)
	}
	if !payload.RunTimestamp.Equal(ts) {
		t.Errorf("payload.RunTimestamp = %v, want %v", payload.RunTimestamp, ts)
	}
	// Medium-range should NOT have calibration.
	if payload.InputConfig.Calibration != nil {
		t.Error("medium-range payload should not have calibration")
	}

	// Verify external ID was updated.
	if len(repo.updateCalls) != 1 || repo.updateCalls[0] != "runpod_job_001" {
		t.Errorf("expected UpdateExternalID with 'runpod_job_001', got %v", repo.updateCalls)
	}
}

func TestPoll_Nowcast_AlignedData(t *testing.T) {
	repo := newMockForecastRunRepo()
	T1 := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	goesSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{T1})
	mrmsSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{T1.Add(2 * time.Minute)})
	calibRepo := newMockCalibrationRepo()
	runpod := newMockRunPodClient("runpod_nowcast_001")

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: calibRepo,
		RunPod:          runpod,
		GOESSource:      goesSource,
		MRMSSource:      mrmsSource,
		ForecastBucket:  "watchpoint-forecasts",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if triggered != 1 {
		t.Errorf("expected 1 triggered, got %d", triggered)
	}

	// Verify calibration was fetched.
	if calibRepo.calls != 1 {
		t.Errorf("expected 1 calibration fetch, got %d", calibRepo.calls)
	}

	// Verify RunPod payload has calibration injected.
	if len(runpod.triggerCalls) != 1 {
		t.Fatalf("expected 1 RunPod trigger, got %d", len(runpod.triggerCalls))
	}
	payload := runpod.triggerCalls[0]
	if payload.Model != types.ForecastNowcast {
		t.Errorf("payload.Model = %v, want nowcast", payload.Model)
	}
	if payload.InputConfig.Calibration == nil {
		t.Fatal("nowcast payload should have calibration")
	}
	if _, ok := payload.InputConfig.Calibration["tile_39_-77"]; !ok {
		t.Error("calibration map should contain tile_39_-77")
	}
}

func TestPoll_Nowcast_NoAlignment_FAIL005(t *testing.T) {
	// Models FAIL-005: GOES data available, MRMS lagging beyond tolerance.
	repo := newMockForecastRunRepo()
	T1 := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	goesSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{T1})
	mrmsSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{T1.Add(10 * time.Minute)}) // 10 min gap > 5 min tolerance
	runpod := newMockRunPodClient()

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GOESSource:      goesSource,
		MRMSSource:      mrmsSource,
		ForecastBucket:  "watchpoint-forecasts",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if triggered != 0 {
		t.Errorf("expected 0 triggered (no alignment), got %d", triggered)
	}
	if len(runpod.triggerCalls) != 0 {
		t.Errorf("RunPod should not have been called, got %d calls", len(runpod.triggerCalls))
	}
}

func TestPoll_BothModels(t *testing.T) {
	repo := newMockForecastRunRepo()
	gfsTS := time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC)
	nowcastTS := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	gfsSource := newMockUpstreamSource(types.ForecastMediumRange, []time.Time{gfsTS})
	goesSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{nowcastTS})
	mrmsSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{nowcastTS})
	runpod := newMockRunPodClient("gfs_job", "nowcast_job")

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GFSSource:       gfsSource,
		GOESSource:      goesSource,
		MRMSSource:      mrmsSource,
		ForecastBucket:  "watchpoint-forecasts",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if triggered != 2 {
		t.Errorf("expected 2 triggered (1 GFS + 1 nowcast), got %d", triggered)
	}
	if len(runpod.triggerCalls) != 2 {
		t.Errorf("expected 2 RunPod triggers, got %d", len(runpod.triggerCalls))
	}
}

// ============================================================
// Test: Limit Parameter
// ============================================================

func TestPoll_LimitEnforced_MediumRange(t *testing.T) {
	repo := newMockForecastRunRepo()
	base := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	timestamps := []time.Time{
		base.Add(6 * time.Hour),
		base.Add(12 * time.Hour),
		base.Add(18 * time.Hour),
	}
	gfsSource := newMockUpstreamSource(types.ForecastMediumRange, timestamps)
	runpod := newMockRunPodClient("job_1", "job_2", "job_3")

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GFSSource:       gfsSource,
		ForecastBucket:  "test-bucket",
		Logger:          discardLogger(),
	})

	// Limit to 2 triggers.
	triggered, err := poller.Poll(context.Background(), DataPollerInput{Limit: 2})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if triggered != 2 {
		t.Errorf("expected 2 triggered (limited), got %d", triggered)
	}
	if len(runpod.triggerCalls) != 2 {
		t.Errorf("expected 2 RunPod triggers (limited), got %d", len(runpod.triggerCalls))
	}
}

func TestPoll_LimitSpansBothModels(t *testing.T) {
	// Limit of 1 should stop after GFS and not proceed to nowcast triggers.
	repo := newMockForecastRunRepo()
	gfsTS := time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC)
	nowcastTS := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	gfsSource := newMockUpstreamSource(types.ForecastMediumRange, []time.Time{gfsTS})
	goesSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{nowcastTS})
	mrmsSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{nowcastTS})
	runpod := newMockRunPodClient("gfs_job", "nowcast_job")

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GFSSource:       gfsSource,
		GOESSource:      goesSource,
		MRMSSource:      mrmsSource,
		ForecastBucket:  "watchpoint-forecasts",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{Limit: 1})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if triggered != 1 {
		t.Errorf("expected 1 triggered (limit=1), got %d", triggered)
	}
	// Only 1 RunPod trigger should have happened.
	if len(runpod.triggerCalls) != 1 {
		t.Errorf("expected 1 RunPod trigger, got %d", len(runpod.triggerCalls))
	}
}

func TestPoll_LimitZeroMeansUnlimited(t *testing.T) {
	repo := newMockForecastRunRepo()
	base := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	timestamps := []time.Time{
		base.Add(6 * time.Hour),
		base.Add(12 * time.Hour),
		base.Add(18 * time.Hour),
	}
	gfsSource := newMockUpstreamSource(types.ForecastMediumRange, timestamps)
	runpod := newMockRunPodClient("job_1", "job_2", "job_3")

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GFSSource:       gfsSource,
		ForecastBucket:  "test-bucket",
		Logger:          discardLogger(),
	})

	// Limit=0 means no limit.
	triggered, err := poller.Poll(context.Background(), DataPollerInput{Limit: 0})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if triggered != 3 {
		t.Errorf("expected 3 triggered (unlimited), got %d", triggered)
	}
}

// ============================================================
// Test: Error Handling
// ============================================================

func TestPoll_RunPodTriggerFailure_ContinuesWithNext(t *testing.T) {
	repo := newMockForecastRunRepo()
	base := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	timestamps := []time.Time{
		base.Add(6 * time.Hour),
		base.Add(12 * time.Hour),
	}
	gfsSource := newMockUpstreamSource(types.ForecastMediumRange, timestamps)

	// RunPod fails on first call, succeeds on second.
	runpod := &failFirstRunPodClient{
		failCount:  1,
		successIDs: []string{"job_success"},
	}

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GFSSource:       gfsSource,
		ForecastBucket:  "test-bucket",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	// Should have triggered 1 (second one succeeded).
	if triggered != 1 {
		t.Errorf("expected 1 triggered (1 failure + 1 success), got %d", triggered)
	}
	// Create should have been called twice (once for each attempt).
	if repo.createCalls != 2 {
		t.Errorf("expected 2 Create calls, got %d", repo.createCalls)
	}
}

func TestPoll_GFSSourceFailure_ContinuesToNowcast(t *testing.T) {
	repo := newMockForecastRunRepo()
	nowcastTS := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	gfsSource := newMockUpstreamSource(types.ForecastMediumRange, nil)
	gfsSource.err = errors.New("S3 access denied")

	goesSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{nowcastTS})
	mrmsSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{nowcastTS})
	runpod := newMockRunPodClient("nowcast_job")

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GFSSource:       gfsSource,
		GOESSource:      goesSource,
		MRMSSource:      mrmsSource,
		ForecastBucket:  "watchpoint-forecasts",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	// GFS failed but nowcast should still work.
	if triggered != 1 {
		t.Errorf("expected 1 triggered (nowcast only), got %d", triggered)
	}
}

func TestPoll_CalibrationFailure_NowcastSkipped(t *testing.T) {
	repo := newMockForecastRunRepo()
	nowcastTS := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	goesSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{nowcastTS})
	mrmsSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{nowcastTS})

	calibRepo := newMockCalibrationRepo()
	calibRepo.err = errors.New("DB connection refused")

	runpod := newMockRunPodClient()

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: calibRepo,
		RunPod:          runpod,
		GOESSource:      goesSource,
		MRMSSource:      mrmsSource,
		ForecastBucket:  "watchpoint-forecasts",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	// Calibration failure should prevent nowcast from triggering.
	if triggered != 0 {
		t.Errorf("expected 0 triggered, got %d", triggered)
	}
	if len(runpod.triggerCalls) != 0 {
		t.Errorf("RunPod should not have been called")
	}
}

// ============================================================
// Test: BackfillHours
// ============================================================

func TestPoll_BackfillHours(t *testing.T) {
	repo := newMockForecastRunRepo()
	// Add an existing run so the poller has a "latest" timestamp.
	repo.runs = append(repo.runs, &types.ForecastRun{
		ID:           "existing_run",
		Model:        types.ForecastMediumRange,
		RunTimestamp: time.Date(2026, 2, 5, 18, 0, 0, 0, time.UTC),
		Status:       "complete",
	})

	gfsSource := &sinceCapturingSource{
		inner: newMockUpstreamSource(types.ForecastMediumRange, nil),
	}

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          newMockRunPodClient(),
		GFSSource:       gfsSource,
		ForecastBucket:  "test-bucket",
		Logger:          discardLogger(),
	})

	// With BackfillHours=48, the since time should be ~48 hours ago,
	// NOT the latest run timestamp.
	_, err := poller.Poll(context.Background(), DataPollerInput{BackfillHours: 48})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	if gfsSource.capturedSince.IsZero() {
		t.Fatal("expected since time to be captured")
	}

	// Since should be approximately 48 hours before now.
	expectedSince := time.Now().UTC().Add(-48 * time.Hour)
	diff := gfsSource.capturedSince.Sub(expectedSince)
	if diff < 0 {
		diff = -diff
	}
	if diff > 5*time.Second {
		t.Errorf("since time diff from expected: %v (should be within 5s)", diff)
	}
}

// ============================================================
// Test: Output Destination Format
// ============================================================

func TestPoll_OutputDestinationFormat(t *testing.T) {
	repo := newMockForecastRunRepo()
	ts := time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC)
	gfsSource := newMockUpstreamSource(types.ForecastMediumRange, []time.Time{ts})
	runpod := newMockRunPodClient("job_1")

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GFSSource:       gfsSource,
		ForecastBucket:  "watchpoint-forecasts",
		Logger:          discardLogger(),
	})

	_, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	expected := "s3://watchpoint-forecasts/medium_range/2026-02-06T06:00:00Z/"
	if len(runpod.triggerCalls) != 1 {
		t.Fatalf("expected 1 trigger call")
	}
	if runpod.triggerCalls[0].OutputDestination != expected {
		t.Errorf("OutputDestination = %q, want %q",
			runpod.triggerCalls[0].OutputDestination, expected)
	}
}

// ============================================================
// Test: No Sources Configured
// ============================================================

func TestPoll_NoSourcesConfigured(t *testing.T) {
	repo := newMockForecastRunRepo()

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          newMockRunPodClient(),
		ForecastBucket:  "test-bucket",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if triggered != 0 {
		t.Errorf("expected 0 triggered with no sources, got %d", triggered)
	}
}

// ============================================================
// Test: Existing Run Timestamp Used as Since
// ============================================================

func TestPoll_UsesLatestRunTimestampAsSince(t *testing.T) {
	lastRunTS := time.Date(2026, 2, 5, 18, 0, 0, 0, time.UTC)
	repo := newMockForecastRunRepo()
	repo.runs = append(repo.runs, &types.ForecastRun{
		ID:           "prev_run",
		Model:        types.ForecastMediumRange,
		RunTimestamp: lastRunTS,
		Status:       "running",
	})

	gfsSource := &sinceCapturingSource{
		inner: newMockUpstreamSource(types.ForecastMediumRange, nil),
	}

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          newMockRunPodClient(),
		GFSSource:       gfsSource,
		ForecastBucket:  "test-bucket",
		Logger:          discardLogger(),
	})

	_, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	// The since time should be the latest run timestamp, not 24h ago.
	if !gfsSource.capturedSince.Equal(lastRunTS) {
		t.Errorf("since = %v, want %v (latest run timestamp)", gfsSource.capturedSince, lastRunTS)
	}
}

// ============================================================
// Test: Multiple Nowcast Timestamps — Partial Alignment
// ============================================================

func TestPoll_Nowcast_PartialAlignment_MultipleTimestamps(t *testing.T) {
	// GOES has 3 timestamps, MRMS has 2 that only align with 2 of the GOES timestamps.
	repo := newMockForecastRunRepo()
	T1 := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	T2 := T1.Add(15 * time.Minute)
	T3 := T1.Add(30 * time.Minute)

	goesSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{T1, T2, T3})
	// MRMS aligns with T1 and T3 but not T2 (10 min gap).
	mrmsSource := newMockUpstreamSource(types.ForecastNowcast, []time.Time{
		T1.Add(1 * time.Minute),
		T3.Add(2 * time.Minute),
	})
	runpod := newMockRunPodClient("job_1", "job_2")

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GOESSource:      goesSource,
		MRMSSource:      mrmsSource,
		ForecastBucket:  "watchpoint-forecasts",
		Logger:          discardLogger(),
	})

	triggered, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if triggered != 2 {
		t.Errorf("expected 2 triggered (T1 and T3 aligned), got %d", triggered)
	}
}

// ============================================================
// Test: ForecastRun Record Correctness
// ============================================================

func TestPoll_ForecastRunRecord_Fields(t *testing.T) {
	repo := newMockForecastRunRepo()
	ts := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	gfsSource := newMockUpstreamSource(types.ForecastMediumRange, []time.Time{ts})
	runpod := newMockRunPodClient("runpod_job_123")

	poller := NewDataPoller(DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: newMockCalibrationRepo(),
		RunPod:          runpod,
		GFSSource:       gfsSource,
		ForecastBucket:  "my-bucket",
		Logger:          discardLogger(),
	})

	_, err := poller.Poll(context.Background(), DataPollerInput{})
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	if len(repo.runs) != 1 {
		t.Fatalf("expected 1 run in repo, got %d", len(repo.runs))
	}

	run := repo.runs[0]
	if run.Model != types.ForecastMediumRange {
		t.Errorf("run.Model = %v, want medium_range", run.Model)
	}
	if !run.RunTimestamp.Equal(ts) {
		t.Errorf("run.RunTimestamp = %v, want %v", run.RunTimestamp, ts)
	}
	if run.Status != "running" {
		t.Errorf("run.Status = %q, want 'running'", run.Status)
	}
	if run.ExternalID != "runpod_job_123" {
		t.Errorf("run.ExternalID = %q, want 'runpod_job_123'", run.ExternalID)
	}
	expectedPath := "s3://my-bucket/medium_range/2026-02-06T12:00:00Z/"
	if run.StoragePath != expectedPath {
		t.Errorf("run.StoragePath = %q, want %q", run.StoragePath, expectedPath)
	}
}

// ============================================================
// Helper: failFirstRunPodClient fails the first N calls
// ============================================================

type failFirstRunPodClient struct {
	callCount  int
	failCount  int
	successIDs []string
	nextIDIdx  int
}

func (c *failFirstRunPodClient) TriggerInference(_ context.Context, _ types.InferencePayload) (string, error) {
	c.callCount++
	if c.callCount <= c.failCount {
		return "", errors.New("RunPod unavailable")
	}
	if c.nextIDIdx < len(c.successIDs) {
		id := c.successIDs[c.nextIDIdx]
		c.nextIDIdx++
		return id, nil
	}
	return fmt.Sprintf("auto_job_%d", c.callCount), nil
}

// ============================================================
// Helper: sinceCapturingSource wraps a source and captures the since param
// ============================================================

type sinceCapturingSource struct {
	inner         *mockUpstreamSource
	capturedSince time.Time
}

func (s *sinceCapturingSource) Name() types.ForecastType {
	return s.inner.Name()
}

func (s *sinceCapturingSource) CheckAvailability(ctx context.Context, since time.Time) ([]time.Time, error) {
	s.capturedSince = since
	return s.inner.CheckAvailability(ctx, since)
}

// ============================================================
// Test: absDuration helper
// ============================================================

func TestAbsDuration(t *testing.T) {
	tests := []struct {
		input    time.Duration
		expected time.Duration
	}{
		{5 * time.Minute, 5 * time.Minute},
		{-5 * time.Minute, 5 * time.Minute},
		{0, 0},
		{-1, 1},
	}

	for _, tt := range tests {
		result := absDuration(tt.input)
		if result != tt.expected {
			t.Errorf("absDuration(%v) = %v, want %v", tt.input, result, tt.expected)
		}
	}
}
