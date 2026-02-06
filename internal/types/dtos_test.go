package types

import (
	"encoding/json"
	"testing"
	"time"
)

// TestNotificationPayloadJSONRoundTrip verifies that NotificationPayload
// serializes to JSON with the exact keys expected by the Python Eval Worker
// and notification workers. This is the CRITICAL cross-language contract.
func TestNotificationPayloadJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	prev := 15.0

	payload := NotificationPayload{
		NotificationID: "notif_abc123",
		WatchPointID:   "wp_xyz789",
		WatchPointName: "Downtown Sensor",
		Timezone:       "America/New_York",
		EventType:      EventThresholdCrossed,
		TriggeredAt:    now,
		Location: LocationSnapshot{
			Lat:         40.7128,
			Lon:         -74.0060,
			DisplayName: "New York, NY",
		},
		TimeWindow: &TimeWindow{
			Start: now,
			End:   now.Add(24 * time.Hour),
		},
		Forecast: ForecastSnapshot{
			PrecipitationProb: 85.0,
			PrecipitationMM:   12.5,
			TemperatureC:      22.0,
			WindSpeedKmh:      30.0,
			Humidity:          70.0,
		},
		Conditions: []ConditionResult{
			{
				Variable:      "precipitation_probability",
				Operator:      OpGreaterThan,
				Threshold:     []float64{80.0},
				ActualValue:   85.0,
				PreviousValue: &prev,
				Matched:       true,
			},
		},
		Urgency:     UrgencyWarning,
		SourceModel: ForecastMediumRange,
		Ordering: OrderingMetadata{
			EventSequence:     42,
			ForecastTimestamp: now.Add(-1 * time.Hour),
			EvalTimestamp:     now,
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Verify critical JSON keys are present (cross-language contract)
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map failed: %v", err)
	}

	requiredKeys := []string{
		"notification_id",
		"watchpoint_id",
		"watchpoint_name",
		"timezone",
		"event_type",
		"triggered_at",
		"location",
		"time_window",
		"forecast_snapshot",
		"conditions_evaluated",
		"urgency",
		"source_model",
		"ordering",
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

	// Verify round-trip
	var decoded NotificationPayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Round-trip unmarshal failed: %v", err)
	}
	if decoded.NotificationID != payload.NotificationID {
		t.Errorf("NotificationID mismatch: got %q, want %q", decoded.NotificationID, payload.NotificationID)
	}
	if decoded.WatchPointID != payload.WatchPointID {
		t.Errorf("WatchPointID mismatch: got %q, want %q", decoded.WatchPointID, payload.WatchPointID)
	}
	if decoded.EventType != payload.EventType {
		t.Errorf("EventType mismatch: got %q, want %q", decoded.EventType, payload.EventType)
	}
	if decoded.Urgency != payload.Urgency {
		t.Errorf("Urgency mismatch: got %q, want %q", decoded.Urgency, payload.Urgency)
	}
	if decoded.SourceModel != payload.SourceModel {
		t.Errorf("SourceModel mismatch: got %q, want %q", decoded.SourceModel, payload.SourceModel)
	}
	if decoded.Ordering.EventSequence != payload.Ordering.EventSequence {
		t.Errorf("EventSequence mismatch: got %d, want %d", decoded.Ordering.EventSequence, payload.Ordering.EventSequence)
	}
	if len(decoded.Conditions) != 1 {
		t.Fatalf("Expected 1 condition, got %d", len(decoded.Conditions))
	}
	if decoded.Conditions[0].PreviousValue == nil {
		t.Error("PreviousValue should not be nil after round-trip")
	} else if *decoded.Conditions[0].PreviousValue != prev {
		t.Errorf("PreviousValue mismatch: got %f, want %f", *decoded.Conditions[0].PreviousValue, prev)
	}
}

// TestNotificationPayloadOmitsNilTimeWindow verifies that time_window
// is omitted from JSON when nil (omitempty behavior).
func TestNotificationPayloadOmitsNilTimeWindow(t *testing.T) {
	payload := NotificationPayload{
		NotificationID: "notif_test",
		WatchPointID:   "wp_test",
		TimeWindow:     nil, // Should be omitted
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if _, ok := raw["time_window"]; ok {
		t.Error("time_window should be omitted when nil")
	}
}

// TestConditionResultPreviousValueOmitempty verifies that previous_value
// is omitted when nil.
func TestConditionResultPreviousValueOmitempty(t *testing.T) {
	cr := ConditionResult{
		Variable:      "temperature_c",
		Operator:      OpGreaterThan,
		Threshold:     []float64{30.0},
		ActualValue:   35.0,
		PreviousValue: nil, // Should be omitted
		Matched:       true,
	}

	data, err := json.Marshal(cr)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if _, ok := raw["previous_value"]; ok {
		t.Error("previous_value should be omitted when nil")
	}
}

// TestMonitorSummaryJSONKeys verifies the MonitorSummary JSON contract
// between the Python Eval Worker (writer) and Go Digest Generator (reader).
func TestMonitorSummaryJSONKeys(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	summary := MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(6 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob": 80.0,
			"wind_kmh":    45.0,
		},
		TriggeredPeriods: []TimeRange{
			{Start: now.Add(1 * time.Hour), End: now.Add(3 * time.Hour)},
		},
	}

	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	for _, key := range []string{"window_start", "window_end", "max_values", "triggered_periods"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("Missing required MonitorSummary JSON key: %q", key)
		}
	}

	// Verify round-trip
	var decoded MonitorSummary
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Round-trip unmarshal failed: %v", err)
	}
	if len(decoded.MaxValues) != 2 {
		t.Errorf("Expected 2 max values, got %d", len(decoded.MaxValues))
	}
	if len(decoded.TriggeredPeriods) != 1 {
		t.Errorf("Expected 1 triggered period, got %d", len(decoded.TriggeredPeriods))
	}
}

// TestSubscriptionDetailsJSONRoundTrip verifies billing type serialization.
func TestSubscriptionDetailsJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	sub := SubscriptionDetails{
		Plan:               PlanPro,
		Status:             SubStatusActive,
		CurrentPeriodStart: now,
		CurrentPeriodEnd:   now.AddDate(0, 1, 0),
		CancelAtPeriodEnd:  false,
		PaymentMethod: &PaymentMethodInfo{
			Type:     "card",
			Last4:    "4242",
			ExpMonth: 12,
			ExpYear:  2027,
		},
	}

	data, err := json.Marshal(sub)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	for _, key := range []string{"plan", "status", "current_period_start", "current_period_end", "cancel_at_period_end", "payment_method"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("Missing required SubscriptionDetails JSON key: %q", key)
		}
	}

	// Verify round-trip
	var decoded SubscriptionDetails
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Round-trip unmarshal failed: %v", err)
	}
	if decoded.Plan != PlanPro {
		t.Errorf("Plan mismatch: got %q, want %q", decoded.Plan, PlanPro)
	}
	if decoded.Status != SubStatusActive {
		t.Errorf("Status mismatch: got %q, want %q", decoded.Status, SubStatusActive)
	}
	if decoded.PaymentMethod == nil {
		t.Fatal("PaymentMethod should not be nil")
	}
	if decoded.PaymentMethod.Last4 != "4242" {
		t.Errorf("Last4 mismatch: got %q, want %q", decoded.PaymentMethod.Last4, "4242")
	}
}

// TestSubscriptionDetailsOmitsNilPaymentMethod verifies omitempty on PaymentMethod.
func TestSubscriptionDetailsOmitsNilPaymentMethod(t *testing.T) {
	sub := SubscriptionDetails{
		Plan:          PlanFree,
		Status:        SubStatusActive,
		PaymentMethod: nil, // Should be omitted
	}

	data, err := json.Marshal(sub)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if _, ok := raw["payment_method"]; ok {
		t.Error("payment_method should be omitted when nil")
	}
}

// TestUsageSnapshotJSONRoundTrip verifies billing usage types.
func TestUsageSnapshotJSONRoundTrip(t *testing.T) {
	nextReset := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	snapshot := UsageSnapshot{
		ResourceUsage: map[ResourceType]int{
			ResourceWatchPoints: 5,
			ResourceAPICalls:    1000,
		},
		LimitDetails: map[ResourceType]LimitDetail{
			ResourceWatchPoints: {
				Limit:     10,
				Used:      5,
				ResetType: ResetNever,
			},
			ResourceAPICalls: {
				Limit:     5000,
				Used:      1000,
				ResetType: ResetDaily,
				NextReset: &nextReset,
			},
		},
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded UsageSnapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Round-trip unmarshal failed: %v", err)
	}

	if decoded.ResourceUsage[ResourceWatchPoints] != 5 {
		t.Errorf("WatchPoints usage mismatch: got %d, want 5", decoded.ResourceUsage[ResourceWatchPoints])
	}
	if decoded.LimitDetails[ResourceAPICalls].ResetType != ResetDaily {
		t.Errorf("ResetType mismatch: got %q, want %q", decoded.LimitDetails[ResourceAPICalls].ResetType, ResetDaily)
	}
}

// TestEvalMessageJSONRoundTrip verifies the Batcher-to-EvalWorker SQS contract.
func TestEvalMessageJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	msg := EvalMessage{
		BatchID:               "batch_001",
		TraceID:               "trace_abc",
		ForecastType:          ForecastMediumRange,
		RunTimestamp:          now,
		TileID:                "tile_39_-77",
		Page:                  1,
		PageSize:              50,
		TotalItems:            120,
		Action:                EvalActionEvaluate,
		SpecificWatchPointIDs: []string{"wp_1", "wp_2"},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify snake_case keys for cross-language compatibility
	requiredKeys := []string{
		"batch_id",
		"trace_id",
		"forecast_type",
		"run_timestamp",
		"tile_id",
		"page",
		"page_size",
		"total_items",
		"action",
		"specific_watchpoint_ids",
	}

	for _, key := range requiredKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("Missing required EvalMessage JSON key: %q", key)
		}
	}

	// Verify round-trip
	var decoded EvalMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Round-trip unmarshal failed: %v", err)
	}
	if decoded.BatchID != msg.BatchID {
		t.Errorf("BatchID mismatch: got %q, want %q", decoded.BatchID, msg.BatchID)
	}
	if decoded.Action != EvalActionEvaluate {
		t.Errorf("Action mismatch: got %q, want %q", decoded.Action, EvalActionEvaluate)
	}
	if len(decoded.SpecificWatchPointIDs) != 2 {
		t.Errorf("SpecificWatchPointIDs count mismatch: got %d, want 2", len(decoded.SpecificWatchPointIDs))
	}
}

// TestEvalMessageOmitsEmptyAction verifies action is omitted when empty (omitempty).
func TestEvalMessageOmitsEmptyAction(t *testing.T) {
	msg := EvalMessage{
		BatchID: "batch_001",
		TileID:  "tile_39_-77",
		// Action defaults to zero value, should be omitted
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if _, ok := raw["action"]; ok {
		t.Error("action should be omitted when empty (omitempty)")
	}
}

// TestEventEnvelopeJSONRoundTrip verifies the internal event wrapper.
func TestEventEnvelopeJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	env := EventEnvelope{
		EventID:   "evt_123",
		EventType: "forecast.ready",
		Timestamp: now,
		Source:    "batcher",
		Version:   "1.0",
		Payload:   json.RawMessage(`{"tile_id":"tile_39"}`),
		Metadata: &EventMetadata{
			CorrelationID: "corr_abc",
			TraceID:       "trace_xyz",
		},
	}

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	for _, key := range []string{"event_id", "event_type", "timestamp", "source", "version", "payload", "metadata"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("Missing required EventEnvelope JSON key: %q", key)
		}
	}

	// Verify round-trip
	var decoded EventEnvelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Round-trip unmarshal failed: %v", err)
	}
	if decoded.EventID != env.EventID {
		t.Errorf("EventID mismatch: got %q, want %q", decoded.EventID, env.EventID)
	}
	if decoded.Metadata == nil {
		t.Fatal("Metadata should not be nil")
	}
	if decoded.Metadata.CorrelationID != "corr_abc" {
		t.Errorf("CorrelationID mismatch: got %q, want %q", decoded.Metadata.CorrelationID, "corr_abc")
	}
}

// TestEventEnvelopeOmitsNilMetadata verifies omitempty on Metadata.
func TestEventEnvelopeOmitsNilMetadata(t *testing.T) {
	env := EventEnvelope{
		EventID:  "evt_123",
		Metadata: nil, // Should be omitted
	}

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if _, ok := raw["metadata"]; ok {
		t.Error("metadata should be omitted when nil")
	}
}

// TestOAuthProfileFieldPresence verifies the auth DTO structure.
func TestOAuthProfileFieldPresence(t *testing.T) {
	profile := OAuthProfile{
		Provider:      "google",
		ProviderID:    "123456789",
		Email:         "user@example.com",
		Name:          "Test User",
		AvatarURL:     "https://example.com/avatar.png",
		EmailVerified: true,
	}

	// OAuthProfile intentionally has no JSON tags (internal only),
	// but verify all fields are accessible and correctly typed.
	if profile.Provider != "google" {
		t.Errorf("Provider mismatch: got %q, want %q", profile.Provider, "google")
	}
	if !profile.EmailVerified {
		t.Error("EmailVerified should be true")
	}
}

// TestSendInputFieldPresence verifies the email worker DTO structure.
func TestSendInputFieldPresence(t *testing.T) {
	input := SendInput{
		To: "user@example.com",
		From: SenderIdentity{
			Name:    "WatchPoint Alerts",
			Address: "alerts@watchpoint.io",
		},
		TemplateID: "threshold_crossed",
		TemplateData: map[string]interface{}{
			"watchpoint_name": "Downtown Sensor",
		},
		ReferenceID: "notif_abc123",
	}

	if input.From.Name != "WatchPoint Alerts" {
		t.Errorf("From.Name mismatch: got %q, want %q", input.From.Name, "WatchPoint Alerts")
	}
	if input.ReferenceID != "notif_abc123" {
		t.Errorf("ReferenceID mismatch: got %q, want %q", input.ReferenceID, "notif_abc123")
	}
}

// TestVerificationResultJSONRoundTrip verifies scheduled job DTO.
func TestVerificationResultJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	vr := VerificationResult{
		ID:            1,
		ForecastRunID: "run_abc",
		LocationID:    "loc_123",
		MetricType:    "rmse",
		Variable:      "temperature_c",
		Value:         1.23,
		ComputedAt:    now,
	}

	data, err := json.Marshal(vr)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	for _, key := range []string{"id", "forecast_run_id", "location_id", "metric_type", "variable", "value", "computed_at"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("Missing required VerificationResult JSON key: %q", key)
		}
	}
}

// TestInvoiceJSONRoundTrip verifies invoice billing type.
func TestInvoiceJSONRoundTrip(t *testing.T) {
	paidAt := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)

	inv := Invoice{
		ID:          "inv_001",
		AmountCents: 4999,
		Status:      "paid",
		PeriodStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		PDFURL:      "https://stripe.com/invoice/pdf/inv_001",
		PaidAt:      &paidAt,
	}

	data, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	for _, key := range []string{"id", "amount_cents", "status", "period_start", "period_end", "pdf_url", "paid_at"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("Missing required Invoice JSON key: %q", key)
		}
	}

	// Verify round-trip
	var decoded Invoice
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Round-trip unmarshal failed: %v", err)
	}
	if decoded.AmountCents != 4999 {
		t.Errorf("AmountCents mismatch: got %d, want 4999", decoded.AmountCents)
	}
}

// TestInvoiceOmitsNilPaidAt verifies omitempty on PaidAt.
func TestInvoiceOmitsNilPaidAt(t *testing.T) {
	inv := Invoice{
		ID:     "inv_open",
		Status: "open",
		PaidAt: nil,
	}

	data, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if _, ok := raw["paid_at"]; ok {
		t.Error("paid_at should be omitted when nil")
	}
}

// TestDeliveryStatusValues verifies all delivery status constants have correct string values.
func TestDeliveryStatusValues(t *testing.T) {
	tests := []struct {
		status   DeliveryStatus
		expected string
	}{
		{DeliveryStatusPending, "pending"},
		{DeliveryStatusSent, "sent"},
		{DeliveryStatusFailed, "failed"},
		{DeliveryStatusBounced, "bounced"},
		{DeliveryStatusRetrying, "retrying"},
		{DeliveryStatusSkipped, "skipped"},
		{DeliveryStatusDeferred, "deferred"},
	}

	for _, tt := range tests {
		if string(tt.status) != tt.expected {
			t.Errorf("DeliveryStatus %q != expected %q", tt.status, tt.expected)
		}
	}
}

// TestLocationSnapshotJSONKeys verifies the location snapshot JSON contract.
func TestLocationSnapshotJSONKeys(t *testing.T) {
	loc := LocationSnapshot{
		Lat:         40.7128,
		Lon:         -74.0060,
		DisplayName: "New York, NY",
	}

	data, err := json.Marshal(loc)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	for _, key := range []string{"lat", "lon", "display_name"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("Missing required LocationSnapshot JSON key: %q", key)
		}
	}
}

// TestNotificationHistoryItemJSONKeys verifies the history DTO JSON contract.
func TestNotificationHistoryItemJSONKeys(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	sentAt := now.Add(1 * time.Minute)

	item := NotificationHistoryItem{
		ID:        "notif_hist_1",
		EventType: EventThresholdCrossed,
		SentAt:    now,
		ForecastSnapshot: ForecastSnapshot{
			PrecipitationProb: 85.0,
			TemperatureC:      22.0,
		},
		TriggeredConditions: []Condition{
			{Variable: "temperature_c", Operator: OpGreaterThan, Threshold: []float64{20.0}, Unit: "celsius"},
		},
		Channels: []DeliverySummary{
			{Channel: "email", Status: "sent", SentAt: &sentAt},
		},
	}

	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	for _, key := range []string{"id", "event_type", "sent_at", "forecast_snapshot", "triggered_conditions", "channels"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("Missing required NotificationHistoryItem JSON key: %q", key)
		}
	}
}

// TestJobRunJSONRoundTrip verifies the scheduled job DTO.
func TestJobRunJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	finished := now.Add(5 * time.Minute)

	jr := JobRun{
		ID:         1,
		JobType:    "archiver",
		StartedAt:  now,
		FinishedAt: &finished,
		Status:     "success",
		ItemsCount: 42,
		Error:      "",
		Metadata:   json.RawMessage(`{"archived_count": 42}`),
	}

	data, err := json.Marshal(jr)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded JobRun
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Round-trip unmarshal failed: %v", err)
	}

	if decoded.JobType != "archiver" {
		t.Errorf("JobType mismatch: got %q, want %q", decoded.JobType, "archiver")
	}
	if decoded.ItemsCount != 42 {
		t.Errorf("ItemsCount mismatch: got %d, want 42", decoded.ItemsCount)
	}
}
