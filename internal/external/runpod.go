package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"watchpoint/internal/types"
)

// runPodAPIBase is the default RunPod Serverless API base URL.
// Overridable in tests via RunPodClientConfig.BaseURL.
const runPodAPIBase = "https://api.runpod.ai"

// RunPodClientConfig holds the configuration for creating a RunPodHTTPClient.
type RunPodClientConfig struct {
	APIKey     string
	EndpointID string
	BaseURL    string // Override for testing; defaults to runPodAPIBase
	Logger     *slog.Logger
}

// runPodRequest is the outer envelope sent to the RunPod /run endpoint.
// RunPod expects the payload under an "input" key.
type runPodRequest struct {
	Input *types.InferencePayload `json:"input"`
}

// runPodRunResponse is the response from the RunPod /run endpoint.
type runPodRunResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// runPodCancelResponse is the response from the RunPod /cancel endpoint.
type runPodCancelResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// RunPodHTTPClient implements RunPodClient by making direct HTTP calls to the
// RunPod Serverless REST API through BaseClient. This approach routes all
// requests through the platform's resilience infrastructure (circuit breaker,
// retries, error mapping) and makes testing with httptest straightforward.
type RunPodHTTPClient struct {
	base       *BaseClient
	apiKey     string
	endpointID string
	baseURL    string
	logger     *slog.Logger
}

// NewRunPodClient creates a new RunPodHTTPClient. The httpClient timeout should
// be set appropriately for the RunPod API (e.g., 30 seconds).
func NewRunPodClient(
	httpClient *http.Client,
	cfg RunPodClientConfig,
) *RunPodHTTPClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = runPodAPIBase
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	base := NewBaseClient(
		httpClient,
		"runpod",
		RetryPolicy{
			MaxRetries: 2,
			MinWait:    1 * time.Second,
			MaxWait:    10 * time.Second,
		},
		"WatchPoint/1.0",
	)

	return &RunPodHTTPClient{
		base:       base,
		apiKey:     cfg.APIKey,
		endpointID: cfg.EndpointID,
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		logger:     logger,
	}
}

// NewRunPodClientWithBase creates a RunPodHTTPClient with a pre-configured
// BaseClient. This is useful for testing when you want to control the
// BaseClient configuration (e.g., disable retries).
func NewRunPodClientWithBase(
	base *BaseClient,
	cfg RunPodClientConfig,
) *RunPodHTTPClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = runPodAPIBase
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &RunPodHTTPClient{
		base:       base,
		apiKey:     cfg.APIKey,
		endpointID: cfg.EndpointID,
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		logger:     logger,
	}
}

// TriggerInference calls the RunPod API to start an inference job.
// It serializes the InferencePayload as the "input" field of the RunPod request
// envelope and POSTs to /v2/{endpoint_id}/run.
// Returns the RunPod job ID on success.
func (c *RunPodHTTPClient) TriggerInference(ctx context.Context, payload types.InferencePayload) (string, error) {
	// Wrap the payload in the RunPod request envelope.
	reqBody := runPodRequest{
		Input: &payload,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to serialize inference payload",
			err,
		)
	}

	url := fmt.Sprintf("%s/v2/%s/run", c.baseURL, c.endpointID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to create RunPod trigger request",
			err,
		)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	c.logger.InfoContext(ctx, "triggering RunPod inference",
		"endpoint_id", c.endpointID,
		"task_type", string(payload.TaskType),
		"model", string(payload.Model),
		"run_timestamp", payload.RunTimestamp.Format(time.RFC3339),
	)

	resp, err := c.base.Do(req)
	if err != nil {
		return "", c.wrapError("TriggerInference", err)
	}
	defer resp.Body.Close()

	// Handle non-2xx responses that made it past the BaseClient retry logic.
	// BaseClient returns 4xx responses (other than 429) as-is without error.
	if resp.StatusCode >= 400 {
		return "", c.handleErrorResponse(resp, "TriggerInference")
	}

	var runResp runPodRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&runResp); err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode RunPod trigger response",
			err,
		)
	}

	if runResp.ID == "" {
		return "", types.NewAppError(
			types.ErrCodeUpstreamForecast,
			"RunPod returned empty job ID",
			nil,
		)
	}

	c.logger.InfoContext(ctx, "RunPod inference triggered successfully",
		"job_id", runResp.ID,
		"status", runResp.Status,
	)

	return runResp.ID, nil
}

// CancelJob terminates a running job on RunPod.
// It POSTs to /v2/{endpoint_id}/cancel/{job_id}.
func (c *RunPodHTTPClient) CancelJob(ctx context.Context, externalID string) error {
	if externalID == "" {
		return types.NewAppError(
			types.ErrCodeValidationMissingField,
			"external job ID is required for cancellation",
			nil,
		)
	}

	url := fmt.Sprintf("%s/v2/%s/cancel/%s", c.baseURL, c.endpointID, externalID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to create RunPod cancel request",
			err,
		)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	c.logger.InfoContext(ctx, "cancelling RunPod job",
		"endpoint_id", c.endpointID,
		"job_id", externalID,
	)

	resp, err := c.base.Do(req)
	if err != nil {
		return c.wrapError("CancelJob", err)
	}
	defer resp.Body.Close()

	// Handle non-2xx responses.
	if resp.StatusCode >= 400 {
		return c.handleErrorResponse(resp, "CancelJob")
	}

	c.logger.InfoContext(ctx, "RunPod job cancelled successfully",
		"job_id", externalID,
	)

	return nil
}

// GetJobStatus retrieves the current status of a RunPod job.
// It sends GET to /v2/{endpoint_id}/status/{job_id}.
func (c *RunPodHTTPClient) GetJobStatus(ctx context.Context, jobID string) (*RunPodJobStatus, error) {
	if jobID == "" {
		return nil, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"job ID is required for status check",
			nil,
		)
	}

	url := fmt.Sprintf("%s/v2/%s/status/%s", c.baseURL, c.endpointID, jobID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to create RunPod status request",
			err,
		)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	c.logger.InfoContext(ctx, "checking RunPod job status",
		"endpoint_id", c.endpointID,
		"job_id", jobID,
	)

	resp, err := c.base.Do(req)
	if err != nil {
		return nil, c.wrapError("GetJobStatus", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, c.handleErrorResponse(resp, "GetJobStatus")
	}

	var status RunPodJobStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode RunPod status response",
			err,
		)
	}

	c.logger.InfoContext(ctx, "RunPod job status retrieved",
		"job_id", status.ID,
		"status", status.Status,
	)

	return &status, nil
}

// handleErrorResponse reads and logs the error body from a non-2xx response,
// then returns an appropriate AppError.
func (c *RunPodHTTPClient) handleErrorResponse(resp *http.Response, operation string) *types.AppError {
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	bodyStr := string(bodyBytes)

	c.logger.Error("RunPod API error",
		"operation", operation,
		"status_code", resp.StatusCode,
		"response_body", bodyStr,
	)

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return types.NewAppError(
			types.ErrCodeUpstreamForecast,
			"RunPod authentication failed (401)",
			fmt.Errorf("RunPod %s returned 401: %s", operation, bodyStr),
		)
	case resp.StatusCode == http.StatusNotFound:
		return types.NewAppError(
			types.ErrCodeUpstreamForecast,
			fmt.Sprintf("RunPod resource not found (404): %s", operation),
			fmt.Errorf("RunPod %s returned 404: %s", operation, bodyStr),
		)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return types.NewAppError(
			types.ErrCodeUpstreamForecast,
			fmt.Sprintf("RunPod client error (%d): %s", resp.StatusCode, operation),
			fmt.Errorf("RunPod %s returned %d: %s", operation, resp.StatusCode, bodyStr),
		)
	default:
		return types.NewAppError(
			types.ErrCodeUpstreamForecast,
			fmt.Sprintf("RunPod server error (%d): %s", resp.StatusCode, operation),
			fmt.Errorf("RunPod %s returned %d: %s", operation, resp.StatusCode, bodyStr),
		)
	}
}

// wrapError converts errors from BaseClient.Do into domain-specific RunPod errors.
func (c *RunPodHTTPClient) wrapError(operation string, err error) error {
	// If it's already an AppError, enhance the message but preserve the code.
	var appErr *types.AppError
	if ok := isAppError(err, &appErr); ok {
		return types.NewAppError(
			appErr.Code,
			fmt.Sprintf("RunPod %s: %s", operation, appErr.Message),
			appErr.Err,
		)
	}

	return types.NewAppError(
		types.ErrCodeUpstreamForecast,
		fmt.Sprintf("RunPod %s failed", operation),
		err,
	)
}

// isAppError checks if err is an *types.AppError and extracts it.
func isAppError(err error, target **types.AppError) bool {
	var ae *types.AppError
	if ok := errors.As(err, &ae); ok {
		*target = ae
		return true
	}
	return false
}

// Compile-time interface compliance check.
var _ RunPodClient = (*RunPodHTTPClient)(nil)
