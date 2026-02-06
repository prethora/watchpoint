package email

import (
	"testing"
	"time"

	"watchpoint/internal/types"
)

// testLogger implements types.Logger for test use.
type testLogger struct {
	infos  []string
	warns  []string
	errors []string
}

func newTestLogger() *testLogger {
	return &testLogger{}
}

func (l *testLogger) Info(msg string, args ...any)  { l.infos = append(l.infos, msg) }
func (l *testLogger) Warn(msg string, args ...any)  { l.warns = append(l.warns, msg) }
func (l *testLogger) Error(msg string, args ...any) { l.errors = append(l.errors, msg) }
func (l *testLogger) With(args ...any) types.Logger { return l }

// --- TemplateConfig Tests ---

func TestTemplateConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  TemplateConfig
		wantErr bool
	}{
		{
			name: "valid config with all critical types",
			config: TemplateConfig{
				Sets: map[string]map[types.EventType]string{
					"default": {
						types.EventThresholdCrossed: "d-crossed-123",
						types.EventThresholdCleared: "d-cleared-456",
						types.EventSystemAlert:      "d-system-789",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "valid config with custom and default sets",
			config: TemplateConfig{
				Sets: map[string]map[types.EventType]string{
					"default": {
						types.EventThresholdCrossed: "d-default-crossed",
						types.EventThresholdCleared: "d-default-cleared",
						types.EventSystemAlert:      "d-default-system",
					},
					"wedding": {
						types.EventThresholdCrossed: "d-wedding-crossed",
					},
				},
			},
			wantErr: false,
		},
		{
			name:    "nil sets",
			config:  TemplateConfig{Sets: nil},
			wantErr: true,
		},
		{
			name: "missing default set",
			config: TemplateConfig{
				Sets: map[string]map[types.EventType]string{
					"wedding": {
						types.EventThresholdCrossed: "d-wedding-crossed",
					},
				},
			},
			wantErr: true,
		},
		{
			name: "default set missing threshold_crossed",
			config: TemplateConfig{
				Sets: map[string]map[types.EventType]string{
					"default": {
						types.EventThresholdCleared: "d-cleared",
						types.EventSystemAlert:      "d-system",
					},
				},
			},
			wantErr: true,
		},
		{
			name: "default set missing system_alert",
			config: TemplateConfig{
				Sets: map[string]map[types.EventType]string{
					"default": {
						types.EventThresholdCrossed: "d-crossed",
						types.EventThresholdCleared: "d-cleared",
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// --- NewTemplateEngine Tests ---

func TestNewTemplateEngine(t *testing.T) {
	validJSON := `{"sets":{"default":{"threshold_crossed":"d-123","threshold_cleared":"d-456","system_alert":"d-789"}}}`

	t.Run("valid JSON", func(t *testing.T) {
		engine, err := NewTemplateEngine(TemplateEngineConfig{
			TemplatesJSON:   validJSON,
			DefaultFromAddr: "alerts@watchpoint.io",
			DefaultFromName: "WatchPoint Alerts",
			Logger:          newTestLogger(),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if engine == nil {
			t.Fatal("expected non-nil engine")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		_, err := NewTemplateEngine(TemplateEngineConfig{
			TemplatesJSON: "not json",
			Logger:        newTestLogger(),
		})
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("valid JSON but missing default set", func(t *testing.T) {
		_, err := NewTemplateEngine(TemplateEngineConfig{
			TemplatesJSON: `{"sets":{"wedding":{"threshold_crossed":"d-123"}}}`,
			Logger:        newTestLogger(),
		})
		if err == nil {
			t.Fatal("expected error for missing default set")
		}
	})
}

// --- Resolve Tests ---

func TestTemplateEngineResolve(t *testing.T) {
	logger := newTestLogger()
	engine, err := NewTemplateEngine(TemplateEngineConfig{
		TemplatesJSON: `{
			"sets": {
				"default": {
					"threshold_crossed": "d-default-crossed",
					"threshold_cleared": "d-default-cleared",
					"system_alert": "d-default-system",
					"monitor_digest": "d-default-digest"
				},
				"wedding": {
					"threshold_crossed": "d-wedding-crossed"
				}
			}
		}`,
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	tests := []struct {
		name      string
		set       string
		eventType types.EventType
		want      string
		wantErr   bool
		wantWarn  bool // Whether a fallback warning is expected
	}{
		{
			name:      "exact match in default set",
			set:       "default",
			eventType: types.EventThresholdCrossed,
			want:      "d-default-crossed",
		},
		{
			name:      "exact match in custom set",
			set:       "wedding",
			eventType: types.EventThresholdCrossed,
			want:      "d-wedding-crossed",
		},
		{
			name:      "fallback from custom to default",
			set:       "wedding",
			eventType: types.EventThresholdCleared,
			want:      "d-default-cleared",
			wantWarn:  true,
		},
		{
			name:      "fallback from unknown set to default",
			set:       "nonexistent",
			eventType: types.EventThresholdCrossed,
			want:      "d-default-crossed",
			wantWarn:  true,
		},
		{
			name:      "event type not in any set",
			set:       "wedding",
			eventType: types.EventBillingWarning,
			wantErr:   true,
			wantWarn:  true,
		},
		{
			name:      "event type not in default set",
			set:       "default",
			eventType: types.EventBillingWarning,
			wantErr:   true,
		},
		{
			name:      "empty set name falls back to default",
			set:       "",
			eventType: types.EventThresholdCrossed,
			want:      "d-default-crossed",
			wantWarn:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset logger warnings.
			logger.warns = nil

			got, err := engine.Resolve(tt.set, tt.eventType)
			if (err != nil) != tt.wantErr {
				t.Errorf("Resolve() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Resolve() = %q, want %q", got, tt.want)
			}
			if tt.wantWarn && len(logger.warns) == 0 {
				t.Error("expected a warning log for fallback, got none")
			}
		})
	}
}

// --- Prepare Tests ---

func TestTemplateEnginePrepare(t *testing.T) {
	logger := newTestLogger()
	engine, err := NewTemplateEngine(TemplateEngineConfig{
		TemplatesJSON: `{
			"sets": {
				"default": {
					"threshold_crossed": "d-crossed",
					"threshold_cleared": "d-cleared",
					"system_alert": "d-system"
				}
			}
		}`,
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	t.Run("basic prepare with timezone", func(t *testing.T) {
		n := &types.Notification{
			ID:             "notif-123",
			WatchPointID:   "wp-456",
			OrganizationID: "org-789",
			EventType:      types.EventThresholdCrossed,
			Urgency:        types.UrgencyWarning,
			TemplateSet:    "default",
			Payload: map[string]interface{}{
				"timezone":    "America/New_York",
				"temperature": 35.5,
				"location":    "New York, NY",
			},
			CreatedAt: time.Date(2026, 2, 6, 15, 30, 0, 0, time.UTC),
		}

		data, sender, err := engine.Prepare(n)
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}

		// Check core metadata.
		if data["notification_id"] != "notif-123" {
			t.Errorf("notification_id = %v, want notif-123", data["notification_id"])
		}
		if data["watchpoint_id"] != "wp-456" {
			t.Errorf("watchpoint_id = %v, want wp-456", data["watchpoint_id"])
		}
		if data["event_type"] != "threshold_crossed" {
			t.Errorf("event_type = %v, want threshold_crossed", data["event_type"])
		}

		// Check payload passthrough.
		if data["temperature"] != 35.5 {
			t.Errorf("temperature = %v, want 35.5", data["temperature"])
		}
		if data["location"] != "New York, NY" {
			t.Errorf("location = %v, want New York, NY", data["location"])
		}

		// Check timezone name is set.
		if data["timezone_name"] != "America/New_York" {
			t.Errorf("timezone_name = %v, want America/New_York", data["timezone_name"])
		}

		// Check formatted date exists and is non-empty.
		if fd, ok := data["formatted_date"].(string); !ok || fd == "" {
			t.Errorf("formatted_date missing or empty: %v", data["formatted_date"])
		}

		// Check created_at formatting.
		if caf, ok := data["created_at_formatted"].(string); !ok || caf == "" {
			t.Errorf("created_at_formatted missing or empty: %v", data["created_at_formatted"])
		}

		// Check sender identity.
		if sender.Address != "alerts@watchpoint.io" {
			t.Errorf("sender.Address = %q, want alerts@watchpoint.io", sender.Address)
		}
		if sender.Name != "WatchPoint Alerts" {
			t.Errorf("sender.Name = %q, want WatchPoint Alerts", sender.Name)
		}
	})

	t.Run("prepare with UTC default timezone", func(t *testing.T) {
		n := &types.Notification{
			ID:        "notif-utc",
			EventType: types.EventThresholdCleared,
			Payload:   map[string]interface{}{},
		}

		data, _, err := engine.Prepare(n)
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}

		if data["timezone_name"] != "UTC" {
			t.Errorf("timezone_name = %v, want UTC", data["timezone_name"])
		}
	})

	t.Run("prepare with invalid timezone defaults to UTC", func(t *testing.T) {
		n := &types.Notification{
			ID:        "notif-invalid-tz",
			EventType: types.EventThresholdCleared,
			Payload: map[string]interface{}{
				"timezone": "Invalid/Timezone",
			},
		}

		data, _, err := engine.Prepare(n)
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}

		if data["timezone_name"] != "UTC" {
			t.Errorf("timezone_name = %v, want UTC", data["timezone_name"])
		}
	})

	t.Run("prepare with custom template set sender name", func(t *testing.T) {
		n := &types.Notification{
			ID:          "notif-custom",
			EventType:   types.EventThresholdCrossed,
			TemplateSet: "wedding",
			Payload:     map[string]interface{}{},
		}

		_, sender, err := engine.Prepare(n)
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}

		want := "WatchPoint Alerts (wedding)"
		if sender.Name != want {
			t.Errorf("sender.Name = %q, want %q", sender.Name, want)
		}
	})

	t.Run("prepare with nil notification", func(t *testing.T) {
		_, _, err := engine.Prepare(nil)
		if err == nil {
			t.Error("expected error for nil notification")
		}
	})

	t.Run("prepare with nil payload", func(t *testing.T) {
		n := &types.Notification{
			ID:        "notif-nil-payload",
			EventType: types.EventThresholdCrossed,
			Payload:   nil,
		}

		data, _, err := engine.Prepare(n)
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}

		if data["timezone_name"] != "UTC" {
			t.Errorf("timezone_name = %v, want UTC", data["timezone_name"])
		}
	})
}

// --- Probability Formatting Tests ---

func TestFormatPayloadNumbers(t *testing.T) {
	tests := []struct {
		name     string
		data     TemplateData
		field    string
		wantFmt  string
		wantHas  bool
	}{
		{
			name:    "probability 0.55 -> 55%",
			data:    TemplateData{"precipitation_probability": 0.55},
			field:   "precipitation_probability_formatted",
			wantFmt: "55%",
			wantHas: true,
		},
		{
			name:    "probability 0.0 -> 0%",
			data:    TemplateData{"probability": 0.0},
			field:   "probability_formatted",
			wantFmt: "0%",
			wantHas: true,
		},
		{
			name:    "probability 1.0 -> 100%",
			data:    TemplateData{"confidence": 1.0},
			field:   "confidence_formatted",
			wantFmt: "100%",
			wantHas: true,
		},
		{
			name:    "value > 1 not formatted",
			data:    TemplateData{"precipitation_probability": 55.0},
			field:   "precipitation_probability_formatted",
			wantHas: false,
		},
		{
			name:    "non-probability field not touched",
			data:    TemplateData{"temperature": 0.5},
			field:   "temperature_formatted",
			wantHas: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			formatPayloadNumbers(tt.data)
			val, ok := tt.data[tt.field]
			if ok != tt.wantHas {
				t.Errorf("field %q present = %v, want %v", tt.field, ok, tt.wantHas)
			}
			if tt.wantHas && val != tt.wantFmt {
				t.Errorf("field %q = %v, want %v", tt.field, val, tt.wantFmt)
			}
		})
	}
}

// --- Timestamp Formatting Tests ---

func TestFormatPayloadTimestamps(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")

	data := TemplateData{
		"forecast_time":        "2026-02-06T15:30:00Z",
		"threshold_crossed_at": "2026-02-06T20:00:00Z",
		"non_timestamp_field":  "not-a-timestamp",
	}

	formatPayloadTimestamps(data, loc)

	// Check forecast_time was formatted.
	if _, ok := data["forecast_time_formatted"].(string); !ok {
		t.Error("forecast_time_formatted not set")
	}

	// Check threshold_crossed_at was formatted.
	if _, ok := data["threshold_crossed_at_formatted"].(string); !ok {
		t.Error("threshold_crossed_at_formatted not set")
	}

	// Non-timestamp fields should not get a _formatted variant.
	if _, ok := data["non_timestamp_field_formatted"]; ok {
		t.Error("non_timestamp_field_formatted should not exist")
	}
}

// --- resolveTimezone Tests ---

func TestResolveTimezone(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]interface{}
		want    string
	}{
		{
			name:    "valid timezone",
			payload: map[string]interface{}{"timezone": "America/Chicago"},
			want:    "America/Chicago",
		},
		{
			name:    "UTC",
			payload: map[string]interface{}{"timezone": "UTC"},
			want:    "UTC",
		},
		{
			name:    "missing timezone",
			payload: map[string]interface{}{},
			want:    "UTC",
		},
		{
			name:    "nil payload",
			payload: nil,
			want:    "UTC",
		},
		{
			name:    "invalid timezone string",
			payload: map[string]interface{}{"timezone": "Not/Real"},
			want:    "UTC",
		},
		{
			name:    "empty timezone string",
			payload: map[string]interface{}{"timezone": ""},
			want:    "UTC",
		},
		{
			name:    "non-string timezone value",
			payload: map[string]interface{}{"timezone": 42},
			want:    "UTC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc := resolveTimezone(tt.payload)
			if loc.String() != tt.want {
				t.Errorf("resolveTimezone() = %q, want %q", loc.String(), tt.want)
			}
		})
	}
}

// --- toFloat64 Tests ---

func TestToFloat64(t *testing.T) {
	tests := []struct {
		name    string
		input   interface{}
		want    float64
		wantOK  bool
	}{
		{"float64", 3.14, 3.14, true},
		{"int", 42, 42.0, true},
		{"int64", int64(100), 100.0, true},
		{"string numeric", "2.5", 2.5, true},
		{"string non-numeric", "abc", 0, false},
		{"bool", true, 0, false},
		{"nil", nil, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toFloat64(tt.input)
			if ok != tt.wantOK {
				t.Errorf("toFloat64(%v) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("toFloat64(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
