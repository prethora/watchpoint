// Package batcher implements the core business logic for the Batcher Lambda function.
// The Batcher is the high-throughput bridge between the Forecast System (S3) and
// the Evaluation Engine (SQS). It groups WatchPoints by geographic tile and
// schedules paginated evaluation jobs.
//
// Architecture reference: architecture/06-batcher.md Section 5
// Flow coverage: EVAL-001, EVAL-002, IMPL-003, OBS-001
package batcher

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"watchpoint/internal/db"
	"watchpoint/internal/types"
)

// DefaultMaxPageSize is the maximum number of WatchPoints per evaluation message.
// Tiles exceeding this threshold are split into multiple pages (Hot Tile Pagination).
// See architecture/06-batcher.md Section 1 and flow EVAL-002.
const DefaultMaxPageSize = 500

// SQSClient abstracts the SQS batch sending logic, chunking messages into
// groups of 10 per API call. Implementations must respect ctx.Done() to abort
// cleanly on Lambda timeout.
//
// Architecture reference: architecture/06-batcher.md Section 4.1
type SQSClient interface {
	// SendBatch chunks messages into groups of 10 and sends them.
	// Must respect ctx.Done() to abort cleanly on timeout.
	SendBatch(ctx context.Context, queueURL string, messages []types.EvalMessage) error
}

// MetricPublisher handles high-cardinality and heartbeat metrics.
//
// Architecture reference: architecture/06-batcher.md Section 4.2
type MetricPublisher interface {
	// PublishForecastReady emits "ForecastReady=1" (Dead Man's Switch heartbeat).
	PublishForecastReady(ctx context.Context, ft types.ForecastType) error

	// PublishStats emits "BatchSizeTiles" and "BatchSizeWatchPoints" metrics.
	PublishStats(ctx context.Context, ft types.ForecastType, tiles int, watchpoints int) error
}

// BatcherConfig holds the configuration for the Batcher Lambda.
// Mapped from architecture/06-batcher.md Section 3.1 and 03-config.md.
type BatcherConfig struct {
	// S3 Validation
	ForecastBucket string // Strictly validate input event bucket

	// Queue Routing
	UrgentQueueURL   string // For Nowcast
	StandardQueueURL string // For Medium-Range

	// Tunables
	MaxPageSize int // Default: 500
	MaxTiles    int // Safety guardrail (Default: 100)
}

// RunContext encapsulates the metadata extracted from the S3 trigger event.
// Architecture reference: architecture/06-batcher.md Section 3.2
type RunContext struct {
	Model     types.ForecastType
	Timestamp time.Time
	Bucket    string
	Key       string
}

// Batcher orchestrates the core business logic for translating a "Forecast Ready"
// signal into paginated evaluation jobs dispatched to SQS.
//
// Architecture reference: architecture/06-batcher.md Section 5.1
type Batcher struct {
	Config  BatcherConfig
	Log     *slog.Logger
	Repo    *db.ForecastRunRepository
	WPRepo  *db.BatcherRepository
	SQS     SQSClient
	Metrics MetricPublisher
}

// ProcessRun is the core orchestration method. It validates the forecast run,
// groups active WatchPoints by tile, generates paginated EvalMessages, and
// dispatches them to SQS.
//
// The method implements the Phantom Run check (IMPL-003):
//  1. Queries forecast_runs for the specific model and run_timestamp.
//  2. If no record found: Phantom Forecast -- log CRITICAL and discard.
//  3. If status='complete': Duplicate S3 event -- log info and exit success.
//  4. If status='running': Normal completion -- update to 'complete' and proceed.
//
// Architecture reference: architecture/06-batcher.md Section 5.2
// Flow references: EVAL-001, IMPL-003
func (b *Batcher) ProcessRun(ctx context.Context, rc RunContext) error {
	// Step 1: Phantom Run Detection (IMPL-003)
	run, err := b.Repo.GetByTimestamp(ctx, rc.Model, rc.Timestamp)
	if err != nil {
		return fmt.Errorf("batcher: failed to query forecast run: %w", err)
	}

	if run == nil {
		// Case C: Phantom Forecast -- no record in DB
		b.Log.ErrorContext(ctx, "Phantom Forecast detected in S3",
			"model", string(rc.Model),
			"run_timestamp", rc.Timestamp.Format(time.RFC3339),
			"bucket", rc.Bucket,
			"key", rc.Key,
		)
		// Discard the event without processing. Do not auto-heal or create records.
		// The DataPoller is the sole authority for initiating forecast runs.
		return nil
	}

	switch run.Status {
	case "complete":
		// Case B: Duplicate S3 event -- already processed
		b.Log.InfoContext(ctx, "Duplicate S3 event, forecast run already complete",
			"run_id", run.ID,
			"model", string(rc.Model),
			"run_timestamp", rc.Timestamp.Format(time.RFC3339),
		)
		return nil

	case "running":
		// Case A: Normal completion -- update status
		// Step 2: Update forecast_runs SET status='complete', storage_path=...
		storagePath := rc.Key
		if err := b.Repo.MarkComplete(ctx, run.ID, storagePath, 0); err != nil {
			return fmt.Errorf("batcher: failed to mark forecast run complete: %w", err)
		}
		b.Log.InfoContext(ctx, "Forecast run marked complete",
			"run_id", run.ID,
			"model", string(rc.Model),
			"storage_path", storagePath,
		)

	default:
		// Unexpected status (e.g., "failed") -- log warning and proceed cautiously
		b.Log.WarnContext(ctx, "Forecast run has unexpected status, proceeding anyway",
			"run_id", run.ID,
			"status", run.Status,
			"model", string(rc.Model),
		)
	}

	// Step 3: Resolve target queue based on ForecastType
	queueURL, err := b.resolveQueue(rc.Model)
	if err != nil {
		return fmt.Errorf("batcher: %w", err)
	}

	// Step 4: Query active tile counts (Index-Only Scan)
	tileCounts, err := b.WPRepo.GetActiveTileCounts(ctx)
	if err != nil {
		return fmt.Errorf("batcher: failed to get active tile counts: %w", err)
	}

	// Calculate total watchpoints for metrics
	totalWatchpoints := 0
	for _, count := range tileCounts {
		totalWatchpoints += count
	}

	b.Log.InfoContext(ctx, "Active tiles queried",
		"model", string(rc.Model),
		"tile_count", len(tileCounts),
		"total_watchpoints", totalWatchpoints,
	)

	// Step 5-6: Generate paginated EvalMessages
	messages := b.generateMessages(tileCounts, rc)

	// Step 7: Send batches to SQS
	if len(messages) > 0 {
		if err := b.SQS.SendBatch(ctx, queueURL, messages); err != nil {
			return fmt.Errorf("batcher: failed to send eval messages to SQS: %w", err)
		}
		b.Log.InfoContext(ctx, "Eval messages dispatched",
			"model", string(rc.Model),
			"message_count", len(messages),
			"queue_url", queueURL,
		)
	}

	// Step 8: Emit metrics
	// Always emit ForecastReady even for zero state (Section 7.2 Zero State)
	if err := b.Metrics.PublishForecastReady(ctx, rc.Model); err != nil {
		b.Log.WarnContext(ctx, "Failed to publish ForecastReady metric",
			"error", err,
			"model", string(rc.Model),
		)
		// Non-fatal: do not fail the batch for a metric publish error
	}

	if err := b.Metrics.PublishStats(ctx, rc.Model, len(tileCounts), totalWatchpoints); err != nil {
		b.Log.WarnContext(ctx, "Failed to publish batch stats metric",
			"error", err,
			"model", string(rc.Model),
		)
		// Non-fatal
	}

	return nil
}

// generateMessages handles the Hot Tile Pagination logic (EVAL-002).
// For each tile, it calculates the number of pages needed based on MaxPageSize
// and creates an EvalMessage for each page.
//
// A single ephemeral BatchID and TraceID are generated for the entire execution
// and injected into every EvalMessage for distributed traceability.
//
// Logic per tile:
//
//	pages = ceil(count / MaxPageSize)
//	For p in 0..pages-1:
//	  Create EvalMessage with Page=p, PageSize=MaxPageSize, TotalItems=count
//
// Architecture reference: architecture/06-batcher.md Section 5.2
// Flow reference: EVAL-002
func (b *Batcher) generateMessages(tileCounts map[string]int, rc RunContext) []types.EvalMessage {
	if len(tileCounts) == 0 {
		return nil
	}

	pageSize := b.Config.MaxPageSize
	if pageSize <= 0 {
		pageSize = DefaultMaxPageSize
	}

	// Step 5: Generate ephemeral BatchID and TraceID for traceability
	batchID := uuid.New().String()
	traceID := uuid.New().String()

	var messages []types.EvalMessage

	for tileID, count := range tileCounts {
		if count <= 0 {
			continue
		}

		pages := int(math.Ceil(float64(count) / float64(pageSize)))

		for p := 0; p < pages; p++ {
			msg := types.EvalMessage{
				BatchID:      batchID,
				TraceID:      traceID,
				ForecastType: rc.Model,
				RunTimestamp: rc.Timestamp,
				TileID:       tileID,
				Page:         p,
				PageSize:     pageSize,
				TotalItems:   count,
				Action:       types.EvalActionEvaluate,
			}
			messages = append(messages, msg)
		}
	}

	return messages
}

// resolveQueue returns the SQS queue URL based on ForecastType.
// Nowcast routes to the Urgent queue; Medium-Range routes to Standard.
// Returns an error for unknown model types per architecture/06-batcher.md
// Section 7.2 (Unknown Models).
//
// Architecture reference: architecture/06-batcher.md Section 5.2
func (b *Batcher) resolveQueue(ft types.ForecastType) (string, error) {
	switch ft {
	case types.ForecastNowcast:
		return b.Config.UrgentQueueURL, nil
	case types.ForecastMediumRange:
		return b.Config.StandardQueueURL, nil
	default:
		return "", fmt.Errorf("unknown forecast model: %q", ft)
	}
}
