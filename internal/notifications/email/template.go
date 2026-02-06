package email

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"watchpoint/internal/types"
)

// TemplateData is the flattened key-value map passed to the email provider's
// dynamic template engine. Keys are strings; values are interface{} to
// support mixed types (strings, numbers, booleans).
type TemplateData map[string]interface{}

// TemplateConfig maps internal template set names to provider-specific template
// IDs. The outer key is the template set name (e.g., "default", "wedding"),
// and the inner key is the EventType (e.g., "threshold_crossed").
type TemplateConfig struct {
	Sets map[string]map[types.EventType]string `json:"sets"`
}

// Validate ensures the TemplateConfig has a "default" set that covers
// all critical EventTypes. Returns an error if validation fails.
func (c *TemplateConfig) Validate() error {
	if c.Sets == nil {
		return fmt.Errorf("template config: sets map is nil")
	}

	defaultSet, ok := c.Sets["default"]
	if !ok {
		return fmt.Errorf("template config: missing required 'default' template set")
	}

	// Verify the default set has entries for critical event types.
	criticalTypes := []types.EventType{
		types.EventThresholdCrossed,
		types.EventThresholdCleared,
		types.EventSystemAlert,
	}

	for _, et := range criticalTypes {
		if _, found := defaultSet[et]; !found {
			return fmt.Errorf("template config: default set missing critical event type %q", et)
		}
	}

	return nil
}

// TemplateService defines the interface for template resolution and data
// preparation. Implementations handle soft-fail fallback to the "default"
// template set and timezone-aware data formatting.
type TemplateService interface {
	// Resolve returns the provider template ID for the given template set and
	// event type. Implements soft-fail fallback: if the requested set is not
	// found, falls back to "default" before returning an error.
	Resolve(set string, eventType types.EventType) (string, error)

	// Prepare converts a Notification into flat template variables with
	// timezone-localized timestamps and a SenderIdentity.
	Prepare(n *types.Notification) (TemplateData, types.SenderIdentity, error)
}

// TemplateEngine is the production implementation of TemplateService.
// It loads template configuration at startup and provides resolution with
// automatic fallback to the "default" template set (VERT-003/VERT-004).
type TemplateEngine struct {
	config          TemplateConfig
	defaultFromAddr string
	defaultFromName string
	logger          types.Logger
}

// TemplateEngineConfig holds the parameters needed to construct a TemplateEngine.
type TemplateEngineConfig struct {
	// TemplatesJSON is the raw JSON string from EMAIL_TEMPLATES_JSON.
	TemplatesJSON string
	// DefaultFromAddr is the default sender email address.
	DefaultFromAddr string
	// DefaultFromName is the default sender display name.
	DefaultFromName string
	// Logger for template operations.
	Logger types.Logger
}

// NewTemplateEngine parses the JSON template configuration and returns a
// TemplateEngine. Returns an error if the JSON is invalid or the configuration
// fails validation.
func NewTemplateEngine(cfg TemplateEngineConfig) (*TemplateEngine, error) {
	var tc TemplateConfig
	if err := json.Unmarshal([]byte(cfg.TemplatesJSON), &tc); err != nil {
		return nil, fmt.Errorf("template engine: failed to parse templates JSON: %w", err)
	}

	if err := tc.Validate(); err != nil {
		return nil, fmt.Errorf("template engine: %w", err)
	}

	return &TemplateEngine{
		config:          tc,
		defaultFromAddr: cfg.DefaultFromAddr,
		defaultFromName: cfg.DefaultFromName,
		logger:          cfg.Logger,
	}, nil
}

// Resolve returns the provider template ID for the given template set and
// event type. It implements the soft-fail fallback logic from VERT-003:
//
//  1. Attempt resolution using the requested set.
//  2. If not found, fall back to the "default" set.
//  3. Return error only if both attempts fail.
func (e *TemplateEngine) Resolve(set string, eventType types.EventType) (string, error) {
	// Step 1: Try the requested set.
	if templateSet, ok := e.config.Sets[set]; ok {
		if tmplID, found := templateSet[eventType]; found {
			return tmplID, nil
		}
	}

	// Step 2: Fall back to "default" (unless we already tried "default").
	if set != "default" {
		e.logger.Warn("template set not found, falling back to default",
			"requested_set", set,
			"event_type", string(eventType),
		)

		if defaultSet, ok := e.config.Sets["default"]; ok {
			if tmplID, found := defaultSet[eventType]; found {
				return tmplID, nil
			}
		}
	}

	// Step 3: Both failed.
	return "", fmt.Errorf("template engine: no template found for event type %q in set %q or default", eventType, set)
}

// Prepare converts a Notification into flattened template variables suitable
// for provider-side rendering, along with the appropriate SenderIdentity.
//
// Behavior:
//   - Extracts "timezone" from n.Payload (defaults to UTC if missing/invalid).
//   - Converts UTC timestamps to human-readable strings in the user's timezone.
//   - Formats numbers (e.g., probability 0.55 -> "55%").
//   - Determines Sender Name based on the TemplateSet.
func (e *TemplateEngine) Prepare(n *types.Notification) (TemplateData, types.SenderIdentity, error) {
	if n == nil {
		return nil, types.SenderIdentity{}, fmt.Errorf("template engine: notification is nil")
	}

	// Resolve timezone from payload.
	loc := resolveTimezone(n.Payload)

	// Build the flat template data map from the notification.
	data := make(TemplateData)

	// Copy all payload fields as-is for template access.
	for k, v := range n.Payload {
		data[k] = v
	}

	// Add core notification metadata.
	data["notification_id"] = n.ID
	data["watchpoint_id"] = n.WatchPointID
	data["organization_id"] = n.OrganizationID
	data["event_type"] = string(n.EventType)
	data["urgency"] = string(n.Urgency)
	data["template_set"] = n.TemplateSet

	// Format timestamps in the user's timezone.
	now := time.Now().UTC()
	data["formatted_date"] = now.In(loc).Format("Mon, Jan 2 at 3:04 PM")
	data["formatted_date_short"] = now.In(loc).Format("Jan 2, 2006")
	data["timezone_name"] = loc.String()

	// Format created_at if available.
	if !n.CreatedAt.IsZero() {
		data["created_at_formatted"] = n.CreatedAt.In(loc).Format("Mon, Jan 2 at 3:04 PM")
	}

	// Format specific payload fields that need transformation.
	formatPayloadNumbers(data)
	formatPayloadTimestamps(data, loc)

	// Determine sender identity based on template set.
	sender := types.SenderIdentity{
		Address: e.defaultFromAddr,
		Name:    e.determineSenderName(n.TemplateSet),
	}

	return data, sender, nil
}

// determineSenderName returns the appropriate sender name for the template set.
// Custom template sets get a branded sender name; the default set uses the
// configured default.
func (e *TemplateEngine) determineSenderName(templateSet string) string {
	if templateSet == "" || templateSet == "default" {
		return e.defaultFromName
	}

	// For vertical/custom template sets, include the set name in the sender.
	// e.g., "WatchPoint Alerts (wedding)"
	return fmt.Sprintf("%s (%s)", e.defaultFromName, templateSet)
}

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
	// Look for common probability fields and format them.
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
// Handles float64, int, int64, and string types.
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

// Compile-time assertion that TemplateEngine implements TemplateService.
var _ TemplateService = (*TemplateEngine)(nil)

