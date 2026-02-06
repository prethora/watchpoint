package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"watchpoint/internal/notifications/core"
	"watchpoint/internal/types"
)

// --- Mock Types ---

// mockDeliveryManager implements core.DeliveryManager for tests.
type mockDeliveryManager struct {
	ensureDeliveryCalls   int
	recordAttemptCalls    int
	markSuccessCalls      int
	markFailureCalls      int
	markSkippedCalls      int
	markDeferredCalls     int
	checkAggregateCalls   int
	resetStateCalls       int
	cancelDeferredCalls   int
	lastDeliveryID        string
	lastProviderMsgID     string
	lastFailureReason     string
	ensureCreated         bool
	markFailureShouldRetry bool
	aggregateAllFailed    bool
}

func (m *mockDeliveryManager) EnsureDeliveryExists(_ context.Context, _ string, _ types.ChannelType, _ int) (string, bool, error) {
	m.ensureDeliveryCalls++
	return "del_test_webhook_0", m.ensureCreated, nil
}

func (m *mockDeliveryManager) RecordAttempt(_ context.Context, _ string) error {
	m.recordAttemptCalls++
	return nil
}

func (m *mockDeliveryManager) MarkSuccess(_ context.Context, deliveryID string, providerMsgID string) error {
	m.markSuccessCalls++
	m.lastDeliveryID = deliveryID
	m.lastProviderMsgID = providerMsgID
	return nil
}

func (m *mockDeliveryManager) MarkFailure(_ context.Context, deliveryID string, reason string) (bool, error) {
	m.markFailureCalls++
	m.lastDeliveryID = deliveryID
	m.lastFailureReason = reason
	return m.markFailureShouldRetry, nil
}

func (m *mockDeliveryManager) MarkSkipped(_ context.Context, deliveryID string, _ string) error {
	m.markSkippedCalls++
	m.lastDeliveryID = deliveryID
	return nil
}

func (m *mockDeliveryManager) MarkDeferred(_ context.Context, _ string, _ time.Time) error {
	m.markDeferredCalls++
	return nil
}

func (m *mockDeliveryManager) CheckAggregateFailure(_ context.Context, _ string) (bool, error) {
	m.checkAggregateCalls++
	return m.aggregateAllFailed, nil
}

func (m *mockDeliveryManager) ResetNotificationState(_ context.Context, _ string) error {
	m.resetStateCalls++
	return nil
}

func (m *mockDeliveryManager) CancelDeferred(_ context.Context, _ string) error {
	m.cancelDeferredCalls++
	return nil
}

// mockMetrics implements core.NotificationMetrics for tests.
type mockMetrics struct {
	deliveryCalls int
	latencyCalls  int
	queueLagCalls int
}

func (m *mockMetrics) RecordDelivery(_ context.Context, _ types.ChannelType, _ core.MetricResult) {
	m.deliveryCalls++
}
func (m *mockMetrics) RecordLatency(_ context.Context, _ types.ChannelType, _ time.Duration) {
	m.latencyCalls++
}
func (m *mockMetrics) RecordQueueLag(_ context.Context, _ time.Duration) {
	m.queueLagCalls++
}

// testLogger implements types.Logger for tests.
type testLogger struct{}

func (l *testLogger) Info(_ string, _ ...any)    {}
func (l *testLogger) Error(_ string, _ ...any)   {}
func (l *testLogger) Warn(_ string, _ ...any)    {}
func (l *testLogger) With(_ ...any) types.Logger { return l }

// --- Helper Functions ---

func buildSQSEvent(messages ...types.NotificationMessage) events.SQSEvent {
	records := make([]events.SQSMessage, len(messages))
	for i, msg := range messages {
		body, _ := json.Marshal(msg)
		records[i] = events.SQSMessage{
			MessageId: "msg-" + msg.NotificationID,
			Body:      string(body),
			Attributes: map[string]string{
				"SentTimestamp": "1706745600000",
			},
		}
	}
	return events.SQSEvent{Records: records}
}

func testNotificationMessage() types.NotificationMessage {
	return types.NotificationMessage{
		NotificationID: "notif-001",
		WatchPointID:   "wp-001",
		OrganizationID: "org-001",
		EventType:      types.EventThresholdCrossed,
		Urgency:        types.UrgencyWarning,
		TestMode:       false,
		RetryCount:     0,
		TraceID:        "trace-001",
		Payload: map[string]interface{}{
			"channels": []map[string]interface{}{
				{
					"id":      "ch-wh-001",
					"type":    "webhook",
					"config":  map[string]interface{}{"url": "https://hooks.slack.com/test"},
					"enabled": true,
				},
			},
		},
	}
}

// --- Tests ---

func TestSlogAdapter_ImplementsLogger(t *testing.T) {
	var logger types.Logger = &slogAdapter{logger: nil}
	if logger == nil {
		t.Fatal("slogAdapter should not be nil")
	}
}

func TestExtractChannels_WithWebhookChannels(t *testing.T) {
	payload := map[string]interface{}{
		"channels": []map[string]interface{}{
			{
				"id":      "ch-001",
				"type":    "email",
				"config":  map[string]interface{}{"address": "user@example.com"},
				"enabled": true,
			},
			{
				"id":      "ch-002",
				"type":    "webhook",
				"config":  map[string]interface{}{"url": "https://hooks.example.com/test"},
				"enabled": true,
			},
		},
	}

	channels := extractChannels(payload)
	if len(channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(channels))
	}
	if channels[0].Type != types.ChannelEmail {
		t.Errorf("expected first channel type 'email', got %q", channels[0].Type)
	}
	if channels[1].Type != types.ChannelWebhook {
		t.Errorf("expected second channel type 'webhook', got %q", channels[1].Type)
	}
}

func TestExtractChannels_NoChannelsKey(t *testing.T) {
	payload := map[string]interface{}{"other": "data"}
	channels := extractChannels(payload)
	if channels != nil {
		t.Errorf("expected nil channels, got %v", channels)
	}
}

func TestExtractChannels_NilPayload(t *testing.T) {
	channels := extractChannels(nil)
	if channels != nil {
		t.Errorf("expected nil channels, got %v", channels)
	}
}

func TestNotificationFromMessage(t *testing.T) {
	msg := testNotificationMessage()
	n := notificationFromMessage(msg)

	if n.ID != msg.NotificationID {
		t.Errorf("expected ID %q, got %q", msg.NotificationID, n.ID)
	}
	if n.WatchPointID != msg.WatchPointID {
		t.Errorf("expected WatchPointID %q, got %q", msg.WatchPointID, n.WatchPointID)
	}
	if n.OrganizationID != msg.OrganizationID {
		t.Errorf("expected OrganizationID %q, got %q", msg.OrganizationID, n.OrganizationID)
	}
	if n.EventType != msg.EventType {
		t.Errorf("expected EventType %q, got %q", msg.EventType, n.EventType)
	}
	if n.Urgency != msg.Urgency {
		t.Errorf("expected Urgency %q, got %q", msg.Urgency, n.Urgency)
	}
	if n.TestMode != msg.TestMode {
		t.Errorf("expected TestMode %v, got %v", msg.TestMode, n.TestMode)
	}
}

func TestParseMillisTimestamp(t *testing.T) {
	ts, err := parseMillisTimestamp("1706745600000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := time.UnixMilli(1706745600000)
	if !ts.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, ts)
	}
}

func TestParseMillisTimestamp_Invalid(t *testing.T) {
	_, err := parseMillisTimestamp("not-a-number")
	if err == nil {
		t.Error("expected error for invalid timestamp")
	}
}

func TestHandler_HandleMalformedMessage(t *testing.T) {
	handler := &Handler{
		channel:     nil, // Won't be called for malformed messages
		deliveryMgr: &mockDeliveryManager{ensureCreated: true},
		metrics:     &mockMetrics{},
		retryPolicy: core.WebhookRetryPolicy,
		logger:      &testLogger{},
	}

	sqsEvent := events.SQSEvent{
		Records: []events.SQSMessage{
			{
				MessageId: "msg-bad",
				Body:      "{{not valid json}}",
				Attributes: map[string]string{
					"SentTimestamp": "1706745600000",
				},
			},
		},
	}

	resp, err := handler.Handle(context.Background(), sqsEvent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Malformed messages should be ACKed to prevent poison pill loops.
	if len(resp.BatchItemFailures) != 0 {
		t.Errorf("expected 0 batch item failures, got %d", len(resp.BatchItemFailures))
	}
}

func TestHandler_NoWebhookChannels(t *testing.T) {
	dm := &mockDeliveryManager{ensureCreated: true}
	handler := &Handler{
		channel:     nil, // Won't be called
		deliveryMgr: dm,
		metrics:     &mockMetrics{},
		retryPolicy: core.WebhookRetryPolicy,
		logger:      &testLogger{},
	}

	msg := types.NotificationMessage{
		NotificationID: "notif-001",
		Payload: map[string]interface{}{
			"channels": []map[string]interface{}{
				{
					"id":      "ch-email-001",
					"type":    "email",
					"config":  map[string]interface{}{"address": "user@example.com"},
					"enabled": true,
				},
			},
		},
	}
	sqsEvent := buildSQSEvent(msg)

	resp, err := handler.Handle(context.Background(), sqsEvent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.BatchItemFailures) != 0 {
		t.Errorf("expected 0 failures, got %d", len(resp.BatchItemFailures))
	}
	// No webhook channels found, so no delivery attempts.
	if dm.ensureDeliveryCalls != 0 {
		t.Errorf("expected 0 EnsureDeliveryExists calls, got %d", dm.ensureDeliveryCalls)
	}
}

func TestHandler_HandleDisabledChannel(t *testing.T) {
	dm := &mockDeliveryManager{ensureCreated: true}
	handler := &Handler{
		channel:     nil, // Won't be called
		deliveryMgr: dm,
		metrics:     &mockMetrics{},
		retryPolicy: core.WebhookRetryPolicy,
		logger:      &testLogger{},
	}

	msg := types.NotificationMessage{
		NotificationID: "notif-001",
		Payload: map[string]interface{}{
			"channels": []map[string]interface{}{
				{
					"id":      "ch-wh-001",
					"type":    "webhook",
					"config":  map[string]interface{}{"url": "https://hooks.example.com"},
					"enabled": false, // Disabled
				},
			},
		},
	}
	sqsEvent := buildSQSEvent(msg)

	resp, err := handler.Handle(context.Background(), sqsEvent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.BatchItemFailures) != 0 {
		t.Errorf("expected 0 failures, got %d", len(resp.BatchItemFailures))
	}
	// Disabled channel is skipped, no delivery attempt.
	if dm.ensureDeliveryCalls != 0 {
		t.Errorf("expected 0 EnsureDeliveryExists calls, got %d", dm.ensureDeliveryCalls)
	}
}
