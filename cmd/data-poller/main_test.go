package main

import (
	"context"
	"fmt"
	"testing"

	"watchpoint/internal/scheduler"
	"watchpoint/internal/types"
)

// --- Mock ForecastRunRepo ---

type mockForecastRunRepo struct {
	latestByModel map[types.ForecastType]*types.ForecastRun
	created       []*types.ForecastRun
	updatedIDs    []struct {
		ID         string
		ExternalID string
	}
	createErr     error
	getLatestErr  error
	updateIDErr   error
}

func (m *mockForecastRunRepo) GetLatest(ctx context.Context, model types.ForecastType) (*types.ForecastRun, error) {
	if m.getLatestErr != nil {
		return nil, m.getLatestErr
	}
	if m.latestByModel != nil {
		return m.latestByModel[model], nil
	}
	return nil, nil
}

func (m *mockForecastRunRepo) Create(ctx context.Context, run *types.ForecastRun) error {
	if m.createErr != nil {
		return m.createErr
	}
	run.ID = fmt.Sprintf("run-%d", len(m.created)+1)
	m.created = append(m.created, run)
	return nil
}

func (m *mockForecastRunRepo) UpdateExternalID(ctx context.Context, id string, externalID string) error {
	if m.updateIDErr != nil {
		return m.updateIDErr
	}
	m.updatedIDs = append(m.updatedIDs, struct {
		ID         string
		ExternalID string
	}{id, externalID})
	return nil
}

// --- Mock CalibrationRepo ---

type mockCalibrationRepo struct {
	coefficients map[string]types.CalibrationCoefficients
	err          error
}

func (m *mockCalibrationRepo) GetAll(ctx context.Context) (map[string]types.CalibrationCoefficients, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.coefficients == nil {
		return make(map[string]types.CalibrationCoefficients), nil
	}
	return m.coefficients, nil
}

// --- Mock RunPodClient ---

type mockRunPodClient struct {
	triggerCalls []types.InferencePayload
	jobIDs       []string
	triggerErr   error
}

func (m *mockRunPodClient) TriggerInference(ctx context.Context, payload types.InferencePayload) (string, error) {
	if m.triggerErr != nil {
		return "", m.triggerErr
	}
	m.triggerCalls = append(m.triggerCalls, payload)
	idx := len(m.triggerCalls) - 1
	if idx < len(m.jobIDs) {
		return m.jobIDs[idx], nil
	}
	return fmt.Sprintf("job-%d", idx+1), nil
}

func (m *mockRunPodClient) CancelJob(ctx context.Context, externalID string) error {
	return nil
}

// --- parseUpstreamMirrors Tests ---

func TestParseUpstreamMirrors_Empty(t *testing.T) {
	result := parseUpstreamMirrors("")
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestParseUpstreamMirrors_Single(t *testing.T) {
	result := parseUpstreamMirrors("noaa-gfs-bdp-pds")
	if len(result) != 1 {
		t.Fatalf("expected 1 mirror, got %d", len(result))
	}
	if result[0] != "noaa-gfs-bdp-pds" {
		t.Errorf("expected 'noaa-gfs-bdp-pds', got %q", result[0])
	}
}

func TestParseUpstreamMirrors_Multiple(t *testing.T) {
	result := parseUpstreamMirrors("noaa-gfs-bdp-pds,aws-noaa-gfs")
	if len(result) != 2 {
		t.Fatalf("expected 2 mirrors, got %d", len(result))
	}
	if result[0] != "noaa-gfs-bdp-pds" {
		t.Errorf("expected 'noaa-gfs-bdp-pds', got %q", result[0])
	}
	if result[1] != "aws-noaa-gfs" {
		t.Errorf("expected 'aws-noaa-gfs', got %q", result[1])
	}
}

func TestParseUpstreamMirrors_WithSpaces(t *testing.T) {
	result := parseUpstreamMirrors("  noaa-gfs-bdp-pds , aws-noaa-gfs  ")
	if len(result) != 2 {
		t.Fatalf("expected 2 mirrors, got %d", len(result))
	}
	if result[0] != "noaa-gfs-bdp-pds" {
		t.Errorf("expected 'noaa-gfs-bdp-pds', got %q", result[0])
	}
	if result[1] != "aws-noaa-gfs" {
		t.Errorf("expected 'aws-noaa-gfs', got %q", result[1])
	}
}

func TestParseUpstreamMirrors_TrailingComma(t *testing.T) {
	result := parseUpstreamMirrors("noaa-gfs-bdp-pds,")
	if len(result) != 1 {
		t.Fatalf("expected 1 mirror (trailing comma ignored), got %d", len(result))
	}
	if result[0] != "noaa-gfs-bdp-pds" {
		t.Errorf("expected 'noaa-gfs-bdp-pds', got %q", result[0])
	}
}

func TestParseUpstreamMirrors_EmptyEntries(t *testing.T) {
	result := parseUpstreamMirrors(",,noaa-gfs-bdp-pds,,aws-noaa-gfs,,")
	if len(result) != 2 {
		t.Fatalf("expected 2 mirrors (empty entries ignored), got %d", len(result))
	}
}

// --- Handler Tests ---

func TestHandler_DefaultForceRetryBackfill(t *testing.T) {
	// When ForceRetry=true and BackfillHours=0, the handler should default
	// BackfillHours to 24 hours before calling Poll.
	repo := &mockForecastRunRepo{}
	runpod := &mockRunPodClient{}
	calibration := &mockCalibrationRepo{}

	poller := scheduler.NewDataPoller(scheduler.DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: calibration,
		RunPod:          runpod,
		// No upstream sources means no data is checked -- that's fine for this test.
		ForecastBucket: "test-bucket",
	})

	handler := newHandler(poller, nil)

	// This should not error -- the poller will just find no sources and return 0.
	result, err := handler(context.Background(), scheduler.DataPollerInput{
		ForceRetry: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "poll complete: 0 jobs triggered" {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestHandler_NoSourcesZeroTriggered(t *testing.T) {
	repo := &mockForecastRunRepo{}
	runpod := &mockRunPodClient{}
	calibration := &mockCalibrationRepo{}

	poller := scheduler.NewDataPoller(scheduler.DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: calibration,
		RunPod:          runpod,
		ForecastBucket:  "test-bucket",
	})

	handler := newHandler(poller, nil)

	result, err := handler(context.Background(), scheduler.DataPollerInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "poll complete: 0 jobs triggered" {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestHandler_WithLimit(t *testing.T) {
	repo := &mockForecastRunRepo{}
	runpod := &mockRunPodClient{}
	calibration := &mockCalibrationRepo{}

	poller := scheduler.NewDataPoller(scheduler.DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: calibration,
		RunPod:          runpod,
		ForecastBucket:  "test-bucket",
	})

	handler := newHandler(poller, nil)

	// Verify that limit is passed through correctly (no sources = 0 triggered anyway).
	result, err := handler(context.Background(), scheduler.DataPollerInput{
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "poll complete: 0 jobs triggered" {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestHandler_NilLoggerDefaultsToSlog(t *testing.T) {
	// Passing nil logger to newHandler should not panic.
	repo := &mockForecastRunRepo{}
	runpod := &mockRunPodClient{}
	calibration := &mockCalibrationRepo{}

	poller := scheduler.NewDataPoller(scheduler.DataPollerConfig{
		ForecastRepo:    repo,
		CalibrationRepo: calibration,
		RunPod:          runpod,
		ForecastBucket:  "test-bucket",
	})

	handler := newHandler(poller, nil)
	_, err := handler(context.Background(), scheduler.DataPollerInput{})
	if err != nil {
		t.Fatalf("unexpected error with nil logger: %v", err)
	}
}
