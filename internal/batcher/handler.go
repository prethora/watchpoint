// Package batcher - Lambda Handler and S3 event parsing.
//
// This file implements the AWS Lambda entrypoint for the Batcher function.
// It handles S3 event parsing, bucket validation, and S3 key parsing to
// extract forecast model and run timestamp metadata.
//
// Architecture reference: architecture/06-batcher.md Section 6
// Flow references: EVAL-001, OBS-001
package batcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"watchpoint/internal/types"
)

// ParseRunContext extracts forecast metadata from an S3 key.
// The expected format is: "forecasts/{model}/{timestamp}/_SUCCESS"
//
// The model must be a recognized ForecastType ("medium_range" or "nowcast").
// The timestamp must be in RFC3339 format (e.g., "2026-02-06T06:00:00Z").
//
// Returns an error for:
//   - Keys that do not match the expected format
//   - Unrecognized model strings
//   - Invalid timestamp formats
//
// Architecture reference: architecture/06-batcher.md Section 6
func ParseRunContext(key string) (types.ForecastType, time.Time, error) {
	// Expected format: forecasts/{model}/{timestamp}/_SUCCESS
	parts := strings.Split(key, "/")
	if len(parts) < 4 {
		return "", time.Time{}, fmt.Errorf("invalid S3 key format: expected at least 4 path segments, got %d: %q", len(parts), key)
	}

	if parts[0] != "forecasts" {
		return "", time.Time{}, fmt.Errorf("invalid S3 key prefix: expected 'forecasts', got %q in key %q", parts[0], key)
	}

	modelStr := parts[1]
	ft := types.ForecastType(modelStr)

	// Validate model is a known ForecastType
	switch ft {
	case types.ForecastMediumRange, types.ForecastNowcast:
		// Valid
	default:
		return "", time.Time{}, fmt.Errorf("unknown forecast model in S3 key: %q", modelStr)
	}

	tsStr := parts[2]
	ts, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("invalid timestamp in S3 key: %q: %w", tsStr, err)
	}

	return ft, ts, nil
}

// Handler is the Lambda entrypoint for the Batcher function.
// It supports two input formats:
//  1. S3 Event (standard trigger from S3 ObjectCreated)
//  2. Manual RunContext JSON (for recovery/re-processing)
//
// For S3 events, the handler:
//  1. Validates the bucket name against Config.ForecastBucket (security check)
//  2. Parses the S3 key to extract ForecastType and RunTimestamp
//  3. Calls ProcessRun with the extracted RunContext
//
// For manual RunContext, the handler:
//  1. Parses the JSON directly into a RunContext
//  2. Calls ProcessRun (bucket validation is skipped for manual triggers)
//
// Security: If the bucket name does not match the configured ForecastBucket,
// the event is logged as a critical error and returned nil (do not retry
// invalid inputs per Section 7.1).
//
// Architecture reference: architecture/06-batcher.md Section 6
// Flow reference: EVAL-001 Step 1
func (b *Batcher) Handler(ctx context.Context, payload json.RawMessage) error {
	// Step 1: Attempt to parse as S3Event
	var s3Event events.S3Event
	if err := json.Unmarshal(payload, &s3Event); err == nil && len(s3Event.Records) > 0 {
		return b.handleS3Event(ctx, s3Event)
	}

	// Step 2: Attempt to parse as manual RunContext
	var rc RunContext
	if err := json.Unmarshal(payload, &rc); err != nil {
		return fmt.Errorf("batcher: failed to parse payload as S3Event or RunContext: %w", err)
	}

	// Validate the manual RunContext has required fields
	if rc.Model == "" {
		return fmt.Errorf("batcher: manual RunContext missing required field 'model'")
	}
	if rc.Timestamp.IsZero() {
		return fmt.Errorf("batcher: manual RunContext missing required field 'timestamp'")
	}

	b.Log.InfoContext(ctx, "Processing manual RunContext",
		"model", string(rc.Model),
		"timestamp", rc.Timestamp.Format(time.RFC3339),
	)

	return b.ProcessRun(ctx, rc)
}

// handleS3Event processes a validated S3 event with one or more records.
// Per the SAM template (04-sam-template.md Section 5.1), the Batcher is
// triggered by s3:ObjectCreated:* events filtered to _SUCCESS suffix.
// In practice, each invocation carries a single record, but we process
// only the first record as each _SUCCESS file represents one forecast run.
func (b *Batcher) handleS3Event(ctx context.Context, s3Event events.S3Event) error {
	record := s3Event.Records[0]
	bucket := record.S3.Bucket.Name
	// S3 event notifications URL-encode the object key (e.g., ':' becomes '%3A').
	// The URLDecodedKey field provides the properly decoded key.
	// If URLDecodedKey is empty (e.g., older event format), fall back to the raw Key.
	key := record.S3.Object.URLDecodedKey
	if key == "" {
		key = record.S3.Object.Key
	}

	b.Log.InfoContext(ctx, "Processing S3 event",
		"bucket", bucket,
		"key", key,
	)

	// Step 3: Validate Bucket Name (Security)
	// If the bucket does not match the configured ForecastBucket, this is either
	// a misconfiguration or a cross-account event injection. Log critically and
	// return nil to prevent retries on invalid inputs.
	if bucket != b.Config.ForecastBucket {
		b.Log.ErrorContext(ctx, "S3 event from unexpected bucket, discarding",
			"expected_bucket", b.Config.ForecastBucket,
			"actual_bucket", bucket,
			"key", key,
		)
		return nil
	}

	// Step 4: Parse Key to extract Model and Timestamp
	model, ts, err := ParseRunContext(key)
	if err != nil {
		// Unknown models or invalid key formats return a hard error to trigger
		// LambdaErrors alarms (Section 7.2 Unknown Models).
		return fmt.Errorf("batcher: failed to parse S3 key: %w", err)
	}

	rc := RunContext{
		Model:     model,
		Timestamp: ts,
		Bucket:    bucket,
		Key:       key,
	}

	// Step 5: Call ProcessRun
	return b.ProcessRun(ctx, rc)
}
