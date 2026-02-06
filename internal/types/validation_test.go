package types

import (
	"strings"
	"testing"
	"time"
)

// --- IsCONUS Tests ---

func TestIsCONUS_WithinBounds(t *testing.T) {
	tests := []struct {
		name string
		lat  float64
		lon  float64
		want bool
	}{
		{"center of CONUS", 37.0, -95.0, true},
		{"northwest corner", 50.0, -125.0, true},
		{"southeast corner", 24.0, -66.0, true},
		{"northeast corner", 50.0, -66.0, true},
		{"southwest corner", 24.0, -125.0, true},
		{"exact min lat boundary", 24.0, -95.0, true},
		{"exact max lat boundary", 50.0, -95.0, true},
		{"exact min lon boundary", 37.0, -125.0, true},
		{"exact max lon boundary", 37.0, -66.0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCONUS(tt.lat, tt.lon); got != tt.want {
				t.Errorf("IsCONUS(%v, %v) = %v, want %v", tt.lat, tt.lon, got, tt.want)
			}
		})
	}
}

func TestIsCONUS_OutsideBounds(t *testing.T) {
	tests := []struct {
		name string
		lat  float64
		lon  float64
	}{
		{"north of CONUS (Canada)", 51.0, -95.0},
		{"south of CONUS (Mexico)", 23.0, -95.0},
		{"east of CONUS (Atlantic)", 37.0, -65.0},
		{"west of CONUS (Pacific)", 37.0, -126.0},
		{"Alaska", 64.0, -150.0},
		{"Hawaii", 20.0, -155.0},
		{"Europe", 48.0, 2.0},
		{"Southern hemisphere", -33.0, -70.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if IsCONUS(tt.lat, tt.lon) {
				t.Errorf("IsCONUS(%v, %v) = true, want false", tt.lat, tt.lon)
			}
		})
	}
}

// --- StandardVariables Tests ---

func TestStandardVariables_ContainsAllExpectedVariables(t *testing.T) {
	expected := []string{
		"temperature_c",
		"precipitation_probability",
		"precipitation_mm",
		"wind_speed_kmh",
		"humidity_percent",
		"cloud_cover_percent",
		"uv_index",
	}

	if len(StandardVariables) != len(expected) {
		t.Errorf("StandardVariables has %d entries, expected %d", len(StandardVariables), len(expected))
	}

	for _, name := range expected {
		meta, ok := StandardVariables[name]
		if !ok {
			t.Errorf("StandardVariables missing expected variable %q", name)
			continue
		}
		if meta.ID != name {
			t.Errorf("StandardVariables[%q].ID = %q, want %q", name, meta.ID, name)
		}
		if meta.Unit == "" {
			t.Errorf("StandardVariables[%q].Unit is empty", name)
		}
		if meta.Description == "" {
			t.Errorf("StandardVariables[%q].Description is empty", name)
		}
		if meta.Range[0] > meta.Range[1] {
			t.Errorf("StandardVariables[%q].Range min (%v) > max (%v)", name, meta.Range[0], meta.Range[1])
		}
	}
}

func TestStandardVariables_SpecificRanges(t *testing.T) {
	tests := []struct {
		variable string
		min      float64
		max      float64
	}{
		{"temperature_c", -60, 60},
		{"precipitation_probability", 0, 100},
		{"precipitation_mm", 0, 500},
		{"wind_speed_kmh", 0, 300},
		{"humidity_percent", 0, 100},
		{"cloud_cover_percent", 0, 100},
		{"uv_index", 0, 15},
	}

	for _, tt := range tests {
		t.Run(tt.variable, func(t *testing.T) {
			meta := StandardVariables[tt.variable]
			if meta.Range[0] != tt.min || meta.Range[1] != tt.max {
				t.Errorf("StandardVariables[%q].Range = [%v, %v], want [%v, %v]",
					tt.variable, meta.Range[0], meta.Range[1], tt.min, tt.max)
			}
		})
	}
}

// --- ValidateConditionThreshold Tests ---

func TestValidateConditionThreshold_ValidValues(t *testing.T) {
	tests := []struct {
		variable  string
		threshold float64
	}{
		{"temperature_c", 0},
		{"temperature_c", -60},
		{"temperature_c", 60},
		{"temperature_c", 25.5},
		{"precipitation_probability", 0},
		{"precipitation_probability", 100},
		{"precipitation_probability", 50},
		{"precipitation_mm", 0},
		{"precipitation_mm", 500},
		{"wind_speed_kmh", 0},
		{"wind_speed_kmh", 300},
		{"humidity_percent", 0},
		{"humidity_percent", 100},
		{"cloud_cover_percent", 0},
		{"cloud_cover_percent", 100},
		{"uv_index", 0},
		{"uv_index", 15},
	}

	for _, tt := range tests {
		t.Run(tt.variable, func(t *testing.T) {
			if err := ValidateConditionThreshold(tt.variable, tt.threshold); err != nil {
				t.Errorf("ValidateConditionThreshold(%q, %v) returned unexpected error: %v",
					tt.variable, tt.threshold, err)
			}
		})
	}
}

func TestValidateConditionThreshold_UnknownVariable(t *testing.T) {
	err := ValidateConditionThreshold("nonexistent_variable", 50)
	if err == nil {
		t.Fatal("ValidateConditionThreshold with unknown variable should return error")
	}
	if !strings.Contains(err.Error(), string(ErrCodeValidationInvalidConditions)) {
		t.Errorf("error should contain code %q, got: %v", ErrCodeValidationInvalidConditions, err)
	}
	if !strings.Contains(err.Error(), "nonexistent_variable") {
		t.Errorf("error should mention the unknown variable name, got: %v", err)
	}
}

func TestValidateConditionThreshold_BelowRange(t *testing.T) {
	err := ValidateConditionThreshold("temperature_c", -61)
	if err == nil {
		t.Fatal("ValidateConditionThreshold below range should return error")
	}
	if !strings.Contains(err.Error(), string(ErrCodeValidationThresholdRange)) {
		t.Errorf("error should contain code %q, got: %v", ErrCodeValidationThresholdRange, err)
	}
}

func TestValidateConditionThreshold_AboveRange(t *testing.T) {
	err := ValidateConditionThreshold("temperature_c", 61)
	if err == nil {
		t.Fatal("ValidateConditionThreshold above range should return error")
	}
	if !strings.Contains(err.Error(), string(ErrCodeValidationThresholdRange)) {
		t.Errorf("error should contain code %q, got: %v", ErrCodeValidationThresholdRange, err)
	}
}

func TestValidateConditionThreshold_BoundaryValues(t *testing.T) {
	tests := []struct {
		name      string
		variable  string
		threshold float64
		wantErr   bool
	}{
		{"precip_prob at 0", "precipitation_probability", 0, false},
		{"precip_prob at 100", "precipitation_probability", 100, false},
		{"precip_prob at -0.01", "precipitation_probability", -0.01, true},
		{"precip_prob at 100.01", "precipitation_probability", 100.01, true},
		{"uv_index at 15", "uv_index", 15, false},
		{"uv_index at 15.01", "uv_index", 15.01, true},
		{"wind at 300", "wind_speed_kmh", 300, false},
		{"wind at 300.01", "wind_speed_kmh", 300.01, true},
		{"precip_mm at 500", "precipitation_mm", 500, false},
		{"precip_mm at 500.01", "precipitation_mm", 500.01, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateConditionThreshold(tt.variable, tt.threshold)
			if tt.wantErr && err == nil {
				t.Errorf("ValidateConditionThreshold(%q, %v) should return error", tt.variable, tt.threshold)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidateConditionThreshold(%q, %v) unexpected error: %v", tt.variable, tt.threshold, err)
			}
		})
	}
}

// --- ValidateTimeWindow Tests ---

func TestValidateTimeWindow_NilWindow(t *testing.T) {
	if err := ValidateTimeWindow(nil); err != nil {
		t.Errorf("ValidateTimeWindow(nil) should return nil, got: %v", err)
	}
}

func TestValidateTimeWindow_ValidWindow(t *testing.T) {
	now := time.Now().UTC()
	tw := &TimeWindow{
		Start: now,
		End:   now.Add(24 * time.Hour),
	}
	if err := ValidateTimeWindow(tw); err != nil {
		t.Errorf("ValidateTimeWindow with valid 24h window returned error: %v", err)
	}
}

func TestValidateTimeWindow_EndBeforeStart(t *testing.T) {
	now := time.Now().UTC()
	tw := &TimeWindow{
		Start: now,
		End:   now.Add(-1 * time.Hour),
	}
	err := ValidateTimeWindow(tw)
	if err == nil {
		t.Fatal("ValidateTimeWindow with end before start should return error")
	}
	if !strings.Contains(err.Error(), string(ErrCodeValidationTimeWindow)) {
		t.Errorf("error should contain code %q, got: %v", ErrCodeValidationTimeWindow, err)
	}
	if !strings.Contains(err.Error(), "end must be after start") {
		t.Errorf("error should mention end must be after start, got: %v", err)
	}
}

func TestValidateTimeWindow_EndEqualsStart(t *testing.T) {
	now := time.Now().UTC()
	tw := &TimeWindow{
		Start: now,
		End:   now,
	}
	err := ValidateTimeWindow(tw)
	if err == nil {
		t.Fatal("ValidateTimeWindow with end == start should return error")
	}
	if !strings.Contains(err.Error(), string(ErrCodeValidationTimeWindow)) {
		t.Errorf("error should contain code %q, got: %v", ErrCodeValidationTimeWindow, err)
	}
}

func TestValidateTimeWindow_ExceedsMaxDuration(t *testing.T) {
	now := time.Now().UTC()
	tw := &TimeWindow{
		Start: now,
		End:   now.Add(31 * 24 * time.Hour),
	}
	err := ValidateTimeWindow(tw)
	if err == nil {
		t.Fatal("ValidateTimeWindow with >30 day window should return error")
	}
	if !strings.Contains(err.Error(), string(ErrCodeValidationTimeWindow)) {
		t.Errorf("error should contain code %q, got: %v", ErrCodeValidationTimeWindow, err)
	}
	if !strings.Contains(err.Error(), "30 days") {
		t.Errorf("error should mention 30 days limit, got: %v", err)
	}
}

func TestValidateTimeWindow_ExactlyMaxDuration(t *testing.T) {
	now := time.Now().UTC()
	tw := &TimeWindow{
		Start: now,
		End:   now.Add(30 * 24 * time.Hour),
	}
	if err := ValidateTimeWindow(tw); err != nil {
		t.Errorf("ValidateTimeWindow with exactly 30 day window should succeed, got: %v", err)
	}
}

func TestValidateTimeWindow_JustOverMaxDuration(t *testing.T) {
	now := time.Now().UTC()
	tw := &TimeWindow{
		Start: now,
		End:   now.Add(30*24*time.Hour + 1*time.Second),
	}
	err := ValidateTimeWindow(tw)
	if err == nil {
		t.Fatal("ValidateTimeWindow with 30 days + 1 second should return error")
	}
}

func TestValidateTimeWindow_MinimalValidWindow(t *testing.T) {
	now := time.Now().UTC()
	tw := &TimeWindow{
		Start: now,
		End:   now.Add(1 * time.Nanosecond),
	}
	// Even a nanosecond difference should be valid (end is after start)
	if err := ValidateTimeWindow(tw); err != nil {
		t.Errorf("ValidateTimeWindow with minimal valid window returned error: %v", err)
	}
}

// --- ValidateWebhookURL Tests ---

func TestValidateWebhookURL_ValidHTTPS(t *testing.T) {
	tests := []string{
		"https://example.com/webhook",
		"https://hooks.slack.com/services/abc/def",
		"https://api.example.com:8443/hooks",
		"https://my-service.internal.company.com/v1/webhooks",
		"https://example.com",
	}

	for _, url := range tests {
		t.Run(url, func(t *testing.T) {
			if err := ValidateWebhookURL(url); err != nil {
				t.Errorf("ValidateWebhookURL(%q) returned unexpected error: %v", url, err)
			}
		})
	}
}

func TestValidateWebhookURL_RejectsHTTP(t *testing.T) {
	err := ValidateWebhookURL("http://example.com/webhook")
	if err == nil {
		t.Fatal("ValidateWebhookURL with HTTP should return error")
	}
	if !strings.Contains(err.Error(), string(ErrCodeValidationInvalidWebhook)) {
		t.Errorf("error should contain code %q, got: %v", ErrCodeValidationInvalidWebhook, err)
	}
	if !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("error should mention HTTPS requirement, got: %v", err)
	}
}

func TestValidateWebhookURL_RejectsOtherSchemes(t *testing.T) {
	tests := []string{
		"ftp://example.com/file",
		"ssh://example.com",
		"file:///etc/passwd",
		"javascript:alert(1)",
	}

	for _, url := range tests {
		t.Run(url, func(t *testing.T) {
			err := ValidateWebhookURL(url)
			if err == nil {
				t.Errorf("ValidateWebhookURL(%q) should return error for non-HTTPS scheme", url)
			}
		})
	}
}

func TestValidateWebhookURL_RejectsEmptyScheme(t *testing.T) {
	err := ValidateWebhookURL("example.com/webhook")
	if err == nil {
		t.Fatal("ValidateWebhookURL with no scheme should return error")
	}
}

func TestValidateWebhookURL_ErrorContainsCode(t *testing.T) {
	err := ValidateWebhookURL("http://example.com")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), string(ErrCodeValidationInvalidWebhook)) {
		t.Errorf("error message should contain %q, got: %v", ErrCodeValidationInvalidWebhook, err)
	}
}

// --- SSRFBlockedCIDRs Tests ---

func TestSSRFBlockedCIDRs_ContainsRequiredRanges(t *testing.T) {
	required := []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"0.0.0.0/8",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"100.64.0.0/10",
		"198.18.0.0/15",
		"fc00::/7",
		"fe80::/10",
		"::1/128",
	}

	cidrs := make(map[string]bool)
	for _, c := range SSRFBlockedCIDRs {
		cidrs[c] = true
	}

	for _, r := range required {
		if !cidrs[r] {
			t.Errorf("SSRFBlockedCIDRs missing required CIDR: %s", r)
		}
	}

	if len(SSRFBlockedCIDRs) != len(required) {
		t.Errorf("SSRFBlockedCIDRs has %d entries, expected %d", len(SSRFBlockedCIDRs), len(required))
	}
}

// --- Validation Constants Tests ---

func TestValidationConstants(t *testing.T) {
	tests := []struct {
		name     string
		got      float64
		expected float64
	}{
		{"MinLat", MinLat, -90.0},
		{"MaxLat", MaxLat, 90.0},
		{"MinLon", MinLon, -180.0},
		{"MaxLon", MaxLon, 180.0},
		{"ConusMinLat", ConusMinLat, 24.0},
		{"ConusMaxLat", ConusMaxLat, 50.0},
		{"ConusMinLon", ConusMinLon, -125.0},
		{"ConusMaxLon", ConusMaxLon, -66.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.expected {
				t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.expected)
			}
		})
	}

	if MaxConditions != 10 {
		t.Errorf("MaxConditions = %d, want 10", MaxConditions)
	}
	if MaxNameLength != 200 {
		t.Errorf("MaxNameLength = %d, want 200", MaxNameLength)
	}
	if MaxMonitorWindow != 168 {
		t.Errorf("MaxMonitorWindow = %d, want 168", MaxMonitorWindow)
	}
	if MinMonitorWindow != 6 {
		t.Errorf("MinMonitorWindow = %d, want 6", MinMonitorWindow)
	}
}
