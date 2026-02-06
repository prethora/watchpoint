package types

import (
	"encoding/json"
	"testing"
	"time"
)

// TestNotificationMessageJSONRoundTrip verifies that NotificationMessage
// serializes to JSON with the exact snake_case keys expected by the Python
// Eval Worker (Pydantic model). This is the cross-language SQS contract.
func TestNotificationMessageJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	msg := NotificationMessage{
		NotificationID: "notif_abc123",
		WatchPointID:   "wp_xyz789",
		OrganizationID: "org_001",
		EventType:      EventThresholdCrossed,
		Urgency:        UrgencyWarning,
		TestMode:       false,
		Ordering: OrderingMetadata{
			EventSequence:     42,
			ForecastTimestamp: now.Add(-1 * time.Hour),
			EvalTimestamp:     now,
		},
		RetryCount: 0,
		TraceID:    "1-67890abc-def012345678",
		Payload: map[string]interface{}{
			"watchpoint_name": "Downtown Sensor",
			"temperature_c":   22.5,
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Verify all required snake_case JSON keys are present
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map failed: %v", err)
	}

	requiredKeys := []string{
		"notification_id",
		"watchpoint_id",
		"organization_id",
		"event_type",
		"urgency",
		"test_mode",
		"ordering",
		"retry_count",
		"trace_id",
		"payload",
	}

	for _, key := range requiredKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("Missing required JSON key: %q", key)
		}
	}

	// Verify ordering sub-keys
	ordering, ok := raw["ordering"].(map[string]interface{})
	if !ok {
		t.Fatal("ordering is not an object")
	}
	for _, key := range []string{"event_sequence", "forecast_timestamp", "eval_timestamp"} {
		if _, ok := ordering[key]; !ok {
			t.Errorf("Missing ordering key: %q", key)
		}
	}

	// Verify round-trip deserialization
	var decoded NotificationMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Round-trip unmarshal failed: %v", err)
	}
	if decoded.NotificationID != msg.NotificationID {
		t.Errorf("NotificationID mismatch: got %q, want %q", decoded.NotificationID, msg.NotificationID)
	}
	if decoded.WatchPointID != msg.WatchPointID {
		t.Errorf("WatchPointID mismatch: got %q, want %q", decoded.WatchPointID, msg.WatchPointID)
	}
	if decoded.OrganizationID != msg.OrganizationID {
		t.Errorf("OrganizationID mismatch: got %q, want %q", decoded.OrganizationID, msg.OrganizationID)
	}
	if decoded.EventType != msg.EventType {
		t.Errorf("EventType mismatch: got %q, want %q", decoded.EventType, msg.EventType)
	}
	if decoded.Urgency != msg.Urgency {
		t.Errorf("Urgency mismatch: got %q, want %q", decoded.Urgency, msg.Urgency)
	}
	if decoded.TestMode != msg.TestMode {
		t.Errorf("TestMode mismatch: got %v, want %v", decoded.TestMode, msg.TestMode)
	}
	if decoded.RetryCount != msg.RetryCount {
		t.Errorf("RetryCount mismatch: got %d, want %d", decoded.RetryCount, msg.RetryCount)
	}
	if decoded.TraceID != msg.TraceID {
		t.Errorf("TraceID mismatch: got %q, want %q", decoded.TraceID, msg.TraceID)
	}
	if decoded.Ordering.EventSequence != msg.Ordering.EventSequence {
		t.Errorf("EventSequence mismatch: got %d, want %d", decoded.Ordering.EventSequence, msg.Ordering.EventSequence)
	}
	if len(decoded.Payload) != 2 {
		t.Errorf("Payload length mismatch: got %d, want 2", len(decoded.Payload))
	}
}

// TestNotificationMessageTestModeFlag verifies the test_mode field serializes
// correctly for both true and false values.
func TestNotificationMessageTestModeFlag(t *testing.T) {
	tests := []struct {
		name     string
		testMode bool
	}{
		{"test mode enabled", true},
		{"test mode disabled", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := NotificationMessage{
				NotificationID: "notif_test",
				TestMode:       tt.testMode,
			}

			data, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("Marshal failed: %v", err)
			}

			var raw map[string]interface{}
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}

			got, ok := raw["test_mode"].(bool)
			if !ok {
				t.Fatal("test_mode is not a boolean in JSON output")
			}
			if got != tt.testMode {
				t.Errorf("test_mode mismatch: got %v, want %v", got, tt.testMode)
			}
		})
	}
}

// TestNotificationMessageRetryCountIncrement verifies that the retry count
// survives JSON serialization across the SQS publish-subscribe cycle.
func TestNotificationMessageRetryCountIncrement(t *testing.T) {
	msg := NotificationMessage{
		NotificationID: "notif_retry",
		WatchPointID:   "wp_001",
		OrganizationID: "org_001",
		RetryCount:     2,
	}

	// Simulate what a worker does: serialize, increment, re-publish
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded NotificationMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	decoded.RetryCount++

	data2, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("Second marshal failed: %v", err)
	}

	var final NotificationMessage
	if err := json.Unmarshal(data2, &final); err != nil {
		t.Fatalf("Final unmarshal failed: %v", err)
	}

	if final.RetryCount != 3 {
		t.Errorf("RetryCount after increment: got %d, want 3", final.RetryCount)
	}
}

// TestNotificationMessageNilPayload verifies that a nil payload serializes
// to JSON null (not omitted), maintaining schema consistency with Pydantic.
func TestNotificationMessageNilPayload(t *testing.T) {
	msg := NotificationMessage{
		NotificationID: "notif_no_payload",
		Payload:        nil,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Payload key should be present (as null) since there is no omitempty tag
	if _, ok := raw["payload"]; !ok {
		t.Error("payload key should be present even when nil (no omitempty)")
	}
}

// TestNotificationMessageFromPythonJSON simulates deserializing a message
// produced by the Python Eval Worker's Pydantic model. This is the exact
// cross-language contract test.
func TestNotificationMessageFromPythonJSON(t *testing.T) {
	// Simulated Python Pydantic output (snake_case keys, ISO 8601 timestamps)
	pythonJSON := `{
		"notification_id": "notif_py_001",
		"watchpoint_id": "wp_py_001",
		"organization_id": "org_py_001",
		"event_type": "threshold_crossed",
		"urgency": "warning",
		"test_mode": false,
		"ordering": {
			"event_sequence": 17,
			"forecast_timestamp": "2026-02-06T10:00:00Z",
			"eval_timestamp": "2026-02-06T11:00:00Z"
		},
		"retry_count": 0,
		"trace_id": "1-abcdef12-3456789abcdef012",
		"payload": {
			"watchpoint_name": "Rooftop Monitor",
			"location": {"lat": 51.5074, "lon": -0.1278, "display_name": "London, UK"},
			"forecast_snapshot": {"precipitation_prob": 90.0, "temperature_c": 8.5}
		}
	}`

	var msg NotificationMessage
	if err := json.Unmarshal([]byte(pythonJSON), &msg); err != nil {
		t.Fatalf("Failed to unmarshal Python JSON: %v", err)
	}

	if msg.NotificationID != "notif_py_001" {
		t.Errorf("NotificationID: got %q, want %q", msg.NotificationID, "notif_py_001")
	}
	if msg.WatchPointID != "wp_py_001" {
		t.Errorf("WatchPointID: got %q, want %q", msg.WatchPointID, "wp_py_001")
	}
	if msg.OrganizationID != "org_py_001" {
		t.Errorf("OrganizationID: got %q, want %q", msg.OrganizationID, "org_py_001")
	}
	if msg.EventType != EventThresholdCrossed {
		t.Errorf("EventType: got %q, want %q", msg.EventType, EventThresholdCrossed)
	}
	if msg.Urgency != UrgencyWarning {
		t.Errorf("Urgency: got %q, want %q", msg.Urgency, UrgencyWarning)
	}
	if msg.TestMode != false {
		t.Errorf("TestMode: got %v, want false", msg.TestMode)
	}
	if msg.Ordering.EventSequence != 17 {
		t.Errorf("EventSequence: got %d, want 17", msg.Ordering.EventSequence)
	}
	if msg.RetryCount != 0 {
		t.Errorf("RetryCount: got %d, want 0", msg.RetryCount)
	}
	if msg.TraceID != "1-abcdef12-3456789abcdef012" {
		t.Errorf("TraceID: got %q, want %q", msg.TraceID, "1-abcdef12-3456789abcdef012")
	}
	if msg.Payload == nil {
		t.Fatal("Payload should not be nil")
	}
	if msg.Payload["watchpoint_name"] != "Rooftop Monitor" {
		t.Errorf("Payload watchpoint_name: got %v, want %q", msg.Payload["watchpoint_name"], "Rooftop Monitor")
	}
}

// TestNotificationMessageAllUrgencyLevels verifies that all urgency levels
// survive the JSON round-trip correctly.
func TestNotificationMessageAllUrgencyLevels(t *testing.T) {
	levels := []UrgencyLevel{
		UrgencyRoutine,
		UrgencyWatch,
		UrgencyWarning,
		UrgencyCritical,
	}

	for _, level := range levels {
		t.Run(string(level), func(t *testing.T) {
			msg := NotificationMessage{
				NotificationID: "notif_urgency_test",
				Urgency:        level,
			}

			data, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("Marshal failed: %v", err)
			}

			var decoded NotificationMessage
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}

			if decoded.Urgency != level {
				t.Errorf("Urgency mismatch: got %q, want %q", decoded.Urgency, level)
			}
		})
	}
}

// TestNotificationMessageAllEventTypes verifies that all event types
// survive the JSON round-trip correctly.
func TestNotificationMessageAllEventTypes(t *testing.T) {
	eventTypes := []EventType{
		EventThresholdCrossed,
		EventThresholdCleared,
		EventForecastChanged,
		EventImminentAlert,
		EventDigest,
		EventSystemAlert,
		EventBillingWarning,
		EventBillingReceipt,
	}

	for _, et := range eventTypes {
		t.Run(string(et), func(t *testing.T) {
			msg := NotificationMessage{
				NotificationID: "notif_event_test",
				EventType:      et,
			}

			data, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("Marshal failed: %v", err)
			}

			var decoded NotificationMessage
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}

			if decoded.EventType != et {
				t.Errorf("EventType mismatch: got %q, want %q", decoded.EventType, et)
			}
		})
	}
}
