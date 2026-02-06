package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"watchpoint/internal/types"
)

// mockSQSSender records all SendMessage calls for verification.
type mockSQSSender struct {
	calls     []*sqs.SendMessageInput
	returnErr error
}

func (m *mockSQSSender) SendMessage(_ context.Context, params *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	m.calls = append(m.calls, params)
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	return &sqs.SendMessageOutput{}, nil
}

func TestNotificationPublisher_Publish_IncrementsRetryCount(t *testing.T) {
	// This is the CRUCIAL behavior from 08a-notification-core.md Section 7.1:
	// RetryCount must be incremented BEFORE serialization.
	sender := &mockSQSSender{}
	logger := &mockLogger{}
	pub := NewNotificationPublisher(sender, "https://sqs.us-east-1.amazonaws.com/123/notifications", logger)

	msg := types.NotificationMessage{
		NotificationID: "notif_001",
		WatchPointID:   "wp_001",
		OrganizationID: "org_001",
		EventType:      types.EventThresholdCrossed,
		Urgency:        types.UrgencyRoutine,
		RetryCount:     0,
		TraceID:        "trace_001",
		Payload:        map[string]interface{}{"key": "value"},
	}

	err := pub.Publish(context.Background(), msg, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 SQS call, got %d", len(sender.calls))
	}

	// Deserialize the sent message body and verify RetryCount was incremented.
	var sent types.NotificationMessage
	if err := json.Unmarshal([]byte(*sender.calls[0].MessageBody), &sent); err != nil {
		t.Fatalf("failed to unmarshal sent body: %v", err)
	}

	if sent.RetryCount != 1 {
		t.Errorf("expected RetryCount=1 in serialized message, got %d", sent.RetryCount)
	}

	// Verify the original message is NOT mutated (passed by value).
	if msg.RetryCount != 0 {
		t.Errorf("original message RetryCount was mutated: expected 0, got %d", msg.RetryCount)
	}
}

func TestNotificationPublisher_Publish_IncrementsFromNonZero(t *testing.T) {
	// Verify increment works on subsequent retries (e.g., RetryCount=2 -> 3).
	sender := &mockSQSSender{}
	logger := &mockLogger{}
	pub := NewNotificationPublisher(sender, "https://sqs.us-east-1.amazonaws.com/123/notifications", logger)

	msg := types.NotificationMessage{
		NotificationID: "notif_002",
		WatchPointID:   "wp_002",
		RetryCount:     2,
		TraceID:        "trace_002",
	}

	err := pub.Publish(context.Background(), msg, 25*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var sent types.NotificationMessage
	if err := json.Unmarshal([]byte(*sender.calls[0].MessageBody), &sent); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if sent.RetryCount != 3 {
		t.Errorf("expected RetryCount=3, got %d", sent.RetryCount)
	}
}

func TestNotificationPublisher_Publish_DelaySeconds(t *testing.T) {
	sender := &mockSQSSender{}
	logger := &mockLogger{}
	pub := NewNotificationPublisher(sender, "https://sqs.us-east-1.amazonaws.com/123/notifications", logger)

	msg := types.NotificationMessage{
		NotificationID: "notif_003",
		RetryCount:     0,
	}

	err := pub.Publish(context.Background(), msg, 10*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.calls[0].DelaySeconds != 10 {
		t.Errorf("expected DelaySeconds=10, got %d", sender.calls[0].DelaySeconds)
	}
}

func TestNotificationPublisher_Publish_ClampsDelayTo900(t *testing.T) {
	// SQS maximum DelaySeconds is 900. Verify clamping.
	sender := &mockSQSSender{}
	logger := &mockLogger{}
	pub := NewNotificationPublisher(sender, "https://sqs.us-east-1.amazonaws.com/123/notifications", logger)

	msg := types.NotificationMessage{
		NotificationID: "notif_004",
		RetryCount:     0,
	}

	err := pub.Publish(context.Background(), msg, 2000*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.calls[0].DelaySeconds != 900 {
		t.Errorf("expected DelaySeconds clamped to 900, got %d", sender.calls[0].DelaySeconds)
	}
}

func TestNotificationPublisher_Publish_ZeroDelay(t *testing.T) {
	sender := &mockSQSSender{}
	logger := &mockLogger{}
	pub := NewNotificationPublisher(sender, "https://sqs.us-east-1.amazonaws.com/123/notifications", logger)

	msg := types.NotificationMessage{
		NotificationID: "notif_005",
		RetryCount:     0,
	}

	err := pub.Publish(context.Background(), msg, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.calls[0].DelaySeconds != 0 {
		t.Errorf("expected DelaySeconds=0, got %d", sender.calls[0].DelaySeconds)
	}
}

func TestNotificationPublisher_Publish_NegativeDelayClampsToZero(t *testing.T) {
	sender := &mockSQSSender{}
	logger := &mockLogger{}
	pub := NewNotificationPublisher(sender, "https://sqs.us-east-1.amazonaws.com/123/notifications", logger)

	msg := types.NotificationMessage{
		NotificationID: "notif_006",
		RetryCount:     0,
	}

	err := pub.Publish(context.Background(), msg, -5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sender.calls[0].DelaySeconds != 0 {
		t.Errorf("expected DelaySeconds=0 for negative delay, got %d", sender.calls[0].DelaySeconds)
	}
}

func TestNotificationPublisher_Publish_SQSError(t *testing.T) {
	sender := &mockSQSSender{returnErr: fmt.Errorf("SQS unavailable")}
	logger := &mockLogger{}
	pub := NewNotificationPublisher(sender, "https://sqs.us-east-1.amazonaws.com/123/notifications", logger)

	msg := types.NotificationMessage{
		NotificationID: "notif_007",
		RetryCount:     0,
	}

	err := pub.Publish(context.Background(), msg, 1*time.Second)
	if err == nil {
		t.Fatal("expected error for SQS failure")
	}

	if len(sender.calls) != 1 {
		t.Errorf("expected 1 SQS call attempt, got %d", len(sender.calls))
	}
}

func TestNotificationPublisher_Publish_QueueURL(t *testing.T) {
	sender := &mockSQSSender{}
	logger := &mockLogger{}
	queueURL := "https://sqs.us-east-1.amazonaws.com/123/my-notifications"
	pub := NewNotificationPublisher(sender, queueURL, logger)

	msg := types.NotificationMessage{
		NotificationID: "notif_008",
		RetryCount:     0,
	}

	err := pub.Publish(context.Background(), msg, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if *sender.calls[0].QueueUrl != queueURL {
		t.Errorf("expected QueueUrl=%q, got %q", queueURL, *sender.calls[0].QueueUrl)
	}
}

func TestNotificationPublisher_Publish_PreservesAllFields(t *testing.T) {
	// Verify the full message round-trips correctly (all fields preserved).
	sender := &mockSQSSender{}
	logger := &mockLogger{}
	pub := NewNotificationPublisher(sender, "https://sqs.us-east-1.amazonaws.com/123/notifications", logger)

	evalTime := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	forecastTime := time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC)

	msg := types.NotificationMessage{
		NotificationID: "notif_full",
		WatchPointID:   "wp_full",
		OrganizationID: "org_full",
		EventType:      types.EventThresholdCrossed,
		Urgency:        types.UrgencyCritical,
		TestMode:       true,
		Ordering: types.OrderingMetadata{
			EventSequence:     42,
			ForecastTimestamp: forecastTime,
			EvalTimestamp:     evalTime,
		},
		RetryCount: 1,
		TraceID:    "trace_full",
		Payload: map[string]interface{}{
			"temperature_c": 35.5,
			"location":      "test",
		},
	}

	err := pub.Publish(context.Background(), msg, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var sent types.NotificationMessage
	if err := json.Unmarshal([]byte(*sender.calls[0].MessageBody), &sent); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// RetryCount should be incremented
	if sent.RetryCount != 2 {
		t.Errorf("RetryCount: expected 2, got %d", sent.RetryCount)
	}

	// All other fields should be preserved
	if sent.NotificationID != "notif_full" {
		t.Errorf("NotificationID: expected notif_full, got %s", sent.NotificationID)
	}
	if sent.WatchPointID != "wp_full" {
		t.Errorf("WatchPointID: expected wp_full, got %s", sent.WatchPointID)
	}
	if sent.OrganizationID != "org_full" {
		t.Errorf("OrganizationID: expected org_full, got %s", sent.OrganizationID)
	}
	if sent.EventType != types.EventThresholdCrossed {
		t.Errorf("EventType: expected threshold_crossed, got %s", sent.EventType)
	}
	if sent.Urgency != types.UrgencyCritical {
		t.Errorf("Urgency: expected critical, got %s", sent.Urgency)
	}
	if !sent.TestMode {
		t.Error("TestMode: expected true, got false")
	}
	if sent.Ordering.EventSequence != 42 {
		t.Errorf("Ordering.EventSequence: expected 42, got %d", sent.Ordering.EventSequence)
	}
	if sent.TraceID != "trace_full" {
		t.Errorf("TraceID: expected trace_full, got %s", sent.TraceID)
	}
}
