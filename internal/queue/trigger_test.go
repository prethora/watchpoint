package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"watchpoint/internal/config"
	"watchpoint/internal/types"
)

// --- Mock SQS Client ---

// mockSQSSender captures SendMessage calls for test assertions.
type mockSQSSender struct {
	// calls records every SendMessageInput passed to SendMessage.
	calls []*sqs.SendMessageInput
	// err is returned by SendMessage if non-nil (simulates SQS failures).
	err error
}

func (m *mockSQSSender) SendMessage(_ context.Context, params *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	m.calls = append(m.calls, params)
	if m.err != nil {
		return nil, m.err
	}
	return &sqs.SendMessageOutput{}, nil
}

// --- Test Helpers ---

const (
	testUrgentURL   = "https://sqs.us-east-1.amazonaws.com/123456789/eval-urgent"
	testStandardURL = "https://sqs.us-east-1.amazonaws.com/123456789/eval-standard"
)

func newTestTrigger(mock *mockSQSSender) *EvalTrigger {
	awsCfg := config.AWSConfig{
		EvalQueueUrgent:   testUrgentURL,
		EvalQueueStandard: testStandardURL,
	}
	logger := slog.Default()
	return NewEvalTrigger(mock, awsCfg, logger)
}

// --- Tests ---

func TestTriggerEvaluation_SendsToStandardQueue(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	err := trigger.TriggerEvaluation(context.Background(), "wp_123", "resume")
	if err != nil {
		t.Fatalf("TriggerEvaluation returned unexpected error: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 SQS call, got %d", len(mock.calls))
	}

	call := mock.calls[0]

	// Targeted evaluations should always go to Standard queue.
	if *call.QueueUrl != testStandardURL {
		t.Errorf("expected queue URL %q, got %q", testStandardURL, *call.QueueUrl)
	}
}

func TestTriggerEvaluation_MapsWPIDToSpecificWatchPointIDs(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	wpID := "wp_abc_123"
	err := trigger.TriggerEvaluation(context.Background(), wpID, "resume")
	if err != nil {
		t.Fatalf("TriggerEvaluation returned unexpected error: %v", err)
	}

	call := mock.calls[0]
	var msg types.EvalMessage
	if err := json.Unmarshal([]byte(*call.MessageBody), &msg); err != nil {
		t.Fatalf("failed to unmarshal message body: %v", err)
	}

	if len(msg.SpecificWatchPointIDs) != 1 {
		t.Fatalf("expected 1 specific WP ID, got %d", len(msg.SpecificWatchPointIDs))
	}
	if msg.SpecificWatchPointIDs[0] != wpID {
		t.Errorf("expected specific WP ID %q, got %q", wpID, msg.SpecificWatchPointIDs[0])
	}
}

func TestTriggerEvaluation_SetsEvalActionEvaluate(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	err := trigger.TriggerEvaluation(context.Background(), "wp_123", "resume")
	if err != nil {
		t.Fatalf("TriggerEvaluation returned unexpected error: %v", err)
	}

	var msg types.EvalMessage
	if err := json.Unmarshal([]byte(*mock.calls[0].MessageBody), &msg); err != nil {
		t.Fatalf("failed to unmarshal message body: %v", err)
	}

	if msg.Action != types.EvalActionEvaluate {
		t.Errorf("expected action %q, got %q", types.EvalActionEvaluate, msg.Action)
	}
}

func TestTriggerEvaluation_GeneratesBatchIDAndTraceID(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	err := trigger.TriggerEvaluation(context.Background(), "wp_123", "resume")
	if err != nil {
		t.Fatalf("TriggerEvaluation returned unexpected error: %v", err)
	}

	var msg types.EvalMessage
	if err := json.Unmarshal([]byte(*mock.calls[0].MessageBody), &msg); err != nil {
		t.Fatalf("failed to unmarshal message body: %v", err)
	}

	if !strings.HasPrefix(msg.BatchID, "targeted_") {
		t.Errorf("expected BatchID to start with 'targeted_', got %q", msg.BatchID)
	}
	if msg.TraceID == "" {
		t.Error("expected non-empty TraceID")
	}
}

func TestTriggerEvaluation_SetsReasonMessageAttribute(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	reason := "watchpoint_resumed"
	err := trigger.TriggerEvaluation(context.Background(), "wp_123", reason)
	if err != nil {
		t.Fatalf("TriggerEvaluation returned unexpected error: %v", err)
	}

	call := mock.calls[0]
	attr, ok := call.MessageAttributes["reason"]
	if !ok {
		t.Fatal("expected 'reason' message attribute to be set")
	}
	if *attr.StringValue != reason {
		t.Errorf("expected reason attribute %q, got %q", reason, *attr.StringValue)
	}
	if *attr.DataType != "String" {
		t.Errorf("expected DataType 'String', got %q", *attr.DataType)
	}
}

func TestTriggerEvaluation_SetsRunTimestamp(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	before := time.Now().UTC()
	err := trigger.TriggerEvaluation(context.Background(), "wp_123", "resume")
	if err != nil {
		t.Fatalf("TriggerEvaluation returned unexpected error: %v", err)
	}
	after := time.Now().UTC()

	var msg types.EvalMessage
	if err := json.Unmarshal([]byte(*mock.calls[0].MessageBody), &msg); err != nil {
		t.Fatalf("failed to unmarshal message body: %v", err)
	}

	if msg.RunTimestamp.Before(before) || msg.RunTimestamp.After(after) {
		t.Errorf("RunTimestamp %v not in expected range [%v, %v]", msg.RunTimestamp, before, after)
	}
}

func TestSendEvalMessage_NowcastRoutesToUrgentQueue(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	msg := types.EvalMessage{
		BatchID:      "batch_001",
		TraceID:      "trace_001",
		ForecastType: types.ForecastNowcast,
		RunTimestamp: time.Now().UTC(),
		TileID:       "tile_39_-77",
		Page:         1,
		PageSize:     50,
		TotalItems:   100,
		Action:       types.EvalActionEvaluate,
	}

	err := trigger.SendEvalMessage(context.Background(), msg, "forecast_ready")
	if err != nil {
		t.Fatalf("SendEvalMessage returned unexpected error: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 SQS call, got %d", len(mock.calls))
	}

	if *mock.calls[0].QueueUrl != testUrgentURL {
		t.Errorf("nowcast message should route to urgent queue %q, got %q",
			testUrgentURL, *mock.calls[0].QueueUrl)
	}
}

func TestSendEvalMessage_MediumRangeRoutesToStandardQueue(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	msg := types.EvalMessage{
		BatchID:      "batch_002",
		TraceID:      "trace_002",
		ForecastType: types.ForecastMediumRange,
		RunTimestamp: time.Now().UTC(),
		TileID:       "tile_40_-74",
		Page:         1,
		PageSize:     50,
		TotalItems:   200,
		Action:       types.EvalActionEvaluate,
	}

	err := trigger.SendEvalMessage(context.Background(), msg, "forecast_ready")
	if err != nil {
		t.Fatalf("SendEvalMessage returned unexpected error: %v", err)
	}

	if *mock.calls[0].QueueUrl != testStandardURL {
		t.Errorf("medium_range message should route to standard queue %q, got %q",
			testStandardURL, *mock.calls[0].QueueUrl)
	}
}

func TestSendEvalMessage_EmptyForecastTypeRoutesToStandardQueue(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	msg := types.EvalMessage{
		BatchID:               "batch_targeted",
		TraceID:               "trace_targeted",
		ForecastType:          "", // Targeted evaluation, no forecast type
		RunTimestamp:          time.Now().UTC(),
		SpecificWatchPointIDs: []string{"wp_1"},
	}

	err := trigger.SendEvalMessage(context.Background(), msg, "targeted_resume")
	if err != nil {
		t.Fatalf("SendEvalMessage returned unexpected error: %v", err)
	}

	if *mock.calls[0].QueueUrl != testStandardURL {
		t.Errorf("empty forecast type should route to standard queue %q, got %q",
			testStandardURL, *mock.calls[0].QueueUrl)
	}
}

func TestSendEvalMessage_PreservesFullPayload(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	original := types.EvalMessage{
		BatchID:               "batch_full",
		TraceID:               "trace_full",
		ForecastType:          types.ForecastMediumRange,
		RunTimestamp:          now,
		TileID:                "tile_39_-77",
		Page:                  2,
		PageSize:              100,
		TotalItems:            350,
		Action:                types.EvalActionEvaluate,
		SpecificWatchPointIDs: []string{"wp_1", "wp_2", "wp_3"},
	}

	err := trigger.SendEvalMessage(context.Background(), original, "batch_dispatch")
	if err != nil {
		t.Fatalf("SendEvalMessage returned unexpected error: %v", err)
	}

	var decoded types.EvalMessage
	if err := json.Unmarshal([]byte(*mock.calls[0].MessageBody), &decoded); err != nil {
		t.Fatalf("failed to unmarshal message body: %v", err)
	}

	if decoded.BatchID != original.BatchID {
		t.Errorf("BatchID mismatch: got %q, want %q", decoded.BatchID, original.BatchID)
	}
	if decoded.TraceID != original.TraceID {
		t.Errorf("TraceID mismatch: got %q, want %q", decoded.TraceID, original.TraceID)
	}
	if decoded.ForecastType != original.ForecastType {
		t.Errorf("ForecastType mismatch: got %q, want %q", decoded.ForecastType, original.ForecastType)
	}
	if !decoded.RunTimestamp.Equal(original.RunTimestamp) {
		t.Errorf("RunTimestamp mismatch: got %v, want %v", decoded.RunTimestamp, original.RunTimestamp)
	}
	if decoded.TileID != original.TileID {
		t.Errorf("TileID mismatch: got %q, want %q", decoded.TileID, original.TileID)
	}
	if decoded.Page != original.Page {
		t.Errorf("Page mismatch: got %d, want %d", decoded.Page, original.Page)
	}
	if decoded.PageSize != original.PageSize {
		t.Errorf("PageSize mismatch: got %d, want %d", decoded.PageSize, original.PageSize)
	}
	if decoded.TotalItems != original.TotalItems {
		t.Errorf("TotalItems mismatch: got %d, want %d", decoded.TotalItems, original.TotalItems)
	}
	if decoded.Action != original.Action {
		t.Errorf("Action mismatch: got %q, want %q", decoded.Action, original.Action)
	}
	if len(decoded.SpecificWatchPointIDs) != len(original.SpecificWatchPointIDs) {
		t.Fatalf("SpecificWatchPointIDs length mismatch: got %d, want %d",
			len(decoded.SpecificWatchPointIDs), len(original.SpecificWatchPointIDs))
	}
	for i, id := range original.SpecificWatchPointIDs {
		if decoded.SpecificWatchPointIDs[i] != id {
			t.Errorf("SpecificWatchPointIDs[%d] mismatch: got %q, want %q", i, decoded.SpecificWatchPointIDs[i], id)
		}
	}
}

func TestSendEvalMessage_SQSError(t *testing.T) {
	sqsErr := fmt.Errorf("service unavailable")
	mock := &mockSQSSender{err: sqsErr}
	trigger := newTestTrigger(mock)

	msg := types.EvalMessage{
		BatchID:      "batch_fail",
		ForecastType: types.ForecastMediumRange,
	}

	err := trigger.SendEvalMessage(context.Background(), msg, "test")
	if err == nil {
		t.Fatal("expected error from SendEvalMessage, got nil")
	}
	if !strings.Contains(err.Error(), "failed to send EvalMessage") {
		t.Errorf("expected error message to contain 'failed to send EvalMessage', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), testStandardURL) {
		t.Errorf("expected error message to contain queue URL %q, got %q", testStandardURL, err.Error())
	}
}

func TestTriggerEvaluation_SQSError(t *testing.T) {
	sqsErr := fmt.Errorf("access denied")
	mock := &mockSQSSender{err: sqsErr}
	trigger := newTestTrigger(mock)

	err := trigger.TriggerEvaluation(context.Background(), "wp_123", "resume")
	if err == nil {
		t.Fatal("expected error from TriggerEvaluation, got nil")
	}
	if !strings.Contains(err.Error(), "failed to send EvalMessage") {
		t.Errorf("expected error message to contain 'failed to send EvalMessage', got %q", err.Error())
	}
}

func TestQueueURLForMessage_AllForecastTypes(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	tests := []struct {
		name         string
		forecastType types.ForecastType
		expectedURL  string
	}{
		{
			name:         "nowcast routes to urgent",
			forecastType: types.ForecastNowcast,
			expectedURL:  testUrgentURL,
		},
		{
			name:         "medium_range routes to standard",
			forecastType: types.ForecastMediumRange,
			expectedURL:  testStandardURL,
		},
		{
			name:         "empty forecast type routes to standard",
			forecastType: "",
			expectedURL:  testStandardURL,
		},
		{
			name:         "unknown forecast type routes to standard",
			forecastType: types.ForecastType("unknown"),
			expectedURL:  testStandardURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := types.EvalMessage{ForecastType: tt.forecastType}
			url := trigger.queueURLForMessage(msg)
			if url != tt.expectedURL {
				t.Errorf("expected %q, got %q", tt.expectedURL, url)
			}
		})
	}
}

func TestNewEvalTrigger_ConfiguresQueueURLs(t *testing.T) {
	mock := &mockSQSSender{}
	awsCfg := config.AWSConfig{
		EvalQueueUrgent:   "https://sqs.us-east-1.amazonaws.com/custom/urgent",
		EvalQueueStandard: "https://sqs.us-east-1.amazonaws.com/custom/standard",
	}
	logger := slog.Default()

	trigger := NewEvalTrigger(mock, awsCfg, logger)

	if trigger.urgentQueueURL != awsCfg.EvalQueueUrgent {
		t.Errorf("urgent queue URL mismatch: got %q, want %q", trigger.urgentQueueURL, awsCfg.EvalQueueUrgent)
	}
	if trigger.standardQueueURL != awsCfg.EvalQueueStandard {
		t.Errorf("standard queue URL mismatch: got %q, want %q", trigger.standardQueueURL, awsCfg.EvalQueueStandard)
	}
}

// TestEvalTriggerSatisfiesEvaluationTriggerInterface is a compile-time check
// that EvalTrigger's TriggerEvaluation method has the correct signature to
// satisfy the EvaluationTrigger interface described in 05b-api-watchpoints.md.
// The interface is defined locally in the handlers package, so we verify the
// method signature here by assigning to a function variable with the expected type.
func TestEvalTriggerSatisfiesEvaluationTriggerInterface(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	// Verify the method exists and has the correct signature.
	var fn func(ctx context.Context, wpID string, reason string) error
	fn = trigger.TriggerEvaluation
	_ = fn // Ensure fn is used; compile-time check only.
}

func TestSendEvalMessage_GenerateSummaryAction(t *testing.T) {
	mock := &mockSQSSender{}
	trigger := newTestTrigger(mock)

	msg := types.EvalMessage{
		BatchID:      "batch_summary",
		TraceID:      "trace_summary",
		ForecastType: types.ForecastMediumRange,
		RunTimestamp: time.Now().UTC(),
		TileID:       "tile_39_-77",
		Page:         1,
		PageSize:     50,
		TotalItems:   100,
		Action:       types.EvalActionGenerateSummary,
	}

	err := trigger.SendEvalMessage(context.Background(), msg, "archival")
	if err != nil {
		t.Fatalf("SendEvalMessage returned unexpected error: %v", err)
	}

	var decoded types.EvalMessage
	if err := json.Unmarshal([]byte(*mock.calls[0].MessageBody), &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.Action != types.EvalActionGenerateSummary {
		t.Errorf("expected action %q, got %q", types.EvalActionGenerateSummary, decoded.Action)
	}
}
