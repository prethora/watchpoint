// Package main is the entrypoint for the Data Poller Lambda function.
//
// The Data Poller runs every 15 minutes via an EventBridge rule. It checks
// upstream NOAA S3 buckets for new forecast model runs and triggers inference
// jobs on RunPod for any new data found. It does NOT download the data itself;
// it passes references (timestamps and S3 paths) to RunPod for processing.
//
// This file handles dependency wiring (Cold Start) and delegates all business
// logic to the internal/scheduler package (DataPoller.Poll).
//
// Architecture reference: architecture/09-scheduled-jobs.md Section 5
// SAM template reference: architecture/04-sam-template.md
// Flow reference: FCST-003
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"

	"watchpoint/internal/config"
	"watchpoint/internal/db"
	"watchpoint/internal/external"
	"watchpoint/internal/forecasts"
	"watchpoint/internal/scheduler"
)

func main() {
	// Initialize structured logger at startup.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	logger.Info("DataPoller Lambda initializing (cold start)")

	// Resolve SSM secrets into environment variables before reading config.
	// In non-local environments, DATABASE_URL and RUNPOD_API_KEY are stored in
	// SSM Parameter Store and referenced via _SSM_PARAM suffix variables.
	if err := config.ResolveSecrets(config.NewSSMProvider(os.Getenv("AWS_REGION"))); err != nil {
		logger.Error("Failed to resolve SSM secrets", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// Load AWS SDK configuration.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("failed to load AWS SDK config", "error", err)
		os.Exit(1)
	}

	// Read configuration from environment variables.
	// The SAM template (04-sam-template.md) injects these via Lambda environment.
	databaseURL := os.Getenv("DATABASE_URL")
	forecastBucket := os.Getenv("FORECAST_BUCKET")
	runpodAPIKey := os.Getenv("RUNPOD_API_KEY")
	runpodEndpointID := os.Getenv("RUNPOD_ENDPOINT_ID")

	// GOES and MRMS bucket names for nowcast pipeline.
	goesBucket := os.Getenv("GOES_BUCKET")
	if goesBucket == "" {
		goesBucket = "noaa-goes16"
	}
	mrmsBucket := os.Getenv("MRMS_BUCKET")
	if mrmsBucket == "" {
		mrmsBucket = "noaa-mrms-pds"
	}

	// Upstream mirrors from config (comma-separated list of GFS mirror bucket names).
	// Per architecture/09-scheduled-jobs.md Section 5.2 step 2: Mirror Failover.
	upstreamMirrors := parseUpstreamMirrors(os.Getenv("UPSTREAM_MIRRORS"))
	if len(upstreamMirrors) == 0 {
		upstreamMirrors = []string{"noaa-gfs-bdp-pds", "aws-noaa-gfs"}
	}

	// Initialize database connection pool.
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		logger.Error("failed to create database pool", "error", err)
		os.Exit(1)
	}

	// Verify database connectivity.
	if err := pool.Ping(ctx); err != nil {
		logger.Error("failed to ping database", "error", err)
		os.Exit(1)
	}

	// Initialize S3 client for upstream source checking.
	s3Client := s3.NewFromConfig(awsCfg)

	// Initialize upstream sources.
	gfsSource := forecasts.NewGFSSource(s3Client, upstreamMirrors, logger)
	goesSource := forecasts.NewGOESSource(s3Client, goesBucket, logger)
	mrmsSource := forecasts.NewMRMSSource(s3Client, mrmsBucket, logger)

	// Initialize RunPod client.
	httpClient := &http.Client{Timeout: 30 * time.Second}
	runpodClient := external.NewRunPodClient(httpClient, external.RunPodClientConfig{
		APIKey:     runpodAPIKey,
		EndpointID: runpodEndpointID,
		Logger:     logger,
	})

	// Initialize database repositories.
	forecastRepo := db.NewForecastRunRepository(pool)
	calibrationRepo := db.NewCalibrationRepository(pool)

	// Create the DataPoller service.
	poller := scheduler.NewDataPoller(scheduler.DataPollerConfig{
		ForecastRepo:    forecastRepo,
		CalibrationRepo: calibrationRepo,
		RunPod:          runpodClient,
		GFSSource:       gfsSource,
		GOESSource:      goesSource,
		MRMSSource:      mrmsSource,
		ForecastBucket:  forecastBucket,
		Logger:          logger,
	})

	logger.Info("DataPoller Lambda initialized",
		"forecast_bucket", forecastBucket,
		"runpod_endpoint_id", runpodEndpointID,
		"upstream_mirrors", upstreamMirrors,
		"goes_bucket", goesBucket,
		"mrms_bucket", mrmsBucket,
	)

	// Create the Lambda handler function.
	handler := newHandler(poller, logger)

	lambda.Start(handler)
}

// newHandler creates the Lambda handler function that processes DataPollerInput
// events. It wraps the DataPoller.Poll method with input parsing and error handling.
//
// Per architecture/09-scheduled-jobs.md Section 5:
//   - ForceRetry in the input overrides the since-time to look further back.
//   - BackfillHours shifts the since-time by the specified number of hours.
//   - Limit caps the number of RunPod triggers per execution.
func newHandler(poller *scheduler.DataPoller, logger *slog.Logger) func(ctx context.Context, input scheduler.DataPollerInput) (string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, input scheduler.DataPollerInput) (string, error) {
		logger.InfoContext(ctx, "DataPoller handler invoked",
			"force_retry", input.ForceRetry,
			"backfill_hours", input.BackfillHours,
			"limit", input.Limit,
		)

		// If ForceRetry is set but no BackfillHours specified, default to 24 hours.
		// This provides a reasonable lookback window for disaster recovery scenarios
		// without requiring the operator to specify the exact number of hours.
		if input.ForceRetry && input.BackfillHours == 0 {
			input.BackfillHours = 24
			logger.InfoContext(ctx, "ForceRetry enabled, defaulting BackfillHours to 24")
		}

		triggered, err := poller.Poll(ctx, input)
		if err != nil {
			logger.ErrorContext(ctx, "DataPoller poll failed",
				"error", err,
				"triggered_before_error", triggered,
			)
			return "", fmt.Errorf("data poller failed: %w", err)
		}

		result := fmt.Sprintf("poll complete: %d jobs triggered", triggered)
		logger.InfoContext(ctx, result,
			"triggered", triggered,
		)

		return result, nil
	}
}

// parseUpstreamMirrors splits a comma-separated list of upstream mirror bucket
// names into a string slice. Whitespace around entries is trimmed and empty
// entries are skipped.
func parseUpstreamMirrors(raw string) []string {
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	var mirrors []string
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			mirrors = append(mirrors, trimmed)
		}
	}
	return mirrors
}
