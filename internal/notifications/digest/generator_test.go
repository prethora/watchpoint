package digest

import (
	"context"
	"testing"
	"time"

	"watchpoint/internal/notifications/core"
	"watchpoint/internal/types"
)

// helper to make a WatchPoint for tests.
func testWatchPoint() *types.WatchPoint {
	return &types.WatchPoint{
		ID:             "wp_test123",
		OrganizationID: "org_test456",
		Name:           "Test WatchPoint",
	}
}

// helper to make a DigestConfig for tests.
func testDigestConfig(sendEmpty bool) *types.DigestConfig {
	return &types.DigestConfig{
		Enabled:      true,
		Frequency:    "daily",
		DeliveryTime: "08:00",
		SendEmpty:    sendEmpty,
		TemplateSet:  "digest_default",
	}
}

func TestGenerate_BasicDigest_NoComparison(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	current := &types.MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(24 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob": 75.0,
			"precip_mm":   12.5,
			"min_temp_c":  2.0,
			"max_temp_c":  15.0,
		},
		TriggeredPeriods: []types.TimeRange{
			{Start: now.Add(3 * time.Hour), End: now.Add(6 * time.Hour)},
		},
	}

	result, err := gen.Generate(ctx, wp, current, nil, testDigestConfig(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.PeriodStart != now {
		t.Errorf("PeriodStart = %v, want %v", result.PeriodStart, now)
	}
	if result.PeriodEnd != now.Add(24*time.Hour) {
		t.Errorf("PeriodEnd = %v, want %v", result.PeriodEnd, now.Add(24*time.Hour))
	}
	if result.ForecastSummary.MaxPrecipProb != 75.0 {
		t.Errorf("MaxPrecipProb = %v, want 75.0", result.ForecastSummary.MaxPrecipProb)
	}
	if result.ForecastSummary.MaxPrecipMM != 12.5 {
		t.Errorf("MaxPrecipMM = %v, want 12.5", result.ForecastSummary.MaxPrecipMM)
	}
	if result.ForecastSummary.MinTempC != 2.0 {
		t.Errorf("MinTempC = %v, want 2.0", result.ForecastSummary.MinTempC)
	}
	if result.ForecastSummary.MaxTempC != 15.0 {
		t.Errorf("MaxTempC = %v, want 15.0", result.ForecastSummary.MaxTempC)
	}
	if len(result.TriggeredPeriods) != 1 {
		t.Fatalf("TriggeredPeriods count = %d, want 1", len(result.TriggeredPeriods))
	}
	if result.Comparison != nil {
		t.Error("Comparison should be nil when no previous digest exists")
	}
	if result.RemainingCount != 0 {
		t.Errorf("RemainingCount = %d, want 0", result.RemainingCount)
	}
}

func TestGenerate_NewThreatDetected(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	current := &types.MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(24 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob": 85.0,
			"precip_mm":   20.0,
			"min_temp_c":  3.0,
			"max_temp_c":  14.0,
		},
		TriggeredPeriods: []types.TimeRange{
			{Start: now.Add(3 * time.Hour), End: now.Add(6 * time.Hour)},
		},
	}

	// Previous digest had no triggered periods (no threat).
	previous := &DigestContent{
		PeriodStart: now.Add(-24 * time.Hour),
		PeriodEnd:   now,
		ForecastSummary: ForecastSummary{
			MaxPrecipProb: 30.0,
			MaxPrecipMM:   5.0,
			MinTempC:      5.0,
			MaxTempC:      18.0,
		},
		TriggeredPeriods: nil, // No previous threats
	}

	result, err := gen.Generate(ctx, wp, current, previous, testDigestConfig(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Comparison == nil {
		t.Fatal("Comparison should not be nil when previous digest exists")
	}

	// Should detect "New Threat" because current has triggered periods, previous did not.
	foundThreatChange := false
	for _, c := range result.Comparison.Changes {
		if c.Variable == "threat_status" {
			foundThreatChange = true
			if c.From != 0 || c.To != 1 {
				t.Errorf("threat_status change: From=%v To=%v, want From=0 To=1", c.From, c.To)
			}
		}
	}
	if !foundThreatChange {
		t.Error("expected threat_status change in comparison (New Threat)")
	}

	// Direction should be worsening (precip up significantly, threat appeared).
	if result.Comparison.Direction != "worsening" {
		t.Errorf("Direction = %q, want %q", result.Comparison.Direction, "worsening")
	}
}

func TestGenerate_ThreatCleared(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	// Current: no triggered periods.
	current := &types.MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(24 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob": 20.0,
			"precip_mm":   2.0,
			"min_temp_c":  8.0,
			"max_temp_c":  16.0,
		},
		TriggeredPeriods: nil,
	}

	// Previous: had triggered periods (active threat).
	previous := &DigestContent{
		PeriodStart: now.Add(-24 * time.Hour),
		PeriodEnd:   now,
		ForecastSummary: ForecastSummary{
			MaxPrecipProb: 80.0,
			MaxPrecipMM:   18.0,
			MinTempC:      2.0,
			MaxTempC:      14.0,
		},
		TriggeredPeriods: []types.TimeRange{
			{Start: now.Add(-20 * time.Hour), End: now.Add(-18 * time.Hour)},
		},
	}

	result, err := gen.Generate(ctx, wp, current, previous, testDigestConfig(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Comparison == nil {
		t.Fatal("Comparison should not be nil")
	}

	foundThreatCleared := false
	for _, c := range result.Comparison.Changes {
		if c.Variable == "threat_status" {
			foundThreatCleared = true
			if c.From != 1 || c.To != 0 {
				t.Errorf("threat_status: From=%v To=%v, want From=1 To=0", c.From, c.To)
			}
		}
	}
	if !foundThreatCleared {
		t.Error("expected threat_status change (Cleared)")
	}

	// Direction should be improving (precip down, threat cleared).
	if result.Comparison.Direction != "improving" {
		t.Errorf("Direction = %q, want %q", result.Comparison.Direction, "improving")
	}
}

func TestGenerate_StableConditions(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	current := &types.MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(24 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob": 50.0,
			"precip_mm":   8.0,
			"min_temp_c":  5.0,
			"max_temp_c":  15.0,
		},
		TriggeredPeriods: nil,
	}

	// Previous has very similar values, no threats in either.
	previous := &DigestContent{
		PeriodStart: now.Add(-24 * time.Hour),
		PeriodEnd:   now,
		ForecastSummary: ForecastSummary{
			MaxPrecipProb: 48.0, // ~4% change, below 20% threshold
			MaxPrecipMM:   7.5,  // ~6.7% change, below threshold
			MinTempC:      5.2,  // ~3.8% change, below threshold
			MaxTempC:      15.5, // ~3.2% change, below threshold
		},
		TriggeredPeriods: nil,
	}

	result, err := gen.Generate(ctx, wp, current, previous, testDigestConfig(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Comparison == nil {
		t.Fatal("Comparison should not be nil")
	}

	if result.Comparison.Direction != "stable" {
		t.Errorf("Direction = %q, want %q", result.Comparison.Direction, "stable")
	}

	// Should have no significant changes (threat_status is ThreatNone: no entry).
	for _, c := range result.Comparison.Changes {
		if c.Variable == "threat_status" {
			t.Error("should not have threat_status change when both have no threats")
		}
	}
}

func TestGenerate_SendEmptyFalse_EmptyDigest(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	// Current data with no triggered periods.
	current := &types.MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(24 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob": 10.0,
			"precip_mm":   1.0,
			"min_temp_c":  8.0,
			"max_temp_c":  16.0,
		},
		TriggeredPeriods: nil,
	}

	// Previous has similar values (no significant changes).
	previous := &DigestContent{
		PeriodStart: now.Add(-24 * time.Hour),
		PeriodEnd:   now,
		ForecastSummary: ForecastSummary{
			MaxPrecipProb: 10.5,
			MaxPrecipMM:   1.1,
			MinTempC:      8.2,
			MaxTempC:      15.8,
		},
		TriggeredPeriods: nil,
	}

	// SendEmpty = false
	_, err := gen.Generate(ctx, wp, current, previous, testDigestConfig(false))
	if err != core.ErrDigestEmpty {
		t.Fatalf("expected ErrDigestEmpty, got %v", err)
	}
}

func TestGenerate_SendEmptyTrue_EmptyDigest(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	current := &types.MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(24 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob": 10.0,
			"precip_mm":   1.0,
			"min_temp_c":  8.0,
			"max_temp_c":  16.0,
		},
		TriggeredPeriods: nil,
	}

	previous := &DigestContent{
		PeriodStart: now.Add(-24 * time.Hour),
		PeriodEnd:   now,
		ForecastSummary: ForecastSummary{
			MaxPrecipProb: 10.5,
			MaxPrecipMM:   1.1,
			MinTempC:      8.2,
			MaxTempC:      15.8,
		},
		TriggeredPeriods: nil,
	}

	// SendEmpty = true => should return the digest even if empty.
	result, err := gen.Generate(ctx, wp, current, previous, testDigestConfig(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("result should not be nil when SendEmpty=true")
	}
}

func TestGenerate_SendEmptyFalse_HasTriggeredPeriods(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	current := &types.MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(24 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob": 10.0,
		},
		TriggeredPeriods: []types.TimeRange{
			{Start: now.Add(2 * time.Hour), End: now.Add(4 * time.Hour)},
		},
	}

	// Even with SendEmpty=false, having triggered periods means we have content.
	result, err := gen.Generate(ctx, wp, current, nil, testDigestConfig(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("result should not be nil when there are triggered periods")
	}
	if len(result.TriggeredPeriods) != 1 {
		t.Errorf("TriggeredPeriods count = %d, want 1", len(result.TriggeredPeriods))
	}
}

func TestGenerate_SendEmptyFalse_HasSignificantChanges(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	// No triggered periods, but significant MaxValue change.
	current := &types.MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(24 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob": 80.0, // jumped from 30
			"precip_mm":   1.0,
			"min_temp_c":  8.0,
			"max_temp_c":  16.0,
		},
		TriggeredPeriods: nil,
	}

	previous := &DigestContent{
		ForecastSummary: ForecastSummary{
			MaxPrecipProb: 30.0,
			MaxPrecipMM:   1.0,
			MinTempC:      8.0,
			MaxTempC:      16.0,
		},
		TriggeredPeriods: nil,
	}

	// SendEmpty=false, but there are significant changes => should return content.
	result, err := gen.Generate(ctx, wp, current, previous, testDigestConfig(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("result should not be nil when there are significant changes")
	}
}

func TestGenerate_TruncateTriggeredPeriods(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	// Create 25 triggered periods.
	periods := make([]types.TimeRange, 25)
	for i := range periods {
		periods[i] = types.TimeRange{
			Start: now.Add(time.Duration(i) * time.Hour),
			End:   now.Add(time.Duration(i)*time.Hour + 30*time.Minute),
		}
	}

	current := &types.MonitorSummary{
		WindowStart:      now,
		WindowEnd:        now.Add(48 * time.Hour),
		MaxValues:        map[string]float64{"precip_prob": 90.0},
		TriggeredPeriods: periods,
	}

	result, err := gen.Generate(ctx, wp, current, nil, testDigestConfig(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.TriggeredPeriods) != 20 {
		t.Errorf("TriggeredPeriods count = %d, want 20 (truncated)", len(result.TriggeredPeriods))
	}
	if result.RemainingCount != 5 {
		t.Errorf("RemainingCount = %d, want 5", result.RemainingCount)
	}
}

func TestGenerate_NilCurrent_SendEmptyFalse(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()

	_, err := gen.Generate(ctx, wp, nil, nil, testDigestConfig(false))
	if err != core.ErrDigestEmpty {
		t.Fatalf("expected ErrDigestEmpty for nil current with SendEmpty=false, got %v", err)
	}
}

func TestGenerate_NilCurrent_SendEmptyTrue(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()

	result, err := gen.Generate(ctx, wp, nil, nil, testDigestConfig(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("result should not be nil when SendEmpty=true")
	}
}

func TestGenerate_NilDigestConfig(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	current := &types.MonitorSummary{
		WindowStart:      now,
		WindowEnd:        now.Add(24 * time.Hour),
		MaxValues:        map[string]float64{"precip_prob": 50.0},
		TriggeredPeriods: nil,
	}

	// nil config means we always generate (no SendEmpty check applies).
	result, err := gen.Generate(ctx, wp, current, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("result should not be nil with nil config")
	}
}

func TestGenerate_PersistingThreat(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	current := &types.MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(24 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob": 80.0,
			"precip_mm":   15.0,
			"min_temp_c":  3.0,
			"max_temp_c":  14.0,
		},
		TriggeredPeriods: []types.TimeRange{
			{Start: now.Add(2 * time.Hour), End: now.Add(5 * time.Hour)},
		},
	}

	previous := &DigestContent{
		ForecastSummary: ForecastSummary{
			MaxPrecipProb: 78.0, // similar values
			MaxPrecipMM:   14.0,
			MinTempC:      3.2,
			MaxTempC:      14.5,
		},
		TriggeredPeriods: []types.TimeRange{
			{Start: now.Add(-22 * time.Hour), End: now.Add(-20 * time.Hour)},
		},
	}

	result, err := gen.Generate(ctx, wp, current, previous, testDigestConfig(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both current and previous have threats => no threat_status change.
	for _, c := range result.Comparison.Changes {
		if c.Variable == "threat_status" {
			t.Error("should not have threat_status change when both have threats (persisting)")
		}
	}

	// Values are similar, so direction should be stable.
	if result.Comparison.Direction != "stable" {
		t.Errorf("Direction = %q, want %q", result.Comparison.Direction, "stable")
	}
}

func TestIsSignificantChange(t *testing.T) {
	tests := []struct {
		name     string
		previous float64
		current  float64
		want     bool
	}{
		{"no change", 50.0, 50.0, false},
		{"small change", 50.0, 55.0, false},             // 10% < 20%
		// Reference = max(abs(prev), abs(curr)); pctChange = abs(delta)/reference * 100
		{"exactly at threshold", 50.0, 60.0, false},      // delta=10, ref=60, pct=16.7% < 20%
		{"above threshold", 50.0, 70.0, true},            // delta=20, ref=70, pct=28.6% > 20%
		{"large increase", 30.0, 80.0, true},             // delta=50, ref=80, pct=62.5%
		{"large decrease", 80.0, 30.0, true},             // delta=50, ref=80, pct=62.5%
		{"zero to non-zero", 0.0, 10.0, true},            // delta=10, ref=10, pct=100%
		{"non-zero to zero", 10.0, 0.0, true},            // delta=10, ref=10, pct=100%
		{"both zero", 0.0, 0.0, false},                   // no change
		{"tiny values near zero", 1e-10, 2e-10, false},   // ref < 1e-9, treated as zero
		{"small but real", 0.5, 1.0, true},               // delta=0.5, ref=1.0, pct=50%
		{"negative values", -5.0, -10.0, true},           // delta=5, ref=10, pct=50%
		{"borderline below", 100.0, 115.0, false},        // delta=15, ref=115, pct=13% < 20%
		{"just above threshold", 100.0, 126.0, true},     // delta=26, ref=126, pct=20.6% > 20%
		{"borderline at 20%", 100.0, 125.0, false},       // delta=25, ref=125, pct=20% NOT > 20%
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSignificantChange(tt.previous, tt.current)
			if got != tt.want {
				t.Errorf("isSignificantChange(%v, %v) = %v, want %v", tt.previous, tt.current, got, tt.want)
			}
		})
	}
}

func TestCompareThreatStatus(t *testing.T) {
	now := time.Now()
	period := []types.TimeRange{{Start: now, End: now.Add(time.Hour)}}

	tests := []struct {
		name            string
		currentPeriods  []types.TimeRange
		previousPeriods []types.TimeRange
		want            ThreatStatus
	}{
		{
			"new threat",
			period,
			nil,
			ThreatNew,
		},
		{
			"cleared",
			nil,
			period,
			ThreatCleared,
		},
		{
			"persisting",
			period,
			period,
			ThreatPersisting,
		},
		{
			"none",
			nil,
			nil,
			ThreatNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := &types.MonitorSummary{
				TriggeredPeriods: tt.currentPeriods,
			}
			previous := &DigestContent{
				TriggeredPeriods: tt.previousPeriods,
			}
			got := compareThreatStatus(current, previous)
			if got != tt.want {
				t.Errorf("compareThreatStatus = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetermineDirection(t *testing.T) {
	tests := []struct {
		name    string
		changes []Change
		want    string
	}{
		{"empty", nil, "stable"},
		{"worsening precip", []Change{{Variable: "precip_prob", From: 30, To: 80}}, "worsening"},
		{"improving precip", []Change{{Variable: "precip_prob", From: 80, To: 30}}, "improving"},
		{"mixed even", []Change{
			{Variable: "precip_prob", From: 30, To: 80}, // worsening
			{Variable: "min_temp_c", From: 2, To: 5},    // improving (warmer)
		}, "stable"},
		{"new threat dominant", []Change{
			{Variable: "threat_status", From: 0, To: 1},
			{Variable: "precip_prob", From: 30, To: 80},
		}, "worsening"},
		{"cleared dominant", []Change{
			{Variable: "threat_status", From: 1, To: 0},
			{Variable: "precip_prob", From: 80, To: 30},
		}, "improving"},
		{"min temp colder", []Change{{Variable: "min_temp_c", From: 5, To: 2}}, "worsening"},
		{"min temp warmer", []Change{{Variable: "min_temp_c", From: 2, To: 5}}, "improving"},
		{"max temp hotter", []Change{{Variable: "max_temp_c", From: 20, To: 35}}, "worsening"},
		{"max temp cooler", []Change{{Variable: "max_temp_c", From: 35, To: 20}}, "improving"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineDirection(tt.changes)
			if got != tt.want {
				t.Errorf("determineDirection = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractForecastSummary(t *testing.T) {
	maxValues := map[string]float64{
		"precip_prob": 75.5,
		"precip_mm":   12.3,
		"min_temp_c":  -2.5,
		"max_temp_c":  18.0,
	}

	summary := extractForecastSummary(maxValues)

	if summary.MaxPrecipProb != 75.5 {
		t.Errorf("MaxPrecipProb = %v, want 75.5", summary.MaxPrecipProb)
	}
	if summary.MaxPrecipMM != 12.3 {
		t.Errorf("MaxPrecipMM = %v, want 12.3", summary.MaxPrecipMM)
	}
	if summary.MinTempC != -2.5 {
		t.Errorf("MinTempC = %v, want -2.5", summary.MinTempC)
	}
	if summary.MaxTempC != 18.0 {
		t.Errorf("MaxTempC = %v, want 18.0", summary.MaxTempC)
	}
}

func TestExtractForecastSummary_MissingKeys(t *testing.T) {
	// Only some keys present; missing ones should default to zero.
	maxValues := map[string]float64{
		"precip_prob": 50.0,
	}

	summary := extractForecastSummary(maxValues)

	if summary.MaxPrecipProb != 50.0 {
		t.Errorf("MaxPrecipProb = %v, want 50.0", summary.MaxPrecipProb)
	}
	if summary.MaxPrecipMM != 0.0 {
		t.Errorf("MaxPrecipMM = %v, want 0.0 (default)", summary.MaxPrecipMM)
	}
	if summary.MinTempC != 0.0 {
		t.Errorf("MinTempC = %v, want 0.0 (default)", summary.MinTempC)
	}
	if summary.MaxTempC != 0.0 {
		t.Errorf("MaxTempC = %v, want 0.0 (default)", summary.MaxTempC)
	}
}

func TestCompareMaxValues_SignificantChanges(t *testing.T) {
	currentMax := map[string]float64{
		"precip_prob": 80.0,
		"precip_mm":   20.0,
		"min_temp_c":  2.0,
		"max_temp_c":  15.0,
	}

	prevSummary := ForecastSummary{
		MaxPrecipProb: 30.0, // 80 vs 30 = 167% change
		MaxPrecipMM:   5.0,  // 20 vs 5 = 300% change
		MinTempC:      5.0,  // 2 vs 5 = 60% change
		MaxTempC:      15.0, // no change
	}

	changes := compareMaxValues(currentMax, prevSummary)

	// Should detect 3 significant changes (precip_prob, precip_mm, min_temp_c).
	if len(changes) != 3 {
		t.Errorf("changes count = %d, want 3", len(changes))
		for _, c := range changes {
			t.Logf("  %s: %v -> %v", c.Variable, c.From, c.To)
		}
	}

	// Verify max_temp_c is NOT in changes (no change).
	for _, c := range changes {
		if c.Variable == "max_temp_c" {
			t.Error("max_temp_c should not be in changes (no change)")
		}
	}
}

func TestGenerate_ExactlyMaxTriggeredPeriods(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	// Exactly 20 triggered periods (should NOT truncate).
	periods := make([]types.TimeRange, 20)
	for i := range periods {
		periods[i] = types.TimeRange{
			Start: now.Add(time.Duration(i) * time.Hour),
			End:   now.Add(time.Duration(i)*time.Hour + 30*time.Minute),
		}
	}

	current := &types.MonitorSummary{
		WindowStart:      now,
		WindowEnd:        now.Add(48 * time.Hour),
		MaxValues:        map[string]float64{"precip_prob": 90.0},
		TriggeredPeriods: periods,
	}

	result, err := gen.Generate(ctx, wp, current, nil, testDigestConfig(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.TriggeredPeriods) != 20 {
		t.Errorf("TriggeredPeriods count = %d, want 20 (not truncated)", len(result.TriggeredPeriods))
	}
	if result.RemainingCount != 0 {
		t.Errorf("RemainingCount = %d, want 0 (not truncated)", result.RemainingCount)
	}
}

func TestGenerate_UnknownMaxValueKeys(t *testing.T) {
	gen := NewGenerator()
	ctx := context.Background()
	wp := testWatchPoint()
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)

	// Current has an extra key not in the standard set.
	current := &types.MonitorSummary{
		WindowStart: now,
		WindowEnd:   now.Add(24 * time.Hour),
		MaxValues: map[string]float64{
			"precip_prob":  80.0,
			"wind_speed":   50.0, // extra key
		},
		TriggeredPeriods: nil,
	}

	previous := &DigestContent{
		ForecastSummary: ForecastSummary{
			MaxPrecipProb: 30.0,
		},
		TriggeredPeriods: nil,
	}

	result, err := gen.Generate(ctx, wp, current, previous, testDigestConfig(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should detect the wind_speed as new (non-zero, not in previous).
	foundWindSpeed := false
	for _, c := range result.Comparison.Changes {
		if c.Variable == "wind_speed" {
			foundWindSpeed = true
			if c.From != 0 || c.To != 50.0 {
				t.Errorf("wind_speed: From=%v To=%v, want From=0 To=50", c.From, c.To)
			}
		}
	}
	if !foundWindSpeed {
		t.Error("expected wind_speed in changes (new variable)")
	}
}
