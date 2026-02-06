package webhook

import (
	"fmt"
	"strings"

	"watchpoint/internal/types"
)

// formatTitle generates a human-readable title for a notification based on
// its event type and urgency.
func formatTitle(n *types.Notification) string {
	var prefix string
	switch n.EventType {
	case types.EventThresholdCrossed:
		prefix = "Threshold Alert"
	case types.EventThresholdCleared:
		prefix = "Threshold Cleared"
	case types.EventForecastChanged:
		prefix = "Forecast Update"
	case types.EventImminentAlert:
		prefix = "Imminent Alert"
	case types.EventDigest:
		prefix = "Monitor Digest"
	case types.EventSystemAlert:
		prefix = "System Alert"
	case types.EventBillingWarning:
		prefix = "Billing Warning"
	case types.EventBillingReceipt:
		prefix = "Billing Receipt"
	default:
		prefix = "Notification"
	}

	// Include WatchPoint name if available.
	if n.Payload != nil {
		if name, ok := n.Payload["watchpoint_name"].(string); ok && name != "" {
			return fmt.Sprintf("%s: %s", prefix, name)
		}
	}

	return prefix
}

// extractTriggeredPeriods extracts triggered period descriptions from the
// notification payload. Returns a formatted string slice for rendering.
func extractTriggeredPeriods(payload map[string]interface{}) ([]string, bool) {
	if payload == nil {
		return nil, false
	}

	periods, ok := payload["triggered_periods"].([]interface{})
	if !ok {
		return nil, false
	}

	var result []string
	for _, p := range periods {
		switch v := p.(type) {
		case string:
			result = append(result, v)
		case map[string]interface{}:
			start, _ := v["start"].(string)
			end, _ := v["end"].(string)
			if start != "" && end != "" {
				result = append(result, fmt.Sprintf("%s - %s", start, end))
			} else if start != "" {
				result = append(result, start)
			}
		}
	}

	return result, len(result) > 0
}

// capitalizeFirst returns the string with its first letter capitalized.
// Used internally for display formatting in notifications.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
