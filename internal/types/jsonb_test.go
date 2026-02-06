package types

import (
	"database/sql/driver"
	"encoding/json"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// scanValuerRoundTrip is a generic helper that tests the Value -> Scan round trip.
func scanValuerRoundTrip(t *testing.T, name string, valuer driver.Valuer, scanner interface{ Scan(interface{}) error }) {
	t.Helper()
	dv, err := valuer.Value()
	if err != nil {
		t.Fatalf("%s: Value() returned error: %v", name, err)
	}
	if err := scanner.Scan(dv); err != nil {
		t.Fatalf("%s: Scan() returned error: %v", name, err)
	}
}

// ---------------------------------------------------------------------------
// ChannelList
// ---------------------------------------------------------------------------

func TestChannelList_ScanValue_RoundTrip(t *testing.T) {
	original := ChannelList{
		{
			ID:      "ch-001",
			Type:    ChannelEmail,
			Config:  map[string]any{"address": "user@example.com"},
			Enabled: true,
		},
		{
			ID:      "ch-002",
			Type:    ChannelWebhook,
			Config:  map[string]any{"url": "https://hooks.example.com/test", "secret": "s3cret"},
			Enabled: false,
		},
	}

	dv, err := original.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}

	// Value should produce []byte (JSON)
	jsonBytes, ok := dv.([]byte)
	if !ok {
		t.Fatalf("Value() did not return []byte, got %T", dv)
	}

	// Verify the secret is NOT redacted in the database representation.
	// The Channel.MarshalJSON redacts secrets for API responses, but Value()
	// must store the full config including secrets.
	if !contains(jsonBytes, "s3cret") {
		t.Fatalf("Value() should NOT redact secrets for database storage, got: %s", string(jsonBytes))
	}

	var scanned ChannelList
	if err := scanned.Scan(jsonBytes); err != nil {
		t.Fatalf("Scan([]byte) error: %v", err)
	}

	if len(scanned) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(scanned))
	}
	if scanned[0].ID != "ch-001" {
		t.Errorf("expected channel ID 'ch-001', got %q", scanned[0].ID)
	}
	if scanned[0].Type != ChannelEmail {
		t.Errorf("expected channel type 'email', got %q", scanned[0].Type)
	}
	if scanned[1].Config["secret"] != "s3cret" {
		t.Errorf("expected secret to survive round trip, got %v", scanned[1].Config["secret"])
	}
}

func TestChannelList_Scan_NilValue(t *testing.T) {
	cl := ChannelList{{ID: "pre-existing"}}
	if err := cl.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	if cl != nil {
		t.Errorf("expected nil after scanning nil, got %v", cl)
	}
}

func TestChannelList_Value_Nil(t *testing.T) {
	var cl ChannelList
	dv, err := cl.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	if dv != nil {
		t.Errorf("expected nil value for nil ChannelList, got %v", dv)
	}
}

func TestChannelList_Scan_StringInput(t *testing.T) {
	jsonStr := `[{"id":"ch-str","type":"email","config":{},"enabled":true}]`
	var cl ChannelList
	if err := cl.Scan(jsonStr); err != nil {
		t.Fatalf("Scan(string) error: %v", err)
	}
	if len(cl) != 1 || cl[0].ID != "ch-str" {
		t.Errorf("unexpected result from string scan: %v", cl)
	}
}

func TestChannelList_Scan_UnsupportedType(t *testing.T) {
	var cl ChannelList
	if err := cl.Scan(12345); err == nil {
		t.Fatal("expected error for unsupported scan type, got nil")
	}
}

// ---------------------------------------------------------------------------
// MonitorConfig
// ---------------------------------------------------------------------------

func TestMonitorConfig_ScanValue_RoundTrip(t *testing.T) {
	original := MonitorConfig{
		WindowHours: 24,
		ActiveHours: [][2]int{{6, 18}, {20, 22}},
		ActiveDays:  []int{1, 2, 3, 4, 5},
	}

	dv, err := original.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}

	var scanned MonitorConfig
	if err := scanned.Scan(dv); err != nil {
		t.Fatalf("Scan() error: %v", err)
	}

	if scanned.WindowHours != 24 {
		t.Errorf("expected WindowHours 24, got %d", scanned.WindowHours)
	}
	if len(scanned.ActiveHours) != 2 {
		t.Errorf("expected 2 ActiveHours entries, got %d", len(scanned.ActiveHours))
	}
	if scanned.ActiveHours[0] != [2]int{6, 18} {
		t.Errorf("expected ActiveHours[0] = [6,18], got %v", scanned.ActiveHours[0])
	}
	if len(scanned.ActiveDays) != 5 {
		t.Errorf("expected 5 ActiveDays, got %d", len(scanned.ActiveDays))
	}
}

func TestMonitorConfig_Scan_NilValue(t *testing.T) {
	mc := MonitorConfig{WindowHours: 99}
	if err := mc.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	// Struct fields should remain unchanged when scanning nil (no unmarshal target)
	if mc.WindowHours != 99 {
		t.Errorf("expected WindowHours to remain 99 after nil scan, got %d", mc.WindowHours)
	}
}

func TestMonitorConfig_Scan_StringInput(t *testing.T) {
	jsonStr := `{"window_hours":48,"active_hours":[[8,20]],"active_days":[1,2,3]}`
	var mc MonitorConfig
	if err := mc.Scan(jsonStr); err != nil {
		t.Fatalf("Scan(string) error: %v", err)
	}
	if mc.WindowHours != 48 {
		t.Errorf("expected WindowHours 48, got %d", mc.WindowHours)
	}
}

// ---------------------------------------------------------------------------
// PlanLimits
// ---------------------------------------------------------------------------

func TestPlanLimits_ScanValue_RoundTrip(t *testing.T) {
	original := PlanLimits{
		MaxWatchPoints:   100,
		MaxAPICallsDaily: 10000,
		AllowNowcast:     true,
	}

	var scanned PlanLimits
	scanValuerRoundTrip(t, "PlanLimits", original, &scanned)

	if scanned.MaxWatchPoints != 100 {
		t.Errorf("expected MaxWatchPoints 100, got %d", scanned.MaxWatchPoints)
	}
	if scanned.MaxAPICallsDaily != 10000 {
		t.Errorf("expected MaxAPICallsDaily 10000, got %d", scanned.MaxAPICallsDaily)
	}
	if !scanned.AllowNowcast {
		t.Error("expected AllowNowcast to be true")
	}
}

func TestPlanLimits_Scan_NilValue(t *testing.T) {
	pl := PlanLimits{MaxWatchPoints: 5}
	if err := pl.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	// scanJSONB returns nil for nil, so struct should be unchanged
	if pl.MaxWatchPoints != 5 {
		t.Errorf("expected MaxWatchPoints to remain 5, got %d", pl.MaxWatchPoints)
	}
}

// ---------------------------------------------------------------------------
// NotificationPreferences
// ---------------------------------------------------------------------------

func TestNotificationPreferences_ScanValue_RoundTrip(t *testing.T) {
	original := NotificationPreferences{
		QuietHours: &QuietHoursConfig{
			Enabled: true,
			Schedule: []QuietPeriod{
				{Days: []string{"mon", "tue"}, Start: "22:00", End: "07:00"},
			},
			Timezone: "America/New_York",
		},
		Digest: &DigestConfig{
			Enabled:      true,
			Frequency:    "daily",
			DeliveryTime: "08:00",
			SendEmpty:    false,
			TemplateSet:  "digest_default",
		},
	}

	var scanned NotificationPreferences
	scanValuerRoundTrip(t, "NotificationPreferences", original, &scanned)

	if scanned.QuietHours == nil {
		t.Fatal("expected QuietHours to be non-nil")
	}
	if !scanned.QuietHours.Enabled {
		t.Error("expected QuietHours.Enabled to be true")
	}
	if scanned.QuietHours.Timezone != "America/New_York" {
		t.Errorf("expected timezone 'America/New_York', got %q", scanned.QuietHours.Timezone)
	}
	if scanned.Digest == nil {
		t.Fatal("expected Digest to be non-nil")
	}
	if scanned.Digest.Frequency != "daily" {
		t.Errorf("expected frequency 'daily', got %q", scanned.Digest.Frequency)
	}
}

func TestNotificationPreferences_Scan_NilValue(t *testing.T) {
	np := NotificationPreferences{Digest: &DigestConfig{Enabled: true}}
	if err := np.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	// Struct should remain unchanged for nil scan
	if np.Digest == nil || !np.Digest.Enabled {
		t.Error("expected Digest to remain unchanged after nil scan")
	}
}

// ---------------------------------------------------------------------------
// Preferences (per-WatchPoint)
// ---------------------------------------------------------------------------

func TestPreferences_ScanValue_RoundTrip(t *testing.T) {
	original := Preferences{
		NotifyOnClear:          true,
		NotifyOnForecastChange: false,
	}

	var scanned Preferences
	scanValuerRoundTrip(t, "Preferences", original, &scanned)

	if !scanned.NotifyOnClear {
		t.Error("expected NotifyOnClear to be true")
	}
	if scanned.NotifyOnForecastChange {
		t.Error("expected NotifyOnForecastChange to be false")
	}
}

func TestPreferences_Scan_NilValue(t *testing.T) {
	p := Preferences{NotifyOnClear: true}
	if err := p.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	if !p.NotifyOnClear {
		t.Error("expected NotifyOnClear to remain true after nil scan")
	}
}

// ---------------------------------------------------------------------------
// EvaluationResult
// ---------------------------------------------------------------------------

func TestEvaluationResult_ScanValue_RoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	original := EvaluationResult{
		Timestamp: now,
		Triggered: true,
		MatchedConditions: []Condition{
			{Variable: "temperature_c", Operator: OpGreaterThan, Threshold: []float64{35.0}, Unit: "celsius"},
		},
		ForecastSnapshot: ForecastSnapshot{
			PrecipitationProb: 60.0,
			PrecipitationMM:   12.5,
			TemperatureC:      37.2,
			WindSpeedKmh:      15.0,
			Humidity:          55.0,
		},
		ModelUsed: "medium_range",
	}

	var scanned EvaluationResult
	scanValuerRoundTrip(t, "EvaluationResult", original, &scanned)

	if !scanned.Triggered {
		t.Error("expected Triggered to be true")
	}
	if scanned.ModelUsed != "medium_range" {
		t.Errorf("expected ModelUsed 'medium_range', got %q", scanned.ModelUsed)
	}
	if len(scanned.MatchedConditions) != 1 {
		t.Fatalf("expected 1 matched condition, got %d", len(scanned.MatchedConditions))
	}
	if scanned.MatchedConditions[0].Variable != "temperature_c" {
		t.Errorf("expected variable 'temperature_c', got %q", scanned.MatchedConditions[0].Variable)
	}
	if scanned.ForecastSnapshot.TemperatureC != 37.2 {
		t.Errorf("expected TemperatureC 37.2, got %f", scanned.ForecastSnapshot.TemperatureC)
	}
	// Verify timestamp round trips correctly (JSON time format)
	if !scanned.Timestamp.Equal(now) {
		t.Errorf("expected timestamp %v, got %v", now, scanned.Timestamp)
	}
}

func TestEvaluationResult_Scan_NilValue(t *testing.T) {
	er := EvaluationResult{Triggered: true}
	if err := er.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	if !er.Triggered {
		t.Error("expected Triggered to remain true after nil scan")
	}
}

// ---------------------------------------------------------------------------
// Generic helpers (scanJSONB / valueJSONB edge cases)
// ---------------------------------------------------------------------------

func TestScanJSONB_InvalidJSON(t *testing.T) {
	var mc MonitorConfig
	err := mc.Scan([]byte(`{not valid json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestScanJSONB_UnsupportedType(t *testing.T) {
	var mc MonitorConfig
	err := mc.Scan(42)
	if err == nil {
		t.Fatal("expected error for unsupported scan type, got nil")
	}
}

func TestValueJSONB_ProducesValidJSON(t *testing.T) {
	mc := MonitorConfig{
		WindowHours: 12,
		ActiveHours: [][2]int{{9, 17}},
		ActiveDays:  []int{1, 2, 3},
	}
	dv, err := mc.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	b, ok := dv.([]byte)
	if !ok {
		t.Fatalf("expected []byte, got %T", dv)
	}
	if !json.Valid(b) {
		t.Errorf("Value() produced invalid JSON: %s", string(b))
	}
}

// ---------------------------------------------------------------------------
// Conditions (already implemented in conditions.go - verify consistency)
// ---------------------------------------------------------------------------

func TestConditions_ScanValue_Consistency(t *testing.T) {
	original := Conditions{
		{Variable: "wind_speed_kmh", Operator: OpGreaterThanEq, Threshold: []float64{50.0}, Unit: "kmh"},
		{Variable: "temperature_c", Operator: OpBetween, Threshold: []float64{-5.0, 5.0}, Unit: "celsius"},
	}

	dv, err := original.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}

	var scanned Conditions
	if err := scanned.Scan(dv); err != nil {
		t.Fatalf("Scan() error: %v", err)
	}

	if len(scanned) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(scanned))
	}
	if scanned[0].Variable != "wind_speed_kmh" {
		t.Errorf("expected variable 'wind_speed_kmh', got %q", scanned[0].Variable)
	}
	if scanned[1].Operator != OpBetween {
		t.Errorf("expected operator 'between', got %q", scanned[1].Operator)
	}
}

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

func contains(data []byte, substr string) bool {
	return len(data) > 0 && len(substr) > 0 && jsonContainsString(data, substr)
}

func jsonContainsString(data []byte, s string) bool {
	for i := 0; i <= len(data)-len(s); i++ {
		if string(data[i:i+len(s)]) == s {
			return true
		}
	}
	return false
}
