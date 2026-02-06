package digest

import (
	"context"
	"math"

	"watchpoint/internal/notifications/core"
	"watchpoint/internal/types"
)

// maxTriggeredPeriods is the truncation limit for triggered periods.
// If the count exceeds this, the list is truncated and RemainingCount is set.
const maxTriggeredPeriods = 20

// defaultChangeThresholdPercent is the minimum percentage change in a MaxValue
// that qualifies as a "significant change" for inclusion in the comparison.
// Per flow NOTIF-003b, a delta > 20% is considered significant.
// The percentage is calculated relative to max(abs(previous), abs(current)).
const defaultChangeThresholdPercent = 20.0

// Known MaxValues keys mapped to their ForecastSummary field names.
// These are the variables the Python Eval Worker writes into MonitorSummary.MaxValues.
const (
	keyPrecipProb = "precip_prob"
	keyPrecipMM   = "precip_mm"
	keyMinTempC   = "min_temp_c"
	keyMaxTempC   = "max_temp_c"
)

// Compile-time interface compliance check.
var _ DigestGenerator = (*Generator)(nil)

// Generator implements the DigestGenerator interface from 08a-notification-core.md.
//
// Logic:
//  1. Extracts ForecastSummary from the current EvaluationResult's MonitorSummary.
//  2. If a previous digest exists, compares MaxValues and TriggeredPeriods (NOTIF-003b).
//  3. Checks DigestConfig.SendEmpty: if no triggers and no significant changes, returns ErrDigestEmpty.
//  4. Truncates TriggeredPeriods if len > 20.
//  5. Returns DigestContent ready for template rendering.
type Generator struct{}

// NewGenerator creates a new DigestGenerator implementation.
func NewGenerator() *Generator {
	return &Generator{}
}

// Generate produces a DigestContent by comparing current forecast data against
// the previous digest snapshot. It implements the DigestGenerator interface
// defined in 08a-notification-core.md Section 6.2.
//
// Parameters:
//   - ctx: Context for cancellation/deadline propagation.
//   - wp: The WatchPoint being evaluated (provides identity and config).
//   - current: The current MonitorSummary from the evaluation state.
//   - previous: The previous DigestContent (nil on first digest).
//   - digestConfig: Controls SendEmpty behavior and template selection.
//
// Returns ErrDigestEmpty (from core package) when SendEmpty=false and the
// digest has no triggered periods and no significant changes.
func (g *Generator) Generate(
	ctx context.Context,
	wp *types.WatchPoint,
	current *types.MonitorSummary,
	previous *DigestContent,
	digestConfig *types.DigestConfig,
) (*DigestContent, error) {
	if current == nil {
		// No current data to generate a digest from. Check SendEmpty.
		if digestConfig != nil && !digestConfig.SendEmpty {
			return nil, core.ErrDigestEmpty
		}
		// Return an empty digest structure.
		return &DigestContent{}, nil
	}

	// Step 1: Build ForecastSummary from MonitorSummary.MaxValues.
	summary := extractForecastSummary(current.MaxValues)

	// Step 2: Prepare triggered periods with truncation.
	triggeredPeriods := current.TriggeredPeriods
	remainingCount := 0
	if len(triggeredPeriods) > maxTriggeredPeriods {
		remainingCount = len(triggeredPeriods) - maxTriggeredPeriods
		triggeredPeriods = triggeredPeriods[:maxTriggeredPeriods]
	}

	// Step 3: Compare with previous digest (NOTIF-003b).
	var comparison *DigestComparison
	if previous != nil {
		comparison = compareDigests(current, previous)
	}

	// Step 4: Check SendEmpty condition.
	// "If the generated digest contains 0 triggered periods and 0 significant changes,
	//  check DigestConfig.SendEmpty. If false, abort generation and return ErrDigestEmpty."
	if digestConfig != nil && !digestConfig.SendEmpty {
		hasTriggeredPeriods := len(current.TriggeredPeriods) > 0
		hasSignificantChanges := comparison != nil && len(comparison.Changes) > 0
		if !hasTriggeredPeriods && !hasSignificantChanges {
			return nil, core.ErrDigestEmpty
		}
	}

	// Step 5: Build the final DigestContent.
	content := &DigestContent{
		PeriodStart:      current.WindowStart,
		PeriodEnd:        current.WindowEnd,
		ForecastSummary:  summary,
		TriggeredPeriods: triggeredPeriods,
		RemainingCount:   remainingCount,
		Comparison:       comparison,
	}

	return content, nil
}

// extractForecastSummary maps the generic MaxValues map from MonitorSummary
// into the typed ForecastSummary struct. Missing keys default to zero values.
func extractForecastSummary(maxValues map[string]float64) ForecastSummary {
	return ForecastSummary{
		MaxPrecipProb: maxValues[keyPrecipProb],
		MaxPrecipMM:   maxValues[keyPrecipMM],
		MinTempC:      maxValues[keyMinTempC],
		MaxTempC:      maxValues[keyMaxTempC],
	}
}

// compareDigests implements the NOTIF-003b sub-flow.
// It compares MaxValues (looking for significant deltas) and
// TriggeredPeriods (detecting New Threat vs Cleared transitions).
func compareDigests(current *types.MonitorSummary, previous *DigestContent) *DigestComparison {
	changes := compareMaxValues(current.MaxValues, previous.ForecastSummary)
	threatStatus := compareThreatStatus(current, previous)

	// Add threat status change as a synthetic Change entry if applicable.
	if threatStatus == ThreatNew {
		changes = append(changes, Change{
			Variable: "threat_status",
			From:     0, // no threat
			To:       1, // threat active
		})
	} else if threatStatus == ThreatCleared {
		changes = append(changes, Change{
			Variable: "threat_status",
			From:     1, // threat active
			To:       0, // cleared
		})
	}

	direction := determineDirection(changes)

	return &DigestComparison{
		Direction: direction,
		Changes:   changes,
	}
}

// compareMaxValues compares the current MonitorSummary.MaxValues against the
// previous ForecastSummary, returning a list of significant changes.
//
// Per NOTIF-003b: Delta = Current.MaxVal - Previous.MaxVal.
// If abs(Delta) > Threshold (e.g., 20%), add to Changes list.
func compareMaxValues(currentMax map[string]float64, prevSummary ForecastSummary) []Change {
	var changes []Change

	// Map previous summary to the same key space for comparison.
	prevValues := map[string]float64{
		keyPrecipProb: prevSummary.MaxPrecipProb,
		keyPrecipMM:   prevSummary.MaxPrecipMM,
		keyMinTempC:   prevSummary.MinTempC,
		keyMaxTempC:   prevSummary.MaxTempC,
	}

	for key, currentVal := range currentMax {
		prevVal, exists := prevValues[key]
		if !exists {
			// New variable not seen in previous; always report if non-zero.
			if currentVal != 0 {
				changes = append(changes, Change{
					Variable: key,
					From:     0,
					To:       currentVal,
				})
			}
			continue
		}

		if isSignificantChange(prevVal, currentVal) {
			changes = append(changes, Change{
				Variable: key,
				From:     prevVal,
				To:       currentVal,
			})
		}
	}

	return changes
}

// isSignificantChange determines whether a value change exceeds the significance
// threshold. Uses a percentage-based check: abs(delta) > 20% of the reference value.
// For values near zero, we use a small absolute threshold to avoid division issues.
func isSignificantChange(previous, current float64) bool {
	delta := current - previous
	absDelta := math.Abs(delta)

	// Use the larger of the two values as the reference for percentage calculation.
	reference := math.Max(math.Abs(previous), math.Abs(current))

	// For very small values, skip percentage check and use absolute threshold.
	if reference < 1e-9 {
		return false // Both values are essentially zero.
	}

	percentChange := (absDelta / reference) * 100.0
	return percentChange > defaultChangeThresholdPercent
}

// compareThreatStatus compares the TriggeredPeriods between current and previous
// to determine the threat transition status (NOTIF-003b).
func compareThreatStatus(current *types.MonitorSummary, previous *DigestContent) ThreatStatus {
	currentHasThreats := len(current.TriggeredPeriods) > 0
	previousHasThreats := len(previous.TriggeredPeriods) > 0

	switch {
	case currentHasThreats && !previousHasThreats:
		return ThreatNew
	case !currentHasThreats && previousHasThreats:
		return ThreatCleared
	case currentHasThreats && previousHasThreats:
		return ThreatPersisting
	default:
		return ThreatNone
	}
}

// determineDirection analyzes the aggregate changes to determine the overall
// direction of forecast evolution: "worsening", "improving", or "stable".
func determineDirection(changes []Change) string {
	if len(changes) == 0 {
		return "stable"
	}

	worsening := 0
	improving := 0

	for _, c := range changes {
		switch c.Variable {
		case "threat_status":
			// Threat appearing = worsening, clearing = improving
			if c.To > c.From {
				worsening++
			} else {
				improving++
			}
		case keyPrecipProb, keyPrecipMM:
			// Higher precipitation = worsening
			if c.To > c.From {
				worsening++
			} else {
				improving++
			}
		case keyMinTempC:
			// Lower min temp = worsening (cold snap)
			if c.To < c.From {
				worsening++
			} else {
				improving++
			}
		case keyMaxTempC:
			// Higher max temp = worsening (heat)
			if c.To > c.From {
				worsening++
			} else {
				improving++
			}
		default:
			// Unknown variables: higher = worse (conservative assumption)
			if c.To > c.From {
				worsening++
			} else {
				improving++
			}
		}
	}

	switch {
	case worsening > improving:
		return "worsening"
	case improving > worsening:
		return "improving"
	default:
		return "stable"
	}
}
