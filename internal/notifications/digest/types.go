// Package digest implements the DigestGenerator for Monitor Mode summaries.
// It compares current forecast data against previous digest snapshots to
// produce diff-aware content suitable for template rendering.
//
// Architecture reference: 08a-notification-core.md (Section 4.2, 6.2)
// Flow reference: NOTIF-003, NOTIF-003b
package digest

import (
	"context"
	"time"

	"watchpoint/internal/types"
)

// DigestGenerator encapsulates logic for Monitor Mode summaries.
//
// The interface signature uses *types.MonitorSummary for the current forecast data
// because the flow simulation (NOTIF-003) specifies that the generator reads
// last_forecast_summary (MonitorSummary) and last_digest_content (DigestContent)
// from the evaluation state. The MonitorSummary contains the MaxValues map and
// TriggeredPeriods needed for comparison.
//
// Logic Updates (from 08a-notification-core.md Section 6.2):
//  1. Empty Check: If 0 triggered periods and 0 significant changes,
//     check DigestConfig.SendEmpty. If false, return ErrDigestEmpty.
//  2. Template Selection: Use DigestConfig.TemplateSet if provided (caller concern).
//  3. Truncation: If len(TriggeredPeriods) > 20, truncate and set RemainingCount.
type DigestGenerator interface {
	Generate(ctx context.Context,
		wp *types.WatchPoint,
		current *types.MonitorSummary,
		previous *DigestContent,
		digestConfig *types.DigestConfig) (*DigestContent, error)
}

// DigestContent defines the structure stored in watchpoint_evaluation_state.last_digest_content.
// It contains the summary of the current forecast window, triggered periods,
// and a comparison to the previous digest.
type DigestContent struct {
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`

	// Summary of the forecast window
	ForecastSummary ForecastSummary `json:"forecast_summary"`

	// Specific periods where conditions are met
	TriggeredPeriods []types.TimeRange `json:"triggered_periods"`

	// RemainingCount is set when TriggeredPeriods was truncated (> maxTriggeredPeriods).
	// It indicates how many additional periods were omitted.
	RemainingCount int `json:"remaining_count,omitempty"`

	// Comparison to the previous digest sent (nil on first digest)
	Comparison *DigestComparison `json:"comparison,omitempty"`
}

// ForecastSummary captures the key forecast metrics for the digest window.
type ForecastSummary struct {
	MaxPrecipProb float64 `json:"max_precip_prob"`
	MaxPrecipMM   float64 `json:"max_precip_mm"`
	MinTempC      float64 `json:"min_temp_c"`
	MaxTempC      float64 `json:"max_temp_c"`
}

// DigestComparison describes how the current digest differs from the previous one.
type DigestComparison struct {
	Direction string   `json:"direction"` // "worsening", "improving", "stable"
	Changes   []Change `json:"changes"`
}

// Change captures a single variable's movement between two digest snapshots.
type Change struct {
	Variable string  `json:"variable"`
	From     float64 `json:"from"`
	To       float64 `json:"to"`
}

// ThreatStatus describes the relationship between current and previous triggered periods.
type ThreatStatus string

const (
	// ThreatNew indicates conditions are now triggered that were not before.
	ThreatNew ThreatStatus = "new_threat"

	// ThreatCleared indicates previously triggered conditions have cleared.
	ThreatCleared ThreatStatus = "cleared"

	// ThreatPersisting indicates triggered conditions continue from the previous digest.
	ThreatPersisting ThreatStatus = "persisting"

	// ThreatNone indicates no triggered periods in either current or previous.
	ThreatNone ThreatStatus = "none"
)
