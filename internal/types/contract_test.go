package types

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"
	"time"
)

// snakeCaseRegexp matches strings that are strictly snake_case:
// lowercase letters, digits, and underscores only. Single-word keys
// like "lat" or "page" are valid snake_case.
var snakeCaseRegexp = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// isSnakeCase returns true if the key conforms to strict snake_case convention.
func isSnakeCase(key string) bool {
	return snakeCaseRegexp.MatchString(key)
}

// assertAllKeysSnakeCase recursively walks a JSON value and asserts that every
// object key is strictly snake_case. The path parameter tracks the JSON path
// for clear error messages (e.g., "ordering.event_sequence").
func assertAllKeysSnakeCase(t *testing.T, path string, v interface{}) {
	t.Helper()

	switch val := v.(type) {
	case map[string]interface{}:
		for key, child := range val {
			fullPath := key
			if path != "" {
				fullPath = path + "." + key
			}
			if !isSnakeCase(key) {
				t.Errorf("JSON key %q at path %q is not snake_case", key, fullPath)
			}
			assertAllKeysSnakeCase(t, fullPath, child)
		}
	case []interface{}:
		for i, item := range val {
			itemPath := fmt.Sprintf("%s[%d]", path, i)
			assertAllKeysSnakeCase(t, itemPath, item)
		}
	// Scalar types (string, float64, bool, nil) have no keys to check.
	default:
	}
}

// TestEvalMessageSnakeCaseContract verifies that all JSON keys produced by
// marshalling EvalMessage are strictly snake_case, as required by Section 13
// of 01-foundation-types.md for the Go-to-Python cross-language contract.
//
// This test will fail if any struct field is missing a json tag (Go defaults
// to PascalCase field names) or if a tag uses camelCase.
func TestEvalMessageSnakeCaseContract(t *testing.T) {
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
		t.Fatalf("Failed to marshal EvalMessage: %v", err)
	}

	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to unmarshal EvalMessage to interface{}: %v", err)
	}

	assertAllKeysSnakeCase(t, "", raw)

	// Verify the expected number of keys to catch missing fields.
	// When Action and SpecificWatchPointIDs are populated, all 10 fields
	// should be present.
	topLevel, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatal("EvalMessage did not marshal to a JSON object")
	}

	expectedKeys := 10
	if len(topLevel) != expectedKeys {
		t.Errorf("EvalMessage has %d top-level keys, expected %d; fields may be missing json tags",
			len(topLevel), expectedKeys)
	}
}

// TestEvalMessageOmittedFieldsSnakeCaseContract verifies that when omitempty
// fields are zero-valued and thus omitted, the remaining keys are still
// strictly snake_case.
func TestEvalMessageOmittedFieldsSnakeCaseContract(t *testing.T) {
	msg := EvalMessage{
		BatchID:      "batch_001",
		TraceID:      "trace_abc",
		ForecastType: ForecastMediumRange,
		RunTimestamp: time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
		TileID:       "tile_39_-77",
		Page:         1,
		PageSize:     50,
		TotalItems:   120,
		// Action and SpecificWatchPointIDs intentionally omitted (zero values).
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Failed to marshal EvalMessage: %v", err)
	}

	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to unmarshal EvalMessage to interface{}: %v", err)
	}

	assertAllKeysSnakeCase(t, "", raw)

	// With omitempty fields absent, expect 8 keys.
	topLevel, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatal("EvalMessage did not marshal to a JSON object")
	}

	expectedKeys := 8
	if len(topLevel) != expectedKeys {
		t.Errorf("EvalMessage (omitted fields) has %d top-level keys, expected %d",
			len(topLevel), expectedKeys)
	}
}

// TestNotificationPayloadSnakeCaseContract verifies that all JSON keys produced
// by marshalling NotificationPayload (and all nested structures) are strictly
// snake_case, as required by Section 13 of 01-foundation-types.md.
//
// This exercises the full depth of the notification payload: LocationSnapshot,
// TimeWindow, ForecastSnapshot, ConditionResult, OrderingMetadata.
func TestNotificationPayloadSnakeCaseContract(t *testing.T) {
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
		t.Fatalf("Failed to marshal NotificationPayload: %v", err)
	}

	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to unmarshal NotificationPayload to interface{}: %v", err)
	}

	assertAllKeysSnakeCase(t, "", raw)

	// Verify expected top-level key count (13 with time_window present).
	topLevel, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatal("NotificationPayload did not marshal to a JSON object")
	}

	expectedTopLevelKeys := 13
	if len(topLevel) != expectedTopLevelKeys {
		t.Errorf("NotificationPayload has %d top-level keys, expected %d; fields may be missing json tags",
			len(topLevel), expectedTopLevelKeys)
	}

	// Verify nested structures are present and have the correct number of keys.
	// This catches cases where nested structs are missing json tags.
	nestedChecks := map[string]int{
		"location":          3, // lat, lon, display_name
		"time_window":       2, // start, end
		"forecast_snapshot": 5, // precipitation_probability, precipitation_mm, temperature_c, wind_speed_kmh, humidity_percent
		"ordering":          3, // event_sequence, forecast_timestamp, eval_timestamp
	}

	for key, expectedCount := range nestedChecks {
		nested, ok := topLevel[key].(map[string]interface{})
		if !ok {
			t.Errorf("Expected %q to be a JSON object", key)
			continue
		}
		if len(nested) != expectedCount {
			t.Errorf("Nested object %q has %d keys, expected %d", key, len(nested), expectedCount)
		}
	}

	// Verify conditions_evaluated array element key count.
	conditions, ok := topLevel["conditions_evaluated"].([]interface{})
	if !ok {
		t.Fatal("Expected conditions_evaluated to be an array")
	}
	if len(conditions) != 1 {
		t.Fatalf("Expected 1 condition, got %d", len(conditions))
	}
	condObj, ok := conditions[0].(map[string]interface{})
	if !ok {
		t.Fatal("Expected condition element to be a JSON object")
	}
	// 6 keys: variable, operator, threshold, actual_value, previous_value, matched
	expectedCondKeys := 6
	if len(condObj) != expectedCondKeys {
		t.Errorf("ConditionResult has %d keys, expected %d", len(condObj), expectedCondKeys)
	}
}

// TestNotificationPayloadOmittedFieldsSnakeCaseContract verifies the contract
// holds when optional fields (time_window, previous_value) are omitted.
func TestNotificationPayloadOmittedFieldsSnakeCaseContract(t *testing.T) {
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

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
		TimeWindow: nil, // omitempty: should be absent
		Forecast: ForecastSnapshot{
			PrecipitationProb: 85.0,
			PrecipitationMM:   12.5,
			TemperatureC:      22.0,
			WindSpeedKmh:      30.0,
			Humidity:          70.0,
		},
		Conditions: []ConditionResult{
			{
				Variable:      "wind_speed_kmh",
				Operator:      OpGreaterThan,
				Threshold:     []float64{50.0},
				ActualValue:   55.0,
				PreviousValue: nil, // omitempty: should be absent
				Matched:       true,
			},
		},
		Urgency:     UrgencyCritical,
		SourceModel: ForecastNowcast,
		Ordering: OrderingMetadata{
			EventSequence:     1,
			ForecastTimestamp: now,
			EvalTimestamp:     now,
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal NotificationPayload: %v", err)
	}

	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to unmarshal NotificationPayload to interface{}: %v", err)
	}

	assertAllKeysSnakeCase(t, "", raw)

	topLevel, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatal("NotificationPayload did not marshal to a JSON object")
	}

	// Without time_window: 12 top-level keys.
	expectedTopLevelKeys := 12
	if len(topLevel) != expectedTopLevelKeys {
		t.Errorf("NotificationPayload (no time_window) has %d top-level keys, expected %d",
			len(topLevel), expectedTopLevelKeys)
	}

	// Verify time_window is absent.
	if _, ok := topLevel["time_window"]; ok {
		t.Error("time_window should be omitted when nil")
	}

	// Verify condition's previous_value is absent.
	conditions := topLevel["conditions_evaluated"].([]interface{})
	condObj := conditions[0].(map[string]interface{})
	if _, ok := condObj["previous_value"]; ok {
		t.Error("previous_value should be omitted when nil")
	}
	// Without previous_value: 5 keys in ConditionResult.
	if len(condObj) != 5 {
		t.Errorf("ConditionResult (no previous_value) has %d keys, expected 5", len(condObj))
	}
}

// TestSnakeCaseHelperFunction validates the isSnakeCase helper itself to ensure
// the contract test's foundation is correct.
func TestSnakeCaseHelperFunction(t *testing.T) {
	valid := []string{
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
		"lat",
		"lon",
		"display_name",
		"start",
		"end",
		"precipitation_probability",
		"precipitation_mm",
		"temperature_c",
		"wind_speed_kmh",
		"humidity_percent",
		"variable",
		"operator",
		"threshold",
		"actual_value",
		"previous_value",
		"matched",
		"event_sequence",
		"forecast_timestamp",
		"eval_timestamp",
	}

	for _, key := range valid {
		if !isSnakeCase(key) {
			t.Errorf("Expected %q to be valid snake_case", key)
		}
	}

	invalid := []string{
		"BatchID",        // PascalCase (missing json tag)
		"batchId",        // camelCase
		"TraceID",        // PascalCase
		"ForecastType",   // PascalCase
		"RunTimestamp",    // PascalCase
		"TileID",         // PascalCase
		"PageSize",       // PascalCase
		"TotalItems",     // PascalCase
		"notificationId", // camelCase
		"watchpointId",   // camelCase
		"_leading",       // leading underscore
		"trailing_",      // trailing underscore
		"double__under",  // double underscore
		"ALLCAPS",        // all caps
		"mixedCASE",      // mixed case
	}

	for _, key := range invalid {
		if isSnakeCase(key) {
			t.Errorf("Expected %q to be invalid snake_case", key)
		}
	}
}
