package external

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// ---------------------------------------------------------------------------
// Helper: Create test RunPod client pointed at httptest server
// ---------------------------------------------------------------------------

func newTestRunPodClient(t *testing.T, serverURL string) *RunPodHTTPClient {
	t.Helper()
	base := NewBaseClient(
		&http.Client{Timeout: 5 * time.Second},
		"test-runpod",
		RetryPolicy{
			MaxRetries: 0, // No retries in tests for deterministic behavior
			MinWait:    1 * time.Millisecond,
			MaxWait:    10 * time.Millisecond,
		},
		"WatchPoint-Test/1.0",
		WithSleepFunc(noopSleep),
	)

	return NewRunPodClientWithBase(base, RunPodClientConfig{
		APIKey:     "test_runpod_api_key",
		EndpointID: "test_endpoint_123",
		BaseURL:    serverURL,
	})
}

// makeTestPayload creates a standard InferencePayload for testing.
func makeTestPayload() types.InferencePayload {
	return types.InferencePayload{
		TaskType:          types.RunPodTaskInference,
		Model:             types.ForecastMediumRange,
		RunTimestamp:      time.Date(2026, 1, 31, 6, 0, 0, 0, time.UTC),
		OutputDestination: "s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z",
		InputConfig: types.InferenceInputConfig{
			SourcePaths: []string{
				"s3://noaa-gfs/gfs.t06z.pgrb2.0p25.f000",
				"s3://noaa-gfs/gfs.t06z.pgrb2.0p25.f003",
			},
		},
		Options: types.InferenceOptions{
			ForceRebuild:  false,
			MockInference: false,
		},
	}
}

// makeNowcastPayload creates an InferencePayload with calibration data for testing.
func makeNowcastPayload() types.InferencePayload {
	return types.InferencePayload{
		TaskType:          types.RunPodTaskInference,
		Model:             types.ForecastNowcast,
		RunTimestamp:      time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC),
		OutputDestination: "s3://watchpoint-forecasts/nowcast/2026-01-31T12:00:00Z",
		InputConfig: types.InferenceInputConfig{
			Calibration: map[string]types.CalibrationCoefficients{
				"tile_01": {
					LocationID:    "tile_01",
					HighThreshold: 45.0,
					HighSlope:     0.8,
					HighBase:      60.0,
					MidThreshold:  25.0,
					MidSlope:      0.5,
					MidBase:       30.0,
					LowBase:       10.0,
					UpdatedAt:     time.Date(2026, 1, 30, 0, 0, 0, 0, time.UTC),
				},
			},
		},
		Options: types.InferenceOptions{
			ForceRebuild:  false,
			MockInference: false,
		},
	}
}

// makeVerificationPayload creates a verification-type InferencePayload for testing.
func makeVerificationPayload() types.InferencePayload {
	return types.InferencePayload{
		TaskType:          types.RunPodTaskVerification,
		Model:             types.ForecastMediumRange,
		RunTimestamp:      time.Date(2026, 1, 31, 6, 0, 0, 0, time.UTC),
		OutputDestination: "",
		InputConfig: types.InferenceInputConfig{
			VerificationWindow: &types.VerificationWindow{
				Start: time.Date(2026, 1, 29, 0, 0, 0, 0, time.UTC),
				End:   time.Date(2026, 1, 30, 0, 0, 0, 0, time.UTC),
			},
		},
	}
}

// ---------------------------------------------------------------------------
// TriggerInference Tests - Success Path
// ---------------------------------------------------------------------------

func TestRunPodTriggerInference_Success(t *testing.T) {
	var receivedBody runPodRequest
	var receivedAuth string
	var receivedContentType string
	var receivedMethod string
	var receivedPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		receivedContentType = r.Header.Get("Content-Type")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		if err := json.Unmarshal(body, &receivedBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(runPodRunResponse{
			ID:     "runpod_job_abc123",
			Status: "IN_QUEUE",
		})
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)
	payload := makeTestPayload()

	jobID, err := client.TriggerInference(context.Background(), payload)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify returned job ID.
	if jobID != "runpod_job_abc123" {
		t.Errorf("expected job ID runpod_job_abc123, got %s", jobID)
	}

	// Verify request method.
	if receivedMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", receivedMethod)
	}

	// Verify request path includes endpoint ID.
	expectedPath := "/v2/test_endpoint_123/run"
	if receivedPath != expectedPath {
		t.Errorf("expected path %s, got %s", expectedPath, receivedPath)
	}

	// Verify authorization header.
	if receivedAuth != "Bearer test_runpod_api_key" {
		t.Errorf("expected Bearer test_runpod_api_key, got %s", receivedAuth)
	}

	// Verify content type.
	if receivedContentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", receivedContentType)
	}

	// Verify the payload is wrapped under "input".
	if receivedBody.Input == nil {
		t.Fatal("expected payload under 'input' key, got nil")
	}

	// Verify payload fields.
	input := receivedBody.Input
	if input.TaskType != types.RunPodTaskInference {
		t.Errorf("expected task_type 'inference', got %s", input.TaskType)
	}
	if input.Model != types.ForecastMediumRange {
		t.Errorf("expected model 'medium_range', got %s", input.Model)
	}
	if !input.RunTimestamp.Equal(time.Date(2026, 1, 31, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("expected run_timestamp 2026-01-31T06:00:00Z, got %s", input.RunTimestamp)
	}
	if input.OutputDestination != "s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z" {
		t.Errorf("expected output_destination s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z, got %s", input.OutputDestination)
	}
	if len(input.InputConfig.SourcePaths) != 2 {
		t.Errorf("expected 2 source paths, got %d", len(input.InputConfig.SourcePaths))
	}
}

// TestRunPodTriggerInference_NowcastWithCalibration verifies that Nowcast payloads
// with calibration data are serialized correctly.
func TestRunPodTriggerInference_NowcastWithCalibration(t *testing.T) {
	var receivedBody runPodRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		if err := json.Unmarshal(body, &receivedBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(runPodRunResponse{
			ID:     "runpod_nowcast_456",
			Status: "IN_QUEUE",
		})
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)
	payload := makeNowcastPayload()

	jobID, err := client.TriggerInference(context.Background(), payload)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if jobID != "runpod_nowcast_456" {
		t.Errorf("expected job ID runpod_nowcast_456, got %s", jobID)
	}

	// Verify calibration data is present.
	input := receivedBody.Input
	if input == nil {
		t.Fatal("expected payload under 'input' key, got nil")
	}
	if input.Model != types.ForecastNowcast {
		t.Errorf("expected model 'nowcast', got %s", input.Model)
	}
	if len(input.InputConfig.Calibration) != 1 {
		t.Fatalf("expected 1 calibration entry, got %d", len(input.InputConfig.Calibration))
	}
	cal, ok := input.InputConfig.Calibration["tile_01"]
	if !ok {
		t.Fatal("expected calibration entry for tile_01")
	}
	if cal.HighThreshold != 45.0 {
		t.Errorf("expected high_threshold 45.0, got %f", cal.HighThreshold)
	}
	if cal.HighSlope != 0.8 {
		t.Errorf("expected high_slope 0.8, got %f", cal.HighSlope)
	}
}

// TestRunPodTriggerInference_VerificationTask verifies that verification payloads
// include the verification window.
func TestRunPodTriggerInference_VerificationTask(t *testing.T) {
	var receivedBody runPodRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		if err := json.Unmarshal(body, &receivedBody); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(runPodRunResponse{
			ID:     "runpod_verify_789",
			Status: "IN_QUEUE",
		})
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)
	payload := makeVerificationPayload()

	jobID, err := client.TriggerInference(context.Background(), payload)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if jobID != "runpod_verify_789" {
		t.Errorf("expected job ID runpod_verify_789, got %s", jobID)
	}

	input := receivedBody.Input
	if input == nil {
		t.Fatal("expected payload under 'input' key, got nil")
	}
	if input.TaskType != types.RunPodTaskVerification {
		t.Errorf("expected task_type 'verification', got %s", input.TaskType)
	}
	if input.InputConfig.VerificationWindow == nil {
		t.Fatal("expected verification_window to be present")
	}
	expectedStart := time.Date(2026, 1, 29, 0, 0, 0, 0, time.UTC)
	if !input.InputConfig.VerificationWindow.Start.Equal(expectedStart) {
		t.Errorf("expected verification_window.start %s, got %s", expectedStart, input.InputConfig.VerificationWindow.Start)
	}
}

// TestRunPodTriggerInference_JSONStructure verifies the exact JSON structure
// matches the contract in 11-runpod.md Section 2.1.
func TestRunPodTriggerInference_JSONStructure(t *testing.T) {
	var receivedRawJSON map[string]json.RawMessage

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		if err := json.Unmarshal(body, &receivedRawJSON); err != nil {
			t.Fatalf("failed to decode request body as raw JSON: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(runPodRunResponse{
			ID:     "runpod_json_check",
			Status: "IN_QUEUE",
		})
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)
	payload := makeTestPayload()

	_, err := client.TriggerInference(context.Background(), payload)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify "input" is the top-level key (RunPod envelope).
	if _, ok := receivedRawJSON["input"]; !ok {
		t.Fatal("expected 'input' key at top level of JSON body")
	}

	// Parse the input object to verify field names match the contract.
	var inputMap map[string]json.RawMessage
	if err := json.Unmarshal(receivedRawJSON["input"], &inputMap); err != nil {
		t.Fatalf("failed to parse input object: %v", err)
	}

	// Verify all expected keys are present.
	expectedKeys := []string{"task_type", "model", "run_timestamp", "output_destination", "input_config", "options"}
	for _, key := range expectedKeys {
		if _, ok := inputMap[key]; !ok {
			t.Errorf("expected key '%s' in input object, not found", key)
		}
	}

	// Verify task_type value.
	var taskType string
	if err := json.Unmarshal(inputMap["task_type"], &taskType); err != nil {
		t.Fatalf("failed to parse task_type: %v", err)
	}
	if taskType != "inference" {
		t.Errorf("expected task_type 'inference', got '%s'", taskType)
	}

	// Verify model value.
	var model string
	if err := json.Unmarshal(inputMap["model"], &model); err != nil {
		t.Fatalf("failed to parse model: %v", err)
	}
	if model != "medium_range" {
		t.Errorf("expected model 'medium_range', got '%s'", model)
	}

	// Verify input_config has gfs_source_paths.
	var inputConfig map[string]json.RawMessage
	if err := json.Unmarshal(inputMap["input_config"], &inputConfig); err != nil {
		t.Fatalf("failed to parse input_config: %v", err)
	}
	if _, ok := inputConfig["gfs_source_paths"]; !ok {
		t.Error("expected 'gfs_source_paths' in input_config")
	}

	// Verify options has expected keys.
	var options map[string]json.RawMessage
	if err := json.Unmarshal(inputMap["options"], &options); err != nil {
		t.Fatalf("failed to parse options: %v", err)
	}
	if _, ok := options["force_rebuild"]; !ok {
		t.Error("expected 'force_rebuild' in options")
	}
	if _, ok := options["mock_inference"]; !ok {
		t.Error("expected 'mock_inference' in options")
	}
}

// ---------------------------------------------------------------------------
// TriggerInference Tests - Error Paths
// ---------------------------------------------------------------------------

func TestRunPodTriggerInference_EmptyJobID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(runPodRunResponse{
			ID:     "",
			Status: "IN_QUEUE",
		})
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	_, err := client.TriggerInference(context.Background(), makeTestPayload())
	if err == nil {
		t.Fatal("expected error for empty job ID, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeUpstreamForecast {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamForecast, appErr.Code)
	}
}

func TestRunPodTriggerInference_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "Invalid API key"}`))
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	_, err := client.TriggerInference(context.Background(), makeTestPayload())
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeUpstreamForecast {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamForecast, appErr.Code)
	}
}

func TestRunPodTriggerInference_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	_, err := client.TriggerInference(context.Background(), makeTestPayload())
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}

	// BaseClient maps 5xx errors to AppError after retries are exhausted.
	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
}

func TestRunPodTriggerInference_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": "endpoint not found"}`))
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	_, err := client.TriggerInference(context.Background(), makeTestPayload())
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeUpstreamForecast {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamForecast, appErr.Code)
	}
}

// ---------------------------------------------------------------------------
// CancelJob Tests - Success Path
// ---------------------------------------------------------------------------

func TestRunPodCancelJob_Success(t *testing.T) {
	var receivedMethod string
	var receivedPath string
	var receivedAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(runPodCancelResponse{
			ID:     "runpod_job_abc123",
			Status: "CANCELLED",
		})
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	err := client.CancelJob(context.Background(), "runpod_job_abc123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify request method.
	if receivedMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", receivedMethod)
	}

	// Verify request path includes endpoint ID and job ID.
	expectedPath := "/v2/test_endpoint_123/cancel/runpod_job_abc123"
	if receivedPath != expectedPath {
		t.Errorf("expected path %s, got %s", expectedPath, receivedPath)
	}

	// Verify authorization header.
	if receivedAuth != "Bearer test_runpod_api_key" {
		t.Errorf("expected Bearer test_runpod_api_key, got %s", receivedAuth)
	}
}

// ---------------------------------------------------------------------------
// CancelJob Tests - Error Paths
// ---------------------------------------------------------------------------

func TestRunPodCancelJob_EmptyExternalID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called with empty external ID")
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	err := client.CancelJob(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty external ID, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeValidationMissingField {
		t.Errorf("expected error code %s, got %s", types.ErrCodeValidationMissingField, appErr.Code)
	}
}

func TestRunPodCancelJob_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	err := client.CancelJob(context.Background(), "runpod_job_abc123")
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
}

func TestRunPodCancelJob_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": "job not found"}`))
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	err := client.CancelJob(context.Background(), "nonexistent_job")
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeUpstreamForecast {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamForecast, appErr.Code)
	}
}

// ---------------------------------------------------------------------------
// GetJobStatus Tests - Success Path
// ---------------------------------------------------------------------------

func TestRunPodGetJobStatus_Success(t *testing.T) {
	var receivedMethod string
	var receivedPath string
	var receivedAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(RunPodJobStatus{
			ID:            "runpod_job_abc123",
			Status:        "COMPLETED",
			Output:        map[string]any{"status": "success", "output_path": "s3://bucket/path"},
			ExecutionTime: 42.5,
		})
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	status, err := client.GetJobStatus(context.Background(), "runpod_job_abc123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if receivedMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", receivedMethod)
	}

	expectedPath := "/v2/test_endpoint_123/status/runpod_job_abc123"
	if receivedPath != expectedPath {
		t.Errorf("expected path %s, got %s", expectedPath, receivedPath)
	}

	if receivedAuth != "Bearer test_runpod_api_key" {
		t.Errorf("expected Bearer test_runpod_api_key, got %s", receivedAuth)
	}

	if status.ID != "runpod_job_abc123" {
		t.Errorf("expected ID runpod_job_abc123, got %s", status.ID)
	}
	if status.Status != "COMPLETED" {
		t.Errorf("expected status COMPLETED, got %s", status.Status)
	}
	if status.ExecutionTime != 42.5 {
		t.Errorf("expected execution time 42.5, got %f", status.ExecutionTime)
	}
}

func TestRunPodGetJobStatus_InProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(RunPodJobStatus{
			ID:     "runpod_job_pending",
			Status: "IN_PROGRESS",
		})
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	status, err := client.GetJobStatus(context.Background(), "runpod_job_pending")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if status.Status != "IN_PROGRESS" {
		t.Errorf("expected status IN_PROGRESS, got %s", status.Status)
	}
}

// ---------------------------------------------------------------------------
// GetJobStatus Tests - Error Paths
// ---------------------------------------------------------------------------

func TestRunPodGetJobStatus_EmptyJobID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called with empty job ID")
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	_, err := client.GetJobStatus(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty job ID, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeValidationMissingField {
		t.Errorf("expected error code %s, got %s", types.ErrCodeValidationMissingField, appErr.Code)
	}
}

func TestRunPodGetJobStatus_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": "job not found"}`))
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	_, err := client.GetJobStatus(context.Background(), "nonexistent_job")
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeUpstreamForecast {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamForecast, appErr.Code)
	}
}

func TestRunPodGetJobStatus_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	_, err := client.GetJobStatus(context.Background(), "runpod_job_abc123")
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
}

func TestRunPodGetJobStatus_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "Invalid API key"}`))
	}))
	defer server.Close()

	client := newTestRunPodClient(t, server.URL)

	_, err := client.GetJobStatus(context.Background(), "runpod_job_abc123")
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeUpstreamForecast {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamForecast, appErr.Code)
	}
}

// ---------------------------------------------------------------------------
// Stub Tests
// ---------------------------------------------------------------------------

func TestStubRunPodClient_TriggerInference(t *testing.T) {
	stub := NewStubRunPodClient(discardLogger())
	payload := makeTestPayload()

	jobID, err := stub.TriggerInference(context.Background(), payload)
	if err != nil {
		t.Fatalf("expected no error from stub, got: %v", err)
	}

	expected := "stub_job_inference_medium_range"
	if jobID != expected {
		t.Errorf("expected stub job ID %s, got %s", expected, jobID)
	}
}

func TestStubRunPodClient_CancelJob(t *testing.T) {
	stub := NewStubRunPodClient(discardLogger())

	err := stub.CancelJob(context.Background(), "some_job_id")
	if err != nil {
		t.Fatalf("expected no error from stub, got: %v", err)
	}
}

func TestStubRunPodClient_GetJobStatus(t *testing.T) {
	stub := NewStubRunPodClient(discardLogger())

	status, err := stub.GetJobStatus(context.Background(), "some_job_id")
	if err != nil {
		t.Fatalf("expected no error from stub, got: %v", err)
	}
	if status.ID != "some_job_id" {
		t.Errorf("expected ID some_job_id, got %s", status.ID)
	}
	if status.Status != "COMPLETED" {
		t.Errorf("expected status COMPLETED, got %s", status.Status)
	}
}

// ---------------------------------------------------------------------------
// Interface Compliance
// ---------------------------------------------------------------------------

func TestRunPodHTTPClient_ImplementsInterface(t *testing.T) {
	var _ RunPodClient = (*RunPodHTTPClient)(nil)
}

func TestStubRunPodClient_ImplementsInterface(t *testing.T) {
	var _ RunPodClient = (*StubRunPodClient)(nil)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// discardLogger returns a logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.Default()
}
