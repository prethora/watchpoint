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

// --- Probability Formatting Tests ---

func TestFormatPayloadNumbers(t *testing.T) {
	tests := []struct {
		name    string
		data    TemplateData
		field   string
		wantFmt string
		wantHas bool
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

	if _, ok := data["forecast_time_formatted"].(string); !ok {
		t.Error("forecast_time_formatted not set")
	}

	if _, ok := data["threshold_crossed_at_formatted"].(string); !ok {
		t.Error("threshold_crossed_at_formatted not set")
	}

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
		name   string
		input  interface{}
		want   float64
		wantOK bool
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
