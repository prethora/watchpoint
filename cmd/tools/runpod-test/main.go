// Package main implements the runpod-test CLI tool for triggering RunPod
// inference jobs and polling for completion.
//
// Usage:
//
//	go run ./cmd/tools/runpod-test \
//	  --api-key=<key> --endpoint-id=<id> \
//	  --model=medium_range --mock \
//	  --bucket=watchpoint-forecasts-dev-XXXXX \
//	  --wait
//
// Environment variables (used as defaults when flags are not set):
//
//	RUNPOD_API_KEY      - RunPod API key
//	RUNPOD_ENDPOINT_ID  - RunPod serverless endpoint ID
//	FORECAST_BUCKET     - S3 bucket name for forecast output
//
// The tool constructs an InferencePayload, triggers a job via the RunPod API,
// and optionally polls for completion. On success it prints the S3 output path
// for use with scripts/validate-zarr.py.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"watchpoint/internal/external"
	"watchpoint/internal/types"
)

func main() {
	// Flags
	apiKey := flag.String("api-key", os.Getenv("RUNPOD_API_KEY"), "RunPod API key (or RUNPOD_API_KEY env)")
	endpointID := flag.String("endpoint-id", os.Getenv("RUNPOD_ENDPOINT_ID"), "RunPod endpoint ID (or RUNPOD_ENDPOINT_ID env)")
	bucket := flag.String("bucket", os.Getenv("FORECAST_BUCKET"), "S3 bucket name for forecast output (or FORECAST_BUCKET env)")
	model := flag.String("model", "medium_range", "Model type: medium_range or nowcast")
	timestamp := flag.String("timestamp", "", "Run timestamp (ISO 8601). Defaults to yesterday 06:00 UTC")
	mock := flag.Bool("mock", false, "Enable mock inference mode")
	wait := flag.Bool("wait", false, "Poll until job reaches terminal state")
	pollInterval := flag.Duration("poll-interval", 5*time.Second, "Polling interval when --wait is set")
	force := flag.Bool("force", false, "Set force_rebuild to skip idempotency check")

	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Validate required flags
	if *apiKey == "" {
		logger.Error("--api-key or RUNPOD_API_KEY is required")
		os.Exit(1)
	}
	if *endpointID == "" {
		logger.Error("--endpoint-id or RUNPOD_ENDPOINT_ID is required")
		os.Exit(1)
	}
	if *bucket == "" {
		logger.Error("--bucket or FORECAST_BUCKET is required")
		os.Exit(1)
	}

	// Parse model
	var forecastModel types.ForecastType
	switch *model {
	case "medium_range":
		forecastModel = types.ForecastMediumRange
	case "nowcast":
		forecastModel = types.ForecastNowcast
	default:
		logger.Error("unsupported model", "model", *model)
		os.Exit(1)
	}

	// Parse timestamp
	var runTS time.Time
	if *timestamp != "" {
		var err error
		runTS, err = time.Parse(time.RFC3339, *timestamp)
		if err != nil {
			logger.Error("invalid timestamp", "timestamp", *timestamp, "error", err)
			os.Exit(1)
		}
	} else {
		// Default: yesterday 06:00 UTC
		now := time.Now().UTC()
		yesterday := now.AddDate(0, 0, -1)
		runTS = time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 6, 0, 0, 0, time.UTC)
	}

	// Build output destination
	outputDest := fmt.Sprintf("s3://%s/%s/%s/", *bucket, *model, runTS.Format(time.RFC3339))

	// Build payload
	payload := types.InferencePayload{
		TaskType:          types.RunPodTaskInference,
		Model:             forecastModel,
		RunTimestamp:      runTS,
		OutputDestination: outputDest,
		Options: types.InferenceOptions{
			ForceRebuild:  *force,
			MockInference: *mock,
		},
	}

	logger.Info("triggering inference",
		"model", *model,
		"run_timestamp", runTS.Format(time.RFC3339),
		"output_destination", outputDest,
		"mock", *mock,
		"force_rebuild", *force,
	)

	// Create client
	httpClient := &http.Client{Timeout: 30 * time.Second}
	client := external.NewRunPodClient(httpClient, external.RunPodClientConfig{
		APIKey:     *apiKey,
		EndpointID: *endpointID,
		Logger:     logger,
	})

	// Set up context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Trigger the job
	jobID, err := client.TriggerInference(ctx, payload)
	if err != nil {
		logger.Error("failed to trigger inference", "error", err)
		os.Exit(1)
	}

	fmt.Printf("Job triggered: %s\n", jobID)
	fmt.Printf("Output path:   %s\n", outputDest)

	if !*wait {
		fmt.Println("\nUse --wait to poll for completion, or check status manually:")
		fmt.Printf("  curl -s -H 'Authorization: Bearer %s' https://api.runpod.ai/v2/%s/status/%s | jq .\n", *apiKey, *endpointID, jobID)
		return
	}

	// Poll for completion
	fmt.Printf("\nPolling every %s...\n", *pollInterval)
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nInterrupted.")
			os.Exit(1)
		case <-time.After(*pollInterval):
		}

		status, err := client.GetJobStatus(ctx, jobID)
		if err != nil {
			logger.Error("failed to get job status", "error", err)
			os.Exit(1)
		}

		fmt.Printf("  Status: %s", status.Status)
		if status.ExecutionTime > 0 {
			fmt.Printf(" (%.1fs)", status.ExecutionTime)
		}
		fmt.Println()

		switch status.Status {
		case "COMPLETED":
			fmt.Printf("\nJob completed successfully!\n")
			fmt.Printf("Output path: %s\n", outputDest)
			fmt.Printf("\nValidate with:\n")
			fmt.Printf("  python3 scripts/validate-zarr.py %s\n", outputDest)
			return
		case "FAILED":
			fmt.Printf("\nJob failed.\n")
			if status.Error != "" {
				fmt.Printf("Error: %s\n", status.Error)
			}
			os.Exit(1)
		case "CANCELLED":
			fmt.Printf("\nJob was cancelled.\n")
			os.Exit(1)
		case "TIMED_OUT":
			fmt.Printf("\nJob timed out.\n")
			os.Exit(1)
		}
		// IN_QUEUE, IN_PROGRESS — keep polling
	}
}
