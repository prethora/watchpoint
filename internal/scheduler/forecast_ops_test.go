package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// ============================================================
// Shared Test Logger
// ============================================================

func forecastOpsTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ============================================================
// Mock: VerificationDB
// ============================================================

type mockVerificationDB struct {
	mu   sync.Mutex
	runs []types.ForecastRun
	err  error
}

func (m *mockVerificationDB) ListCompletedRunsInWindow(_ context.Context, _, _ time.Time) ([]types.ForecastRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return m.runs, nil
}

// ============================================================
// Mock: ObservationProbe
// ============================================================

type mockObservationProbe struct {
	mu        sync.Mutex
	available bool
	err       error
}

func (m *mockObservationProbe) DataExists(_ context.Context, _, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	return m.available, nil
}

// ============================================================
// Mock: VerificationRunPodClient
// ============================================================

type mockVerificationRunPod struct {
	mu         sync.Mutex
	triggered  []types.InferencePayload
	externalID string
	err        error
}

func (m *mockVerificationRunPod) TriggerInference(_ context.Context, payload types.InferencePayload) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return "", m.err
	}
	m.triggered = append(m.triggered, payload)
	return m.externalID, nil
}

// ============================================================
// Mock: CalibrationDB
// ============================================================

type mockCalibrationDB struct {
	mu              sync.Mutex
	coefficients    map[string]types.CalibrationCoefficients
	hasVerification bool
	hasVerifErr     error
	getAllErr       error
	updateErr       error
	createCandErr   error
	updatedCoeffs   []types.CalibrationCoefficients
	candidates      []mockCandidate
}

type mockCandidate struct {
	LocationID      string
	Coefficients    []byte
	ViolationReason string
}

func (m *mockCalibrationDB) GetAllCoefficients(_ context.Context) (map[string]types.CalibrationCoefficients, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getAllErr != nil {
		return nil, m.getAllErr
	}
	return m.coefficients, nil
}

func (m *mockCalibrationDB) UpdateCoefficients(_ context.Context, c *types.CalibrationCoefficients) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateErr != nil {
		return m.updateErr
	}
	m.updatedCoeffs = append(m.updatedCoeffs, *c)
	return nil
}

func (m *mockCalibrationDB) CreateCandidate(_ context.Context, locationID string, coefficients []byte, violationReason string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createCandErr != nil {
		return 0, m.createCandErr
	}
	m.candidates = append(m.candidates, mockCandidate{
		LocationID:      locationID,
		Coefficients:    coefficients,
		ViolationReason: violationReason,
	})
	return int64(len(m.candidates)), nil
}

func (m *mockCalibrationDB) HasRecentVerificationData(_ context.Context, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasVerifErr != nil {
		return false, m.hasVerifErr
	}
	return m.hasVerification, nil
}

// ============================================================
// Mock: ReconcilerDB
// ============================================================

type mockReconcilerDB struct {
	mu         sync.Mutex
	staleRuns  []types.ForecastRun
	listErr    error
	updateErr  error
	markErr    error
	updated    []reconcilerUpdate
	failedRuns []reconcilerFailed
}

type reconcilerUpdate struct {
	ID         string
	ExternalID string
}

type reconcilerFailed struct {
	ID     string
	Reason string
}

func (m *mockReconcilerDB) ListStaleRuns(_ context.Context, _ time.Time) ([]types.ForecastRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.staleRuns, nil
}

func (m *mockReconcilerDB) IncrementRetryAndUpdateExternalID(_ context.Context, id string, externalID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateErr != nil {
		return m.updateErr
	}
	m.updated = append(m.updated, reconcilerUpdate{ID: id, ExternalID: externalID})
	return nil
}

func (m *mockReconcilerDB) MarkRunFailed(_ context.Context, id string, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markErr != nil {
		return m.markErr
	}
	m.failedRuns = append(m.failedRuns, reconcilerFailed{ID: id, Reason: reason})
	return nil
}

// ============================================================
// Mock: ReconcilerRunPodClient
// ============================================================

type mockReconcilerRunPod struct {
	mu           sync.Mutex
	triggered    []types.InferencePayload
	cancelledIDs []string
	externalID   string
	triggerErr   error
	cancelErr    error
	// Per-ID cancel errors
	cancelFailOn map[string]error
}

func (m *mockReconcilerRunPod) TriggerInference(_ context.Context, payload types.InferencePayload) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.triggerErr != nil {
		return "", m.triggerErr
	}
	m.triggered = append(m.triggered, payload)
	return m.externalID, nil
}

func (m *mockReconcilerRunPod) CancelJob(_ context.Context, externalID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancelFailOn != nil {
		if err, ok := m.cancelFailOn[externalID]; ok {
			return err
		}
	}
	if m.cancelErr != nil {
		return m.cancelErr
	}
	m.cancelledIDs = append(m.cancelledIDs, externalID)
	return nil
}

// ============================================================
// VerificationService Tests
// ============================================================

func TestTriggerVerification_Success(t *testing.T) {
	now := time.Date(2026, 2, 6, 3, 0, 0, 0, time.UTC)
	windowStart := now.Add(-VerificationWindowStart)
	windowEnd := now.Add(-VerificationWindowEnd)

	db := &mockVerificationDB{
		runs: []types.ForecastRun{
			{ID: "run_1", Model: types.ForecastMediumRange, Status: "complete", RunTimestamp: windowStart.Add(2 * time.Hour)},
			{ID: "run_2", Model: types.ForecastNowcast, Status: "complete", RunTimestamp: windowStart.Add(6 * time.Hour)},
		},
	}
	probe := &mockObservationProbe{available: true}
	runpod := &mockVerificationRunPod{externalID: "rpod_verif_001"}
	svc := NewVerificationService(db, probe, runpod, forecastOpsTestLogger())

	count, err := svc.TriggerVerification(ctx(), windowStart, windowEnd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 verification job triggered, got %d", count)
	}

	// Verify the RunPod payload.
	if len(runpod.triggered) != 1 {
		t.Fatalf("expected 1 triggered payload, got %d", len(runpod.triggered))
	}
	payload := runpod.triggered[0]
	if payload.TaskType != types.RunPodTaskVerification {
		t.Errorf("expected TaskType %q, got %q", types.RunPodTaskVerification, payload.TaskType)
	}
	if payload.InputConfig.VerificationWindow == nil {
		t.Fatal("expected VerificationWindow to be set")
	}
	if !payload.InputConfig.VerificationWindow.Start.Equal(windowStart) {
		t.Errorf("expected window start %v, got %v", windowStart, payload.InputConfig.VerificationWindow.Start)
	}
	if !payload.InputConfig.VerificationWindow.End.Equal(windowEnd) {
		t.Errorf("expected window end %v, got %v", windowEnd, payload.InputConfig.VerificationWindow.End)
	}
}

func TestTriggerVerification_NoRuns(t *testing.T) {
	db := &mockVerificationDB{runs: []types.ForecastRun{}}
	probe := &mockObservationProbe{available: true}
	runpod := &mockVerificationRunPod{externalID: "rpod_001"}
	svc := NewVerificationService(db, probe, runpod, forecastOpsTestLogger())

	count, err := svc.TriggerVerification(ctx(), time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 (no runs), got %d", count)
	}
	// RunPod should NOT be called.
	if len(runpod.triggered) != 0 {
		t.Errorf("expected 0 RunPod triggers, got %d", len(runpod.triggered))
	}
}

func TestTriggerVerification_NoObservationalData(t *testing.T) {
	db := &mockVerificationDB{
		runs: []types.ForecastRun{
			{ID: "run_1", Status: "complete"},
		},
	}
	probe := &mockObservationProbe{available: false}
	runpod := &mockVerificationRunPod{externalID: "rpod_001"}
	svc := NewVerificationService(db, probe, runpod, forecastOpsTestLogger())

	count, err := svc.TriggerVerification(ctx(), time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 (no observational data), got %d", count)
	}
	if len(runpod.triggered) != 0 {
		t.Errorf("expected 0 RunPod triggers, got %d", len(runpod.triggered))
	}
}

func TestTriggerVerification_DBError(t *testing.T) {
	db := &mockVerificationDB{err: errors.New("db connection lost")}
	probe := &mockObservationProbe{available: true}
	runpod := &mockVerificationRunPod{externalID: "rpod_001"}
	svc := NewVerificationService(db, probe, runpod, forecastOpsTestLogger())

	_, err := svc.TriggerVerification(ctx(), time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestTriggerVerification_ProbeError(t *testing.T) {
	db := &mockVerificationDB{
		runs: []types.ForecastRun{{ID: "run_1", Status: "complete"}},
	}
	probe := &mockObservationProbe{err: errors.New("S3 access denied")}
	runpod := &mockVerificationRunPod{externalID: "rpod_001"}
	svc := NewVerificationService(db, probe, runpod, forecastOpsTestLogger())

	_, err := svc.TriggerVerification(ctx(), time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestTriggerVerification_RunPodError(t *testing.T) {
	db := &mockVerificationDB{
		runs: []types.ForecastRun{{ID: "run_1", Status: "complete"}},
	}
	probe := &mockObservationProbe{available: true}
	runpod := &mockVerificationRunPod{err: errors.New("RunPod API timeout")}
	svc := NewVerificationService(db, probe, runpod, forecastOpsTestLogger())

	_, err := svc.TriggerVerification(ctx(), time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ============================================================
// CalibrationService Tests
// ============================================================

func TestUpdateCoefficients_Success(t *testing.T) {
	now := time.Date(2026, 2, 6, 5, 0, 0, 0, time.UTC)
	db := &mockCalibrationDB{
		hasVerification: true,
		coefficients:    map[string]types.CalibrationCoefficients{},
	}
	runpod := &mockVerificationRunPod{externalID: "rpod_calib_001"}
	svc := NewCalibrationService(db, runpod, forecastOpsTestLogger())

	count, err := svc.UpdateCoefficients(ctx(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 calibration job triggered, got %d", count)
	}

	// Verify the RunPod payload.
	if len(runpod.triggered) != 1 {
		t.Fatalf("expected 1 triggered, got %d", len(runpod.triggered))
	}
	payload := runpod.triggered[0]
	if payload.TaskType != types.RunPodTaskCalibration {
		t.Errorf("expected TaskType %q, got %q", types.RunPodTaskCalibration, payload.TaskType)
	}
	if payload.InputConfig.CalibrationTimeRange == nil {
		t.Fatal("expected CalibrationTimeRange to be set")
	}

	// Verify time range: 7 days back to now.
	expectedStart := now.Add(-CalibrationLookback)
	if !payload.InputConfig.CalibrationTimeRange.Start.Equal(expectedStart) {
		t.Errorf("expected time range start %v, got %v", expectedStart, payload.InputConfig.CalibrationTimeRange.Start)
	}
	if !payload.InputConfig.CalibrationTimeRange.End.Equal(now) {
		t.Errorf("expected time range end %v, got %v", now, payload.InputConfig.CalibrationTimeRange.End)
	}
}

func TestUpdateCoefficients_NoVerificationData(t *testing.T) {
	db := &mockCalibrationDB{hasVerification: false}
	runpod := &mockVerificationRunPod{externalID: "rpod_001"}
	svc := NewCalibrationService(db, runpod, forecastOpsTestLogger())

	count, err := svc.UpdateCoefficients(ctx(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 (no verification data), got %d", count)
	}
	if len(runpod.triggered) != 0 {
		t.Errorf("expected 0 RunPod triggers, got %d", len(runpod.triggered))
	}
}

func TestUpdateCoefficients_VerificationCheckError(t *testing.T) {
	db := &mockCalibrationDB{hasVerifErr: errors.New("db timeout")}
	runpod := &mockVerificationRunPod{externalID: "rpod_001"}
	svc := NewCalibrationService(db, runpod, forecastOpsTestLogger())

	_, err := svc.UpdateCoefficients(ctx(), time.Now())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestUpdateCoefficients_RunPodError(t *testing.T) {
	db := &mockCalibrationDB{hasVerification: true}
	runpod := &mockVerificationRunPod{err: errors.New("RunPod unavailable")}
	svc := NewCalibrationService(db, runpod, forecastOpsTestLogger())

	_, err := svc.UpdateCoefficients(ctx(), time.Now())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ============================================================
// Calibration Safety Check Tests
// ============================================================

func TestCheckCalibrationSafety_AllSafe(t *testing.T) {
	current := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 100, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
		"loc_2": {LocationID: "loc_2", HighThreshold: 80, HighSlope: 1.8, HighBase: 0.4, MidThreshold: 40, MidSlope: 1.2, MidBase: 0.2, LowBase: 0.08},
	}
	// Proposed values within 15% of current.
	proposed := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 105, HighSlope: 2.1, HighBase: 0.52, MidThreshold: 52, MidSlope: 1.55, MidBase: 0.31, LowBase: 0.105},
		"loc_2": {LocationID: "loc_2", HighThreshold: 82, HighSlope: 1.85, HighBase: 0.42, MidThreshold: 41, MidSlope: 1.25, MidBase: 0.21, LowBase: 0.085},
	}

	safe, unsafe := CheckCalibrationSafety(current, proposed)
	if len(safe) != 2 {
		t.Errorf("expected 2 safe, got %d", len(safe))
	}
	if len(unsafe) != 0 {
		t.Errorf("expected 0 unsafe, got %d", len(unsafe))
	}
}

func TestCheckCalibrationSafety_OneUnsafe(t *testing.T) {
	current := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 100, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
		"loc_2": {LocationID: "loc_2", HighThreshold: 80, HighSlope: 1.8, HighBase: 0.4, MidThreshold: 40, MidSlope: 1.2, MidBase: 0.2, LowBase: 0.08},
	}
	// loc_1 has a 20% change in HighThreshold (unsafe), loc_2 is within bounds.
	proposed := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 120, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
		"loc_2": {LocationID: "loc_2", HighThreshold: 82, HighSlope: 1.85, HighBase: 0.42, MidThreshold: 41, MidSlope: 1.25, MidBase: 0.21, LowBase: 0.085},
	}

	safe, unsafe := CheckCalibrationSafety(current, proposed)
	if len(safe) != 1 {
		t.Errorf("expected 1 safe, got %d", len(safe))
	}
	if len(unsafe) != 1 {
		t.Errorf("expected 1 unsafe, got %d", len(unsafe))
	}
	if len(unsafe) > 0 {
		if unsafe[0].LocationID != "loc_1" {
			t.Errorf("expected loc_1 to be unsafe, got %s", unsafe[0].LocationID)
		}
		if unsafe[0].MaxDelta < CalibrationSafetyThreshold {
			t.Errorf("expected max delta > 15%%, got %.2f%%", unsafe[0].MaxDelta*100)
		}
	}
}

func TestCheckCalibrationSafety_AllUnsafe(t *testing.T) {
	current := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 100, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
	}
	// 30% change on everything.
	proposed := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 130, HighSlope: 2.6, HighBase: 0.65, MidThreshold: 65, MidSlope: 1.95, MidBase: 0.39, LowBase: 0.13},
	}

	safe, unsafe := CheckCalibrationSafety(current, proposed)
	if len(safe) != 0 {
		t.Errorf("expected 0 safe, got %d", len(safe))
	}
	if len(unsafe) != 1 {
		t.Errorf("expected 1 unsafe, got %d", len(unsafe))
	}
}

func TestCheckCalibrationSafety_NewLocation(t *testing.T) {
	// New location (no existing coefficients) should always be safe.
	current := map[string]types.CalibrationCoefficients{}
	proposed := map[string]types.CalibrationCoefficients{
		"new_loc": {LocationID: "new_loc", HighThreshold: 100, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
	}

	safe, unsafe := CheckCalibrationSafety(current, proposed)
	if len(safe) != 1 {
		t.Errorf("expected 1 safe (new location), got %d", len(safe))
	}
	if len(unsafe) != 0 {
		t.Errorf("expected 0 unsafe, got %d", len(unsafe))
	}
}

func TestCheckCalibrationSafety_ExactThreshold(t *testing.T) {
	// Exactly at 15% should NOT trigger the safety check (uses > not >=).
	current := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 100, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
	}
	// 15% change on HighThreshold.
	proposed := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 115, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
	}

	safe, unsafe := CheckCalibrationSafety(current, proposed)
	// Exactly 15% is NOT > 15%, so it should be safe.
	if len(safe) != 1 {
		t.Errorf("expected 1 safe (exactly at threshold), got %d", len(safe))
	}
	if len(unsafe) != 0 {
		t.Errorf("expected 0 unsafe, got %d", len(unsafe))
	}
}

func TestCheckCalibrationSafety_JustAboveThreshold(t *testing.T) {
	current := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 100, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
	}
	// 15.01% change on HighThreshold.
	proposed := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 115.01, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
	}

	safe, unsafe := CheckCalibrationSafety(current, proposed)
	if len(safe) != 0 {
		t.Errorf("expected 0 safe, got %d", len(safe))
	}
	if len(unsafe) != 1 {
		t.Errorf("expected 1 unsafe, got %d", len(unsafe))
	}
}

func TestCheckCalibrationSafety_EmptyProposed(t *testing.T) {
	current := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 100},
	}
	proposed := map[string]types.CalibrationCoefficients{}

	safe, unsafe := CheckCalibrationSafety(current, proposed)
	if len(safe) != 0 {
		t.Errorf("expected 0 safe, got %d", len(safe))
	}
	if len(unsafe) != 0 {
		t.Errorf("expected 0 unsafe, got %d", len(unsafe))
	}
}

func TestCheckCalibrationSafety_ViolationReason(t *testing.T) {
	current := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 100, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
	}
	proposed := map[string]types.CalibrationCoefficients{
		"loc_1": {LocationID: "loc_1", HighThreshold: 120, HighSlope: 2.0, HighBase: 0.5, MidThreshold: 50, MidSlope: 1.5, MidBase: 0.3, LowBase: 0.1},
	}

	_, unsafe := CheckCalibrationSafety(current, proposed)
	if len(unsafe) != 1 {
		t.Fatalf("expected 1 unsafe, got %d", len(unsafe))
	}
	// Verify the violation reason contains the delta and threshold.
	reason := unsafe[0].Reason
	if reason == "" {
		t.Error("expected non-empty violation reason")
	}
	// The reason should mention the percentage.
	if !containsSubstr(reason, "20.00%") {
		t.Errorf("expected reason to contain '20.00%%', got: %s", reason)
	}
	if !containsSubstr(reason, "15%") {
		t.Errorf("expected reason to contain '15%%', got: %s", reason)
	}
}

// ============================================================
// Relative Delta Tests
// ============================================================

func TestRelativeDelta(t *testing.T) {
	tests := []struct {
		name     string
		old      float64
		new      float64
		expected float64
	}{
		{"no change", 100, 100, 0},
		{"10% increase", 100, 110, 0.10},
		{"10% decrease", 100, 90, 0.10},
		{"20% increase", 100, 120, 0.20},
		{"zero old, non-zero new", 0, 5.0, 5.0}, // Absolute diff when old is zero
		{"both zero", 0, 0, 0},
		{"negative values", -100, -115, 0.15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := relativeDelta(tt.old, tt.new)
			if diff := got - tt.expected; diff > 0.001 || diff < -0.001 {
				t.Errorf("relativeDelta(%v, %v) = %v, want %v", tt.old, tt.new, got, tt.expected)
			}
		})
	}
}

// ============================================================
// ForecastReconciler Tests
// ============================================================

func TestReconcileStaleRuns_TimeoutAndRetry(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	db := &mockReconcilerDB{
		staleRuns: []types.ForecastRun{
			{
				ID:         "run_1",
				Model:      types.ForecastMediumRange,
				ExternalID: "rpod_old_1",
				RetryCount: 0,
				Status:     "running",
				CreatedAt:  now.Add(-4 * time.Hour), // 4h ago, > 3h timeout for medium-range
				RunTimestamp:   now.Add(-4 * time.Hour),
				StoragePath:    "s3://bucket/medium_range/old/",
			},
		},
	}
	runpod := &mockReconcilerRunPod{externalID: "rpod_new_1"}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:                 db,
		RunPod:             runpod,
		TimeoutMediumRange: 3 * time.Hour,
		TimeoutNowcast:     20 * time.Minute,
		Logger:             forecastOpsTestLogger(),
	})

	processed, err := svc.ReconcileStaleRuns(ctx(), now, 2*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 1 {
		t.Errorf("expected 1 processed, got %d", processed)
	}

	// Verify cancellation.
	if len(runpod.cancelledIDs) != 1 || runpod.cancelledIDs[0] != "rpod_old_1" {
		t.Errorf("expected cancel on rpod_old_1, got %v", runpod.cancelledIDs)
	}

	// Verify re-submission.
	if len(runpod.triggered) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(runpod.triggered))
	}
	payload := runpod.triggered[0]
	if payload.TaskType != types.RunPodTaskInference {
		t.Errorf("expected TaskType %q, got %q", types.RunPodTaskInference, payload.TaskType)
	}
	if !payload.Options.ForceRebuild {
		t.Error("expected ForceRebuild to be true")
	}
	if payload.Model != types.ForecastMediumRange {
		t.Errorf("expected model %q, got %q", types.ForecastMediumRange, payload.Model)
	}

	// Verify DB update.
	if len(db.updated) != 1 {
		t.Fatalf("expected 1 DB update, got %d", len(db.updated))
	}
	if db.updated[0].ExternalID != "rpod_new_1" {
		t.Errorf("expected external_id rpod_new_1, got %s", db.updated[0].ExternalID)
	}
}

func TestReconcileStaleRuns_MaxRetriesExhausted(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	db := &mockReconcilerDB{
		staleRuns: []types.ForecastRun{
			{
				ID:         "run_maxed",
				Model:      types.ForecastNowcast,
				ExternalID: "rpod_old_maxed",
				RetryCount: 3, // >= MaxRetryCount
				Status:     "running",
				CreatedAt:  now.Add(-1 * time.Hour), // Past nowcast timeout (20m)
			},
		},
	}
	runpod := &mockReconcilerRunPod{externalID: "rpod_should_not_be_used"}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:             db,
		RunPod:         runpod,
		TimeoutNowcast: 20 * time.Minute,
		Logger:         forecastOpsTestLogger(),
	})

	processed, err := svc.ReconcileStaleRuns(ctx(), now, 20*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 1 {
		t.Errorf("expected 1 processed, got %d", processed)
	}

	// Verify cancellation still happens.
	if len(runpod.cancelledIDs) != 1 {
		t.Errorf("expected 1 cancel, got %d", len(runpod.cancelledIDs))
	}

	// Should NOT trigger a new inference job.
	if len(runpod.triggered) != 0 {
		t.Errorf("expected 0 triggers (max retries), got %d", len(runpod.triggered))
	}

	// Should mark as failed.
	if len(db.failedRuns) != 1 {
		t.Fatalf("expected 1 failed, got %d", len(db.failedRuns))
	}
	if db.failedRuns[0].ID != "run_maxed" {
		t.Errorf("expected run_maxed to be failed, got %s", db.failedRuns[0].ID)
	}
	if db.failedRuns[0].Reason != "Max retries exhausted" {
		t.Errorf("expected reason 'Max retries exhausted', got %q", db.failedRuns[0].Reason)
	}
}

func TestReconcileStaleRuns_ModelSpecificTimeout_NowcastSkipped(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	db := &mockReconcilerDB{
		staleRuns: []types.ForecastRun{
			{
				ID:         "run_nowcast",
				Model:      types.ForecastNowcast,
				ExternalID: "rpod_nc",
				RetryCount: 0,
				Status:     "running",
				CreatedAt:  now.Add(-10 * time.Minute), // Only 10m ago, < 20m timeout
			},
		},
	}
	runpod := &mockReconcilerRunPod{externalID: "rpod_new"}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:             db,
		RunPod:         runpod,
		TimeoutNowcast: 20 * time.Minute,
		Logger:         forecastOpsTestLogger(),
	})

	// Use a 5-minute threshold so the DB query returns the run,
	// but the model-specific timeout (20m) means it's not actually stale.
	processed, err := svc.ReconcileStaleRuns(ctx(), now, 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 0 {
		t.Errorf("expected 0 processed (not past model timeout), got %d", processed)
	}
	if len(runpod.cancelledIDs) != 0 {
		t.Errorf("expected 0 cancels, got %d", len(runpod.cancelledIDs))
	}
}

func TestReconcileStaleRuns_NoStaleRuns(t *testing.T) {
	db := &mockReconcilerDB{staleRuns: []types.ForecastRun{}}
	runpod := &mockReconcilerRunPod{externalID: "rpod_001"}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:     db,
		RunPod: runpod,
		Logger: forecastOpsTestLogger(),
	})

	processed, err := svc.ReconcileStaleRuns(ctx(), time.Now(), 2*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 0 {
		t.Errorf("expected 0, got %d", processed)
	}
}

func TestReconcileStaleRuns_ListError(t *testing.T) {
	db := &mockReconcilerDB{listErr: errors.New("db timeout")}
	runpod := &mockReconcilerRunPod{}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:     db,
		RunPod: runpod,
		Logger: forecastOpsTestLogger(),
	})

	_, err := svc.ReconcileStaleRuns(ctx(), time.Now(), 2*time.Hour)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestReconcileStaleRuns_CancelFailure_StillRetries(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	db := &mockReconcilerDB{
		staleRuns: []types.ForecastRun{
			{
				ID:         "run_cancel_fail",
				Model:      types.ForecastMediumRange,
				ExternalID: "rpod_dead",
				RetryCount: 1,
				Status:     "running",
				CreatedAt:  now.Add(-5 * time.Hour),
				RunTimestamp:   now.Add(-5 * time.Hour),
				StoragePath:    "s3://bucket/medium_range/retry/",
			},
		},
	}
	runpod := &mockReconcilerRunPod{
		externalID:   "rpod_retry_2",
		cancelFailOn: map[string]error{"rpod_dead": errors.New("RunPod 404: job not found")},
	}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:                 db,
		RunPod:             runpod,
		TimeoutMediumRange: 3 * time.Hour,
		Logger:             forecastOpsTestLogger(),
	})

	processed, err := svc.ReconcileStaleRuns(ctx(), now, 2*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should still process (cancel failure is not fatal).
	if processed != 1 {
		t.Errorf("expected 1 processed, got %d", processed)
	}
	// Should still re-submit.
	if len(runpod.triggered) != 1 {
		t.Errorf("expected 1 trigger, got %d", len(runpod.triggered))
	}
	if len(db.updated) != 1 {
		t.Errorf("expected 1 DB update, got %d", len(db.updated))
	}
}

func TestReconcileStaleRuns_TriggerFailure_MarksFailed(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	db := &mockReconcilerDB{
		staleRuns: []types.ForecastRun{
			{
				ID:         "run_trigger_fail",
				Model:      types.ForecastNowcast,
				ExternalID: "rpod_old",
				RetryCount: 1,
				Status:     "running",
				CreatedAt:  now.Add(-1 * time.Hour),
			},
		},
	}
	runpod := &mockReconcilerRunPod{
		triggerErr: errors.New("RunPod capacity full"),
	}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:             db,
		RunPod:         runpod,
		TimeoutNowcast: 20 * time.Minute,
		Logger:         forecastOpsTestLogger(),
	})

	processed, err := svc.ReconcileStaleRuns(ctx(), now, 20*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The run counts as processed (failed to retry) but the error is handled gracefully.
	// reconcileRun returns an error, so it won't be counted.
	if processed != 0 {
		t.Errorf("expected 0 processed (trigger failed), got %d", processed)
	}

	// Should have attempted to mark as failed when trigger fails.
	if len(db.failedRuns) != 1 {
		t.Fatalf("expected 1 failed run recorded, got %d", len(db.failedRuns))
	}
	if !containsSubstr(db.failedRuns[0].Reason, "Re-submission failed") {
		t.Errorf("expected failure reason to mention 're-submission', got: %s", db.failedRuns[0].Reason)
	}
}

func TestReconcileStaleRuns_EmptyExternalID_SkipsCancel(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	db := &mockReconcilerDB{
		staleRuns: []types.ForecastRun{
			{
				ID:         "run_no_external",
				Model:      types.ForecastNowcast,
				ExternalID: "", // No external ID (trigger failed before ID was set)
				RetryCount: 0,
				Status:     "running",
				CreatedAt:  now.Add(-1 * time.Hour),
				RunTimestamp:   now.Add(-1 * time.Hour),
				StoragePath:    "s3://bucket/nowcast/orphan/",
			},
		},
	}
	runpod := &mockReconcilerRunPod{externalID: "rpod_new"}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:             db,
		RunPod:         runpod,
		TimeoutNowcast: 20 * time.Minute,
		Logger:         forecastOpsTestLogger(),
	})

	processed, err := svc.ReconcileStaleRuns(ctx(), now, 20*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 1 {
		t.Errorf("expected 1 processed, got %d", processed)
	}
	// CancelJob should NOT be called since there's no external ID.
	if len(runpod.cancelledIDs) != 0 {
		t.Errorf("expected 0 cancels (no external ID), got %d", len(runpod.cancelledIDs))
	}
	// Should still re-submit.
	if len(runpod.triggered) != 1 {
		t.Errorf("expected 1 trigger, got %d", len(runpod.triggered))
	}
}

func TestReconcileStaleRuns_MultipleRuns_MixedResults(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	db := &mockReconcilerDB{
		staleRuns: []types.ForecastRun{
			{
				ID:          "run_retry",
				Model:       types.ForecastMediumRange,
				ExternalID:  "rpod_1",
				RetryCount:  1,
				Status:      "running",
				CreatedAt:   now.Add(-5 * time.Hour),
				RunTimestamp:    now.Add(-5 * time.Hour),
				StoragePath:    "s3://bucket/medium_range/1/",
			},
			{
				ID:          "run_fail",
				Model:       types.ForecastMediumRange,
				ExternalID:  "rpod_2",
				RetryCount:  3, // Max retries
				Status:      "running",
				CreatedAt:   now.Add(-5 * time.Hour),
			},
			{
				ID:          "run_young",
				Model:       types.ForecastNowcast,
				ExternalID:  "rpod_3",
				RetryCount:  0,
				Status:      "running",
				CreatedAt:   now.Add(-10 * time.Minute), // Not past nowcast timeout
			},
		},
	}
	runpod := &mockReconcilerRunPod{externalID: "rpod_new"}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:                 db,
		RunPod:             runpod,
		TimeoutMediumRange: 3 * time.Hour,
		TimeoutNowcast:     20 * time.Minute,
		Logger:             forecastOpsTestLogger(),
	})

	processed, err := svc.ReconcileStaleRuns(ctx(), now, 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// run_retry: retried (processed)
	// run_fail: max retries exhausted (processed)
	// run_young: skipped (not past model timeout)
	if processed != 2 {
		t.Errorf("expected 2 processed, got %d", processed)
	}

	// Verify: 2 cancels (run_retry + run_fail), 1 trigger (run_retry), 1 fail (run_fail)
	if len(runpod.cancelledIDs) != 2 {
		t.Errorf("expected 2 cancels, got %d", len(runpod.cancelledIDs))
	}
	if len(runpod.triggered) != 1 {
		t.Errorf("expected 1 trigger, got %d", len(runpod.triggered))
	}
	if len(db.failedRuns) != 1 {
		t.Errorf("expected 1 failed, got %d", len(db.failedRuns))
	}
	if len(db.updated) != 1 {
		t.Errorf("expected 1 update (retry), got %d", len(db.updated))
	}
}

func TestReconcileStaleRuns_DefaultTimeouts(t *testing.T) {
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:     &mockReconcilerDB{},
		RunPod: &mockReconcilerRunPod{},
		Logger: forecastOpsTestLogger(),
	})

	if svc.timeoutNowcast != 20*time.Minute {
		t.Errorf("expected default nowcast timeout 20m, got %v", svc.timeoutNowcast)
	}
	if svc.timeoutMediumRange != 3*time.Hour {
		t.Errorf("expected default medium-range timeout 3h, got %v", svc.timeoutMediumRange)
	}
}

// ============================================================
// Constants Tests
// ============================================================

func TestConstants(t *testing.T) {
	if VerificationWindowStart != 48*time.Hour {
		t.Errorf("VerificationWindowStart = %v, want 48h", VerificationWindowStart)
	}
	if VerificationWindowEnd != 24*time.Hour {
		t.Errorf("VerificationWindowEnd = %v, want 24h", VerificationWindowEnd)
	}
	if CalibrationLookback != 7*24*time.Hour {
		t.Errorf("CalibrationLookback = %v, want 7d", CalibrationLookback)
	}
	if CalibrationSafetyThreshold != 0.15 {
		t.Errorf("CalibrationSafetyThreshold = %v, want 0.15", CalibrationSafetyThreshold)
	}
	if MaxRetryCount != 3 {
		t.Errorf("MaxRetryCount = %v, want 3", MaxRetryCount)
	}
}

// ============================================================
// CalibrationViolation JSON Serialization Test
// ============================================================

func TestCalibrationViolation_CanSerialize(t *testing.T) {
	violation := CalibrationViolation{
		LocationID: "loc_test",
		Proposed: types.CalibrationCoefficients{
			LocationID:    "loc_test",
			HighThreshold: 120,
		},
		MaxDelta: 0.20,
		Reason:   "max delta 20.00% exceeds 15% threshold",
	}

	data, err := json.Marshal(violation)
	if err != nil {
		t.Fatalf("failed to marshal CalibrationViolation: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty JSON output")
	}
}

// ============================================================
// Reconciler TimeoutForModel Tests
// ============================================================

func TestTimeoutForModel(t *testing.T) {
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:                 &mockReconcilerDB{},
		RunPod:             &mockReconcilerRunPod{},
		TimeoutNowcast:     20 * time.Minute,
		TimeoutMediumRange: 3 * time.Hour,
		Logger:             forecastOpsTestLogger(),
	})

	if svc.timeoutForModel(types.ForecastNowcast) != 20*time.Minute {
		t.Errorf("expected nowcast timeout 20m, got %v", svc.timeoutForModel(types.ForecastNowcast))
	}
	if svc.timeoutForModel(types.ForecastMediumRange) != 3*time.Hour {
		t.Errorf("expected medium-range timeout 3h, got %v", svc.timeoutForModel(types.ForecastMediumRange))
	}
	// Unknown model should default to medium-range timeout.
	if svc.timeoutForModel("unknown_model") != 3*time.Hour {
		t.Errorf("expected unknown model timeout 3h (default), got %v", svc.timeoutForModel("unknown_model"))
	}
}

// ============================================================
// Reconciler RetryCount Boundary Tests
// ============================================================

func TestReconcileStaleRuns_RetryCountBoundary_TwoRetries(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	db := &mockReconcilerDB{
		staleRuns: []types.ForecastRun{
			{
				ID:         "run_retry2",
				Model:      types.ForecastNowcast,
				ExternalID: "rpod_old",
				RetryCount: 2, // One below max
				Status:     "running",
				CreatedAt:  now.Add(-1 * time.Hour),
				RunTimestamp:   now.Add(-1 * time.Hour),
				StoragePath:    "s3://bucket/nowcast/retry/",
			},
		},
	}
	runpod := &mockReconcilerRunPod{externalID: "rpod_retry3"}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:             db,
		RunPod:         runpod,
		TimeoutNowcast: 20 * time.Minute,
		Logger:         forecastOpsTestLogger(),
	})

	processed, err := svc.ReconcileStaleRuns(ctx(), now, 20*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 1 {
		t.Errorf("expected 1 processed, got %d", processed)
	}
	// Should still retry (retry_count=2 < 3).
	if len(runpod.triggered) != 1 {
		t.Errorf("expected 1 trigger (retry), got %d", len(runpod.triggered))
	}
	if len(db.failedRuns) != 0 {
		t.Errorf("expected 0 failed (should retry), got %d", len(db.failedRuns))
	}
}

func TestReconcileStaleRuns_RetryCountBoundary_ExactlyThree(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	db := &mockReconcilerDB{
		staleRuns: []types.ForecastRun{
			{
				ID:         "run_max",
				Model:      types.ForecastNowcast,
				ExternalID: "rpod_old",
				RetryCount: 3, // Exactly at max
				Status:     "running",
				CreatedAt:  now.Add(-1 * time.Hour),
			},
		},
	}
	runpod := &mockReconcilerRunPod{externalID: "rpod_unused"}
	svc := NewForecastReconciler(ForecastReconcilerConfig{
		DB:             db,
		RunPod:         runpod,
		TimeoutNowcast: 20 * time.Minute,
		Logger:         forecastOpsTestLogger(),
	})

	processed, err := svc.ReconcileStaleRuns(ctx(), now, 20*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 1 {
		t.Errorf("expected 1 processed, got %d", processed)
	}
	// Should NOT retry (retry_count=3 >= 3).
	if len(runpod.triggered) != 0 {
		t.Errorf("expected 0 triggers (max retries), got %d", len(runpod.triggered))
	}
	if len(db.failedRuns) != 1 {
		t.Fatalf("expected 1 failed, got %d", len(db.failedRuns))
	}
}

// ============================================================
// Unused import guard for fmt in this test file
// ============================================================

var _ = fmt.Sprintf
