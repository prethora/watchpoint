package email

import (
	"encoding/json"
	"strconv"
	"time"

	"watchpoint/internal/types"
)

// TemplateData is the flattened key-value map used internally by helper
// functions for payload formatting (probability, timestamp conversions).
type TemplateData map[string]interface{}

// TemplateService defines the interface for client-side email template
// rendering. Implementations render a Notification into pre-built email
// content (Subject, BodyHTML, BodyText) and determine the SenderIdentity.
type TemplateService interface {
	// Render converts a Notification into a fully rendered email with
	// Subject, BodyHTML, and BodyText, plus the SenderIdentity.
	// The set parameter is used for sender name customization (soft-fail
	// fallback per VERT-003/VERT-004).
	Render(set string, eventType types.EventType, n *types.Notification) (*RenderedEmail, types.SenderIdentity, error)
}

// ---------------------------------------------------------------------------
// Helper Functions (shared between Renderer and tests)
// ---------------------------------------------------------------------------

// resolveTimezone extracts the "timezone" key from the payload map and loads
// the corresponding time.Location. Returns UTC if the timezone is missing,
// empty, or invalid.
func resolveTimezone(payload map[string]interface{}) *time.Location {
	if payload == nil {
		return time.UTC
	}

	tzStr, ok := payload["timezone"].(string)
	if !ok || tzStr == "" {
		return time.UTC
	}

	loc, err := time.LoadLocation(tzStr)
	if err != nil {
		return time.UTC
	}

	return loc
}

// formatPayloadNumbers converts numeric payload values that represent
// probabilities (0.0-1.0) into percentage strings for display.
func formatPayloadNumbers(data TemplateData) {
	probabilityFields := []string{
		"precipitation_probability",
		"probability",
		"confidence",
	}

	for _, field := range probabilityFields {
		if val, ok := data[field]; ok {
			if num, isNum := toFloat64(val); isNum && num >= 0 && num <= 1 {
				data[field+"_formatted"] = strconv.FormatFloat(num*100, 'f', 0, 64) + "%"
			}
		}
	}
}

// formatPayloadTimestamps converts UTC timestamp strings in the payload to
// formatted strings in the user's timezone.
func formatPayloadTimestamps(data TemplateData, loc *time.Location) {
	timestampFields := []string{
		"forecast_time",
		"threshold_crossed_at",
		"threshold_cleared_at",
		"valid_time",
	}

	for _, field := range timestampFields {
		if val, ok := data[field]; ok {
			if ts, isStr := val.(string); isStr {
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					data[field+"_formatted"] = t.In(loc).Format("Mon, Jan 2 at 3:04 PM")
				}
			}
		}
	}
}

// toFloat64 attempts to convert an interface{} value to float64.
// Handles float64, int, int64, json.Number, and string types.
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	default:
		return 0, false
	}
}
