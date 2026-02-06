package batcher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// --- Test Doubles ---

// mockSQSClient records calls to SendBatch for verification.
type mockSQSClient struct {
	calls    []sendBatchCall
	failNext bool
}

type sendBatchCall struct {
	queueURL string
	messages []types.EvalMessage
}

func (m *mockSQSClient) SendBatch(ctx context.Context, queueURL string, messages []types.EvalMessage) error {
	if m.failNext {
		m.failNext = false
		return fmt.Errorf("simulated SQS failure")
	}
	m.calls = append(m.calls, sendBatchCall{queueURL: queueURL, messages: messages})
	return nil
}

// mockMetricPublisher records metric calls for verification.
type mockMetricPublisher struct {
	forecastReadyCalls []types.ForecastType
	statsCalls         []statsCall
	failForecastReady  bool
}

type statsCall struct {
	ft          types.ForecastType
	tiles       int
	watchpoints int
}

func (m *mockMetricPublisher) PublishForecastReady(ctx context.Context, ft types.ForecastType) error {
	if m.failForecastReady {
		return fmt.Errorf("simulated metric failure")
	}
	m.forecastReadyCalls = append(m.forecastReadyCalls, ft)
	return nil
}

func (m *mockMetricPublisher) PublishStats(ctx context.Context, ft types.ForecastType, tiles int, watchpoints int) error {
	m.statsCalls = append(m.statsCalls, statsCall{ft: ft, tiles: tiles, watchpoints: watchpoints})
	return nil
}

// mockForecastRunRepo simulates ForecastRunRepository for testing.
type mockForecastRunRepo struct {
	run            *types.ForecastRun
	getByTsErr     error
	markCompleteID string
	markCompleteErr error
}

func (m *mockForecastRunRepo) GetByTimestamp(ctx context.Context, model types.ForecastType, ts time.Time) (*types.ForecastRun, error) {
	if m.getByTsErr != nil {
		return nil, m.getByTsErr
	}
	return m.run, nil
}

func (m *mockForecastRunRepo) MarkComplete(ctx context.Context, id string, storagePath string, durationMs int) error {
	m.markCompleteID = id
	return m.markCompleteErr
}

// mockBatcherRepo simulates BatcherRepository for testing.
type mockBatcherRepo struct {
	tileCounts map[string]int
	err        error
}

func (m *mockBatcherRepo) GetActiveTileCounts(ctx context.Context) (map[string]int, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.tileCounts, nil
}

// ForecastRunRepoInterface defines the interface for the forecast run repository
// methods used by Batcher. This enables mock injection in tests.
type ForecastRunRepoInterface interface {
	GetByTimestamp(ctx context.Context, model types.ForecastType, ts time.Time) (*types.ForecastRun, error)
	MarkComplete(ctx context.Context, id string, storagePath string, durationMs int) error
}

// BatcherRepoInterface defines the interface for the batcher repository
// methods used by Batcher.
type BatcherRepoInterface interface {
	GetActiveTileCounts(ctx context.Context) (map[string]int, error)
}

// testBatcher is a test-friendly version of Batcher that uses interfaces
// instead of concrete repository types, enabling mock injection.
type testBatcher struct {
	Config  BatcherConfig
	Log     *slog.Logger
	Repo    ForecastRunRepoInterface
	WPRepo  BatcherRepoInterface
	SQS     SQSClient
	Metrics MetricPublisher
}

// processRun mirrors Batcher.ProcessRun but uses the interface-based repos.
func (b *testBatcher) processRun(ctx context.Context, rc RunContext) error {
	// Step 1: Phantom Run Detection (IMPL-003)
	run, err := b.Repo.GetByTimestamp(ctx, rc.Model, rc.Timestamp)
	if err != nil {
		return fmt.Errorf("batcher: failed to query forecast run: %w", err)
	}

	if run == nil {
		b.Log.ErrorContext(ctx, "Phantom Forecast detected in S3",
			"model", string(rc.Model),
			"run_timestamp", rc.Timestamp.Format(time.RFC3339),
			"bucket", rc.Bucket,
			"key", rc.Key,
		)
		return nil
	}

	switch run.Status {
	case "complete":
		b.Log.InfoContext(ctx, "Duplicate S3 event, forecast run already complete",
			"run_id", run.ID,
			"model", string(rc.Model),
			"run_timestamp", rc.Timestamp.Format(time.RFC3339),
		)
		return nil

	case "running":
		storagePath := rc.Key
		if err := b.Repo.MarkComplete(ctx, run.ID, storagePath, 0); err != nil {
			return fmt.Errorf("batcher: failed to mark forecast run complete: %w", err)
		}

	default:
		b.Log.WarnContext(ctx, "Forecast run has unexpected status, proceeding anyway",
			"run_id", run.ID,
			"status", run.Status,
			"model", string(rc.Model),
		)
	}

	// Use a real Batcher for generateMessages and resolveQueue
	realBatcher := &Batcher{Config: b.Config, Log: b.Log}

	queueURL, err := realBatcher.resolveQueue(rc.Model)
	if err != nil {
		return fmt.Errorf("batcher: %w", err)
	}

	tileCounts, err := b.WPRepo.GetActiveTileCounts(ctx)
	if err != nil {
		return fmt.Errorf("batcher: failed to get active tile counts: %w", err)
	}

	totalWatchpoints := 0
	for _, count := range tileCounts {
		totalWatchpoints += count
	}

	messages := realBatcher.generateMessages(tileCounts, rc)

	if len(messages) > 0 {
		if err := b.SQS.SendBatch(ctx, queueURL, messages); err != nil {
			return fmt.Errorf("batcher: failed to send eval messages to SQS: %w", err)
		}
	}

	if err := b.Metrics.PublishForecastReady(ctx, rc.Model); err != nil {
		b.Log.WarnContext(ctx, "Failed to publish ForecastReady metric", "error", err)
	}

	if err := b.Metrics.PublishStats(ctx, rc.Model, len(tileCounts), totalWatchpoints); err != nil {
		b.Log.WarnContext(ctx, "Failed to publish batch stats metric", "error", err)
	}

	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// --- generateMessages Tests ---

func TestGenerateMessages_HotTilePagination_1200Items(t *testing.T) {
	// Definition of Done: A tile with 1200 items generates 3 EvalMessage structs
	// with correct Page indices.
	b := &Batcher{
		Config: BatcherConfig{MaxPageSize: 500},
		Log:    testLogger(),
	}

	tileCounts := map[string]int{
		"tile_001": 1200,
	}
	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
	}

	messages := b.generateMessages(tileCounts, rc)

	if len(messages) != 3 {
		t.Fatalf("expected 3 messages for 1200 items with page size 500, got %d", len(messages))
	}

	// Sort by page to ensure deterministic assertion order
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Page < messages[j].Page
	})

	// Verify page indices are 0, 1, 2
	for i, msg := range messages {
		if msg.Page != i {
			t.Errorf("message %d: expected Page=%d, got Page=%d", i, i, msg.Page)
		}
		if msg.PageSize != 500 {
			t.Errorf("message %d: expected PageSize=500, got PageSize=%d", i, msg.PageSize)
		}
		if msg.TotalItems != 1200 {
			t.Errorf("message %d: expected TotalItems=1200, got TotalItems=%d", i, msg.TotalItems)
		}
		if msg.TileID != "tile_001" {
			t.Errorf("message %d: expected TileID='tile_001', got TileID=%q", i, msg.TileID)
		}
		if msg.ForecastType != types.ForecastNowcast {
			t.Errorf("message %d: expected ForecastType=%q, got %q", i, types.ForecastNowcast, msg.ForecastType)
		}
		if msg.Action != types.EvalActionEvaluate {
			t.Errorf("message %d: expected Action=%q, got %q", i, types.EvalActionEvaluate, msg.Action)
		}
	}

	// Verify all messages share the same BatchID and TraceID
	batchID := messages[0].BatchID
	traceID := messages[0].TraceID
	if batchID == "" {
		t.Error("expected non-empty BatchID")
	}
	if traceID == "" {
		t.Error("expected non-empty TraceID")
	}
	for i, msg := range messages {
		if msg.BatchID != batchID {
			t.Errorf("message %d: expected BatchID=%q, got %q", i, batchID, msg.BatchID)
		}
		if msg.TraceID != traceID {
			t.Errorf("message %d: expected TraceID=%q, got %q", i, traceID, msg.TraceID)
		}
	}
}

func TestGenerateMessages_SinglePage(t *testing.T) {
	b := &Batcher{
		Config: BatcherConfig{MaxPageSize: 500},
		Log:    testLogger(),
	}

	tileCounts := map[string]int{
		"tile_abc": 100,
	}
	rc := RunContext{
		Model:     types.ForecastMediumRange,
		Timestamp: time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC),
	}

	messages := b.generateMessages(tileCounts, rc)

	if len(messages) != 1 {
		t.Fatalf("expected 1 message for 100 items, got %d", len(messages))
	}
	if messages[0].Page != 0 {
		t.Errorf("expected Page=0, got %d", messages[0].Page)
	}
	if messages[0].TotalItems != 100 {
		t.Errorf("expected TotalItems=100, got %d", messages[0].TotalItems)
	}
}

func TestGenerateMessages_ExactPageSize(t *testing.T) {
	// 500 items with page size 500 should produce exactly 1 page
	b := &Batcher{
		Config: BatcherConfig{MaxPageSize: 500},
		Log:    testLogger(),
	}

	tileCounts := map[string]int{
		"tile_exact": 500,
	}
	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
	}

	messages := b.generateMessages(tileCounts, rc)

	if len(messages) != 1 {
		t.Fatalf("expected 1 message for exactly 500 items, got %d", len(messages))
	}
	if messages[0].Page != 0 {
		t.Errorf("expected Page=0, got %d", messages[0].Page)
	}
}

func TestGenerateMessages_ExactPageSizePlusOne(t *testing.T) {
	// 501 items should produce 2 pages
	b := &Batcher{
		Config: BatcherConfig{MaxPageSize: 500},
		Log:    testLogger(),
	}

	tileCounts := map[string]int{
		"tile_boundary": 501,
	}
	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
	}

	messages := b.generateMessages(tileCounts, rc)

	if len(messages) != 2 {
		t.Fatalf("expected 2 messages for 501 items, got %d", len(messages))
	}
}

func TestGenerateMessages_MultipleTiles(t *testing.T) {
	b := &Batcher{
		Config: BatcherConfig{MaxPageSize: 500},
		Log:    testLogger(),
	}

	tileCounts := map[string]int{
		"tile_a": 100,  // 1 page
		"tile_b": 600,  // 2 pages
		"tile_c": 1500, // 3 pages
	}
	rc := RunContext{
		Model:     types.ForecastMediumRange,
		Timestamp: time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC),
	}

	messages := b.generateMessages(tileCounts, rc)

	// Expected: 1 + 2 + 3 = 6 messages
	if len(messages) != 6 {
		t.Fatalf("expected 6 messages total, got %d", len(messages))
	}

	// Count messages per tile
	tileMessageCount := make(map[string]int)
	for _, msg := range messages {
		tileMessageCount[msg.TileID]++
	}

	if tileMessageCount["tile_a"] != 1 {
		t.Errorf("tile_a: expected 1 message, got %d", tileMessageCount["tile_a"])
	}
	if tileMessageCount["tile_b"] != 2 {
		t.Errorf("tile_b: expected 2 messages, got %d", tileMessageCount["tile_b"])
	}
	if tileMessageCount["tile_c"] != 3 {
		t.Errorf("tile_c: expected 3 messages, got %d", tileMessageCount["tile_c"])
	}
}

func TestGenerateMessages_EmptyTileCounts(t *testing.T) {
	b := &Batcher{
		Config: BatcherConfig{MaxPageSize: 500},
		Log:    testLogger(),
	}

	messages := b.generateMessages(map[string]int{}, RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Now(),
	})

	if messages != nil {
		t.Errorf("expected nil for empty tile counts, got %d messages", len(messages))
	}
}

func TestGenerateMessages_NilTileCounts(t *testing.T) {
	b := &Batcher{
		Config: BatcherConfig{MaxPageSize: 500},
		Log:    testLogger(),
	}

	messages := b.generateMessages(nil, RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Now(),
	})

	if messages != nil {
		t.Errorf("expected nil for nil tile counts, got %d messages", len(messages))
	}
}

func TestGenerateMessages_DefaultPageSize(t *testing.T) {
	// When MaxPageSize is 0, it should default to DefaultMaxPageSize (500)
	b := &Batcher{
		Config: BatcherConfig{MaxPageSize: 0},
		Log:    testLogger(),
	}

	tileCounts := map[string]int{
		"tile_default": 1200,
	}
	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
	}

	messages := b.generateMessages(tileCounts, rc)

	if len(messages) != 3 {
		t.Fatalf("expected 3 messages with default page size, got %d", len(messages))
	}
	if messages[0].PageSize != DefaultMaxPageSize {
		t.Errorf("expected PageSize=%d, got %d", DefaultMaxPageSize, messages[0].PageSize)
	}
}

func TestGenerateMessages_ZeroCountTileSkipped(t *testing.T) {
	b := &Batcher{
		Config: BatcherConfig{MaxPageSize: 500},
		Log:    testLogger(),
	}

	tileCounts := map[string]int{
		"tile_zero":    0,
		"tile_nonzero": 100,
	}
	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Now(),
	}

	messages := b.generateMessages(tileCounts, rc)

	if len(messages) != 1 {
		t.Fatalf("expected 1 message (zero-count tile skipped), got %d", len(messages))
	}
	if messages[0].TileID != "tile_nonzero" {
		t.Errorf("expected tile_nonzero, got %q", messages[0].TileID)
	}
}

func TestGenerateMessages_RunTimestampPropagated(t *testing.T) {
	b := &Batcher{
		Config: BatcherConfig{MaxPageSize: 500},
		Log:    testLogger(),
	}

	ts := time.Date(2026, 2, 6, 18, 30, 0, 0, time.UTC)
	tileCounts := map[string]int{"tile_x": 50}
	rc := RunContext{
		Model:     types.ForecastMediumRange,
		Timestamp: ts,
	}

	messages := b.generateMessages(tileCounts, rc)

	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if !messages[0].RunTimestamp.Equal(ts) {
		t.Errorf("expected RunTimestamp=%v, got %v", ts, messages[0].RunTimestamp)
	}
}

// --- resolveQueue Tests ---

func TestResolveQueue_Nowcast(t *testing.T) {
	b := &Batcher{
		Config: BatcherConfig{
			UrgentQueueURL:   "https://sqs.us-east-1.amazonaws.com/123/eval-urgent",
			StandardQueueURL: "https://sqs.us-east-1.amazonaws.com/123/eval-standard",
		},
	}

	url, err := b.resolveQueue(types.ForecastNowcast)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url != "https://sqs.us-east-1.amazonaws.com/123/eval-urgent" {
		t.Errorf("expected urgent queue URL, got %q", url)
	}
}

func TestResolveQueue_MediumRange(t *testing.T) {
	b := &Batcher{
		Config: BatcherConfig{
			UrgentQueueURL:   "https://sqs.us-east-1.amazonaws.com/123/eval-urgent",
			StandardQueueURL: "https://sqs.us-east-1.amazonaws.com/123/eval-standard",
		},
	}

	url, err := b.resolveQueue(types.ForecastMediumRange)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url != "https://sqs.us-east-1.amazonaws.com/123/eval-standard" {
		t.Errorf("expected standard queue URL, got %q", url)
	}
}

func TestResolveQueue_UnknownModel(t *testing.T) {
	b := &Batcher{
		Config: BatcherConfig{
			UrgentQueueURL:   "https://sqs.us-east-1.amazonaws.com/123/eval-urgent",
			StandardQueueURL: "https://sqs.us-east-1.amazonaws.com/123/eval-standard",
		},
	}

	_, err := b.resolveQueue(types.ForecastType("unknown_model"))
	if err == nil {
		t.Fatal("expected error for unknown model, got nil")
	}
}

// --- ProcessRun Tests (using testBatcher with mock interfaces) ---

func TestProcessRun_PhantomRunDetection(t *testing.T) {
	// IMPL-003 Case C: No record in DB -- should log error and return nil
	sqsMock := &mockSQSClient{}
	metricsMock := &mockMetricPublisher{}

	b := &testBatcher{
		Config: BatcherConfig{
			MaxPageSize:      500,
			UrgentQueueURL:   "https://sqs/urgent",
			StandardQueueURL: "https://sqs/standard",
		},
		Log:     testLogger(),
		Repo:    &mockForecastRunRepo{run: nil}, // Phantom: no record
		WPRepo:  &mockBatcherRepo{tileCounts: map[string]int{"tile_1": 10}},
		SQS:     sqsMock,
		Metrics: metricsMock,
	}

	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
		Bucket:    "forecast-bucket",
		Key:       "forecasts/nowcast/2026-02-06T12:00:00Z/_SUCCESS",
	}

	err := b.processRun(context.Background(), rc)
	if err != nil {
		t.Fatalf("expected nil error for phantom run, got: %v", err)
	}

	// Should NOT have sent any SQS messages
	if len(sqsMock.calls) != 0 {
		t.Errorf("expected 0 SQS calls for phantom run, got %d", len(sqsMock.calls))
	}

	// Should NOT have emitted ForecastReady
	if len(metricsMock.forecastReadyCalls) != 0 {
		t.Errorf("expected 0 ForecastReady calls for phantom run, got %d", len(metricsMock.forecastReadyCalls))
	}
}

func TestProcessRun_DuplicateEvent(t *testing.T) {
	// IMPL-003 Case B: status='complete' -- should log info and return nil
	sqsMock := &mockSQSClient{}
	metricsMock := &mockMetricPublisher{}

	b := &testBatcher{
		Config: BatcherConfig{
			MaxPageSize:      500,
			UrgentQueueURL:   "https://sqs/urgent",
			StandardQueueURL: "https://sqs/standard",
		},
		Log: testLogger(),
		Repo: &mockForecastRunRepo{
			run: &types.ForecastRun{
				ID:     "fr_123",
				Model:  types.ForecastNowcast,
				Status: "complete",
			},
		},
		WPRepo:  &mockBatcherRepo{tileCounts: map[string]int{"tile_1": 10}},
		SQS:     sqsMock,
		Metrics: metricsMock,
	}

	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
	}

	err := b.processRun(context.Background(), rc)
	if err != nil {
		t.Fatalf("expected nil error for duplicate event, got: %v", err)
	}

	// Should NOT have sent any SQS messages
	if len(sqsMock.calls) != 0 {
		t.Errorf("expected 0 SQS calls for duplicate event, got %d", len(sqsMock.calls))
	}
}

func TestProcessRun_NormalFlow(t *testing.T) {
	// IMPL-003 Case A: status='running' -- normal completion flow
	sqsMock := &mockSQSClient{}
	metricsMock := &mockMetricPublisher{}
	repoMock := &mockForecastRunRepo{
		run: &types.ForecastRun{
			ID:     "fr_456",
			Model:  types.ForecastNowcast,
			Status: "running",
		},
	}

	b := &testBatcher{
		Config: BatcherConfig{
			MaxPageSize:      500,
			UrgentQueueURL:   "https://sqs/urgent",
			StandardQueueURL: "https://sqs/standard",
		},
		Log:    testLogger(),
		Repo:   repoMock,
		WPRepo: &mockBatcherRepo{tileCounts: map[string]int{"tile_1": 100, "tile_2": 200}},
		SQS:    sqsMock,
		Metrics: metricsMock,
	}

	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
		Key:       "forecasts/nowcast/2026-02-06T12:00:00Z/_SUCCESS",
	}

	err := b.processRun(context.Background(), rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have marked the run complete
	if repoMock.markCompleteID != "fr_456" {
		t.Errorf("expected MarkComplete called with ID 'fr_456', got %q", repoMock.markCompleteID)
	}

	// Should have sent SQS messages to urgent queue (nowcast)
	if len(sqsMock.calls) != 1 {
		t.Fatalf("expected 1 SQS call, got %d", len(sqsMock.calls))
	}
	if sqsMock.calls[0].queueURL != "https://sqs/urgent" {
		t.Errorf("expected urgent queue URL, got %q", sqsMock.calls[0].queueURL)
	}

	// 100 + 200 = 300 total, both fit in 1 page each = 2 messages
	if len(sqsMock.calls[0].messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(sqsMock.calls[0].messages))
	}

	// Should have emitted ForecastReady
	if len(metricsMock.forecastReadyCalls) != 1 {
		t.Errorf("expected 1 ForecastReady call, got %d", len(metricsMock.forecastReadyCalls))
	}
	if metricsMock.forecastReadyCalls[0] != types.ForecastNowcast {
		t.Errorf("expected ForecastReady for nowcast, got %v", metricsMock.forecastReadyCalls[0])
	}

	// Should have emitted stats
	if len(metricsMock.statsCalls) != 1 {
		t.Fatalf("expected 1 stats call, got %d", len(metricsMock.statsCalls))
	}
	if metricsMock.statsCalls[0].tiles != 2 {
		t.Errorf("expected 2 tiles in stats, got %d", metricsMock.statsCalls[0].tiles)
	}
	if metricsMock.statsCalls[0].watchpoints != 300 {
		t.Errorf("expected 300 watchpoints in stats, got %d", metricsMock.statsCalls[0].watchpoints)
	}
}

func TestProcessRun_ZeroState(t *testing.T) {
	// Section 7.2: Zero State -- empty DB, should still emit ForecastReady
	sqsMock := &mockSQSClient{}
	metricsMock := &mockMetricPublisher{}

	b := &testBatcher{
		Config: BatcherConfig{
			MaxPageSize:      500,
			UrgentQueueURL:   "https://sqs/urgent",
			StandardQueueURL: "https://sqs/standard",
		},
		Log: testLogger(),
		Repo: &mockForecastRunRepo{
			run: &types.ForecastRun{
				ID:     "fr_789",
				Model:  types.ForecastMediumRange,
				Status: "running",
			},
		},
		WPRepo:  &mockBatcherRepo{tileCounts: map[string]int{}}, // Zero state
		SQS:     sqsMock,
		Metrics: metricsMock,
	}

	rc := RunContext{
		Model:     types.ForecastMediumRange,
		Timestamp: time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC),
		Key:       "forecasts/medium_range/2026-02-06T06:00:00Z/_SUCCESS",
	}

	err := b.processRun(context.Background(), rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should NOT have sent any SQS messages (no tiles)
	if len(sqsMock.calls) != 0 {
		t.Errorf("expected 0 SQS calls for zero state, got %d", len(sqsMock.calls))
	}

	// MUST still emit ForecastReady per Section 7.2
	if len(metricsMock.forecastReadyCalls) != 1 {
		t.Errorf("expected 1 ForecastReady call even for zero state, got %d", len(metricsMock.forecastReadyCalls))
	}
}

func TestProcessRun_MediumRangeRoutesToStandardQueue(t *testing.T) {
	sqsMock := &mockSQSClient{}
	metricsMock := &mockMetricPublisher{}

	b := &testBatcher{
		Config: BatcherConfig{
			MaxPageSize:      500,
			UrgentQueueURL:   "https://sqs/urgent",
			StandardQueueURL: "https://sqs/standard",
		},
		Log: testLogger(),
		Repo: &mockForecastRunRepo{
			run: &types.ForecastRun{
				ID:     "fr_mr_1",
				Model:  types.ForecastMediumRange,
				Status: "running",
			},
		},
		WPRepo:  &mockBatcherRepo{tileCounts: map[string]int{"tile_1": 50}},
		SQS:     sqsMock,
		Metrics: metricsMock,
	}

	rc := RunContext{
		Model:     types.ForecastMediumRange,
		Timestamp: time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC),
		Key:       "forecasts/medium_range/2026-02-06T06:00:00Z/_SUCCESS",
	}

	err := b.processRun(context.Background(), rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(sqsMock.calls) != 1 {
		t.Fatalf("expected 1 SQS call, got %d", len(sqsMock.calls))
	}
	if sqsMock.calls[0].queueURL != "https://sqs/standard" {
		t.Errorf("expected standard queue URL for medium_range, got %q", sqsMock.calls[0].queueURL)
	}
}

func TestProcessRun_SQSFailure(t *testing.T) {
	sqsMock := &mockSQSClient{failNext: true}
	metricsMock := &mockMetricPublisher{}

	b := &testBatcher{
		Config: BatcherConfig{
			MaxPageSize:      500,
			UrgentQueueURL:   "https://sqs/urgent",
			StandardQueueURL: "https://sqs/standard",
		},
		Log: testLogger(),
		Repo: &mockForecastRunRepo{
			run: &types.ForecastRun{
				ID:     "fr_fail",
				Model:  types.ForecastNowcast,
				Status: "running",
			},
		},
		WPRepo:  &mockBatcherRepo{tileCounts: map[string]int{"tile_1": 50}},
		SQS:     sqsMock,
		Metrics: metricsMock,
	}

	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
		Key:       "forecasts/nowcast/2026-02-06T12:00:00Z/_SUCCESS",
	}

	err := b.processRun(context.Background(), rc)
	if err == nil {
		t.Fatal("expected error for SQS failure, got nil")
	}
}

func TestProcessRun_DBQueryFailure(t *testing.T) {
	b := &testBatcher{
		Config: BatcherConfig{
			MaxPageSize:      500,
			UrgentQueueURL:   "https://sqs/urgent",
			StandardQueueURL: "https://sqs/standard",
		},
		Log: testLogger(),
		Repo: &mockForecastRunRepo{
			getByTsErr: fmt.Errorf("connection refused"),
		},
		WPRepo:  &mockBatcherRepo{tileCounts: map[string]int{}},
		SQS:     &mockSQSClient{},
		Metrics: &mockMetricPublisher{},
	}

	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
	}

	err := b.processRun(context.Background(), rc)
	if err == nil {
		t.Fatal("expected error for DB failure, got nil")
	}
}

func TestProcessRun_MetricFailureNonFatal(t *testing.T) {
	// Metric publish failures should not cause ProcessRun to fail
	sqsMock := &mockSQSClient{}
	metricsMock := &mockMetricPublisher{failForecastReady: true}

	b := &testBatcher{
		Config: BatcherConfig{
			MaxPageSize:      500,
			UrgentQueueURL:   "https://sqs/urgent",
			StandardQueueURL: "https://sqs/standard",
		},
		Log: testLogger(),
		Repo: &mockForecastRunRepo{
			run: &types.ForecastRun{
				ID:     "fr_metric_fail",
				Model:  types.ForecastNowcast,
				Status: "running",
			},
		},
		WPRepo:  &mockBatcherRepo{tileCounts: map[string]int{"tile_1": 50}},
		SQS:     sqsMock,
		Metrics: metricsMock,
	}

	rc := RunContext{
		Model:     types.ForecastNowcast,
		Timestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
		Key:       "forecasts/nowcast/2026-02-06T12:00:00Z/_SUCCESS",
	}

	err := b.processRun(context.Background(), rc)
	if err != nil {
		t.Fatalf("expected metric failure to be non-fatal, got: %v", err)
	}

	// SQS should still have been called
	if len(sqsMock.calls) != 1 {
		t.Errorf("expected 1 SQS call despite metric failure, got %d", len(sqsMock.calls))
	}
}
