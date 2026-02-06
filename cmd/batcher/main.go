// Package main is the entrypoint for the Batcher Lambda function.
//
// The Batcher is triggered by S3 ObjectCreated events (filtered to _SUCCESS
// suffix) when new forecast data lands in the ForecastBucket. It parses the
// S3 event, groups active WatchPoints by geographic tile, and dispatches
// paginated evaluation jobs to SQS.
//
// This file handles dependency wiring (Cold Start) and delegates all business
// logic to the internal/batcher package.
//
// Architecture reference: architecture/06-batcher.md
// SAM template reference: architecture/04-sam-template.md Section 5.1
// Flow reference: EVAL-001, OBS-001
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwTypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqsTypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"watchpoint/internal/batcher"
	"watchpoint/internal/types"
)

// --- SQS Client Implementation ---

// sqsAPI is the subset of the SQS SDK client used by the batcher.
type sqsAPI interface {
	SendMessageBatch(ctx context.Context, params *sqs.SendMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
}

// liveSQSClient is the production implementation of batcher.SQSClient.
// It chunks messages into groups of 10 per SQS API call (the SQS maximum)
// and respects context cancellation for clean Lambda timeout handling.
//
// Architecture reference: architecture/06-batcher.md Section 4.1
type liveSQSClient struct {
	client sqsAPI
}

// SendBatch chunks messages into groups of 10 and sends them via the SQS
// SendMessageBatch API. Each message is serialized to JSON.
// Must respect ctx.Done() to abort cleanly on Lambda timeout.
func (s *liveSQSClient) SendBatch(ctx context.Context, queueURL string, messages []types.EvalMessage) error {
	const maxBatchSize = 10

	for i := 0; i < len(messages); i += maxBatchSize {
		// Check context before each batch
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during SQS send: %w", ctx.Err())
		default:
		}

		end := i + maxBatchSize
		if end > len(messages) {
			end = len(messages)
		}

		chunk := messages[i:end]
		entries := make([]sqsTypes.SendMessageBatchRequestEntry, len(chunk))

		for j, msg := range chunk {
			body, err := json.Marshal(msg)
			if err != nil {
				return fmt.Errorf("failed to marshal EvalMessage: %w", err)
			}
			entries[j] = sqsTypes.SendMessageBatchRequestEntry{
				Id:          aws.String(fmt.Sprintf("msg-%d", i+j)),
				MessageBody: aws.String(string(body)),
			}
		}

		output, err := s.client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
			QueueUrl: aws.String(queueURL),
			Entries:  entries,
		})
		if err != nil {
			return fmt.Errorf("SQS SendMessageBatch failed: %w", err)
		}

		// Check for partial failures
		if len(output.Failed) > 0 {
			return fmt.Errorf("SQS SendMessageBatch had %d failures, first: code=%s, message=%s",
				len(output.Failed),
				aws.ToString(output.Failed[0].Code),
				aws.ToString(output.Failed[0].Message),
			)
		}
	}

	return nil
}

// --- Metric Publisher Implementation ---

// cloudwatchAPI is the subset of the CloudWatch SDK client used by the batcher.
type cloudwatchAPI interface {
	PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

// liveMetricPublisher is the production implementation of batcher.MetricPublisher.
// It publishes metrics to CloudWatch under the configured namespace.
//
// Architecture reference: architecture/06-batcher.md Section 4.2
type liveMetricPublisher struct {
	client    cloudwatchAPI
	namespace string
}

// PublishForecastReady emits "ForecastReady=1" metric to CloudWatch.
// This serves as the heartbeat for the Dead Man's Switch alarm (OBS-001/002).
// The metric is dimensioned by ForecastType to enable separate alarms for
// medium_range (8h threshold) and nowcast (30m threshold).
func (p *liveMetricPublisher) PublishForecastReady(ctx context.Context, ft types.ForecastType) error {
	_, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(p.namespace),
		MetricData: []cwTypes.MetricDatum{
			{
				MetricName: aws.String("ForecastReady"),
				Value:      aws.Float64(1),
				Unit:       cwTypes.StandardUnitCount,
				Dimensions: []cwTypes.Dimension{
					{
						Name:  aws.String("ForecastType"),
						Value: aws.String(string(ft)),
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to publish ForecastReady metric: %w", err)
	}
	return nil
}

// PublishStats emits "BatchSizeTiles" and "BatchSizeWatchPoints" metrics
// to CloudWatch, dimensioned by ForecastType.
func (p *liveMetricPublisher) PublishStats(ctx context.Context, ft types.ForecastType, tiles int, watchpoints int) error {
	dims := []cwTypes.Dimension{
		{
			Name:  aws.String("ForecastType"),
			Value: aws.String(string(ft)),
		},
	}

	_, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(p.namespace),
		MetricData: []cwTypes.MetricDatum{
			{
				MetricName: aws.String("BatchSizeTiles"),
				Value:      aws.Float64(float64(tiles)),
				Unit:       cwTypes.StandardUnitCount,
				Dimensions: dims,
			},
			{
				MetricName: aws.String("BatchSizeWatchPoints"),
				Value:      aws.Float64(float64(watchpoints)),
				Unit:       cwTypes.StandardUnitCount,
				Dimensions: dims,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to publish batch stats metrics: %w", err)
	}
	return nil
}

func main() {
	// Initialize structured logger at startup.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	logger.Info("Batcher Lambda initializing (cold start)")

	// Load AWS SDK configuration.
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		logger.Error("Failed to load AWS SDK config", "error", err)
		os.Exit(1)
	}

	// Read configuration from environment variables.
	// The SAM template (04-sam-template.md) injects these via Lambda environment.
	forecastBucket := os.Getenv("FORECAST_BUCKET")
	urgentQueueURL := os.Getenv("SQS_EVAL_URGENT")
	standardQueueURL := os.Getenv("SQS_EVAL_STANDARD")
	metricNamespace := os.Getenv("METRIC_NAMESPACE")
	if metricNamespace == "" {
		metricNamespace = "WatchPoint"
	}

	// Initialize SQS client.
	sqsClient := sqs.NewFromConfig(awsCfg)

	// Initialize CloudWatch client.
	cwClient := cloudwatch.NewFromConfig(awsCfg)

	// Wire up the Batcher.
	// NOTE: Database repositories (Repo, WPRepo) require a database connection pool.
	// In the production Lambda, these would be initialized from the DATABASE_URL
	// environment variable during cold start. For now, they are left nil and will
	// be wired when the database connection infrastructure is integrated.
	b := &batcher.Batcher{
		Config: batcher.BatcherConfig{
			ForecastBucket:   forecastBucket,
			UrgentQueueURL:   urgentQueueURL,
			StandardQueueURL: standardQueueURL,
			MaxPageSize:      batcher.DefaultMaxPageSize,
			MaxTiles:         100,
		},
		Log: logger,
		// Repo and WPRepo are nil -- they require database pool initialization
		// which will be wired in a subsequent integration task.
		Repo:   nil,
		WPRepo: nil,
		SQS: &liveSQSClient{
			client: sqsClient,
		},
		Metrics: &liveMetricPublisher{
			client:    cwClient,
			namespace: metricNamespace,
		},
	}

	logger.Info("Batcher Lambda initialized",
		"forecast_bucket", forecastBucket,
		"urgent_queue", urgentQueueURL,
		"standard_queue", standardQueueURL,
		"metric_namespace", metricNamespace,
	)

	// Local mode: read JSON event from stdin instead of starting Lambda runtime.
	// This enables local integration testing without the AWS Lambda RIE.
	// Usage: echo '{"model":"medium_range","timestamp":"..."}' | go run cmd/batcher/main.go
	if os.Getenv("APP_ENV") == "local" {
		logger.Info("APP_ENV=local: reading event from stdin")
		payload, err := io.ReadAll(os.Stdin)
		if err != nil {
			logger.Error("Failed to read stdin", "error", err)
			os.Exit(1)
		}
		if len(payload) == 0 {
			logger.Error("No input received on stdin")
			os.Exit(1)
		}
		ctx := context.Background()
		if err := b.Handler(ctx, json.RawMessage(payload)); err != nil {
			logger.Error("Handler execution failed", "error", err)
			os.Exit(1)
		}
		logger.Info("Handler execution completed successfully")
		return
	}

	lambda.Start(b.Handler)
}
