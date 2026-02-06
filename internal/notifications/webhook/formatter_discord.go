package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"watchpoint/internal/types"
)

// Discord color codes for urgency levels (decimal color values).
const (
	colorRoutine  = 0x2196F3 // Blue
	colorWatch    = 0xFFC107 // Amber
	colorWarning  = 0xFF9800 // Orange
	colorCritical = 0xF44336 // Red
	colorCleared  = 0x4CAF50 // Green
)

// DiscordFormatter formats notifications as Discord webhook JSON with embeds.
//
// Architecture reference: 08c-webhook-worker.md Section 4.3
type DiscordFormatter struct{}

// Platform returns the platform identifier.
func (f *DiscordFormatter) Platform() Platform {
	return PlatformDiscord
}

// Format transforms a Notification into Discord webhook JSON.
func (f *DiscordFormatter) Format(_ context.Context, n *types.Notification, _ map[string]any) ([]byte, error) {
	if n == nil {
		return nil, fmt.Errorf("discord formatter: notification is nil")
	}

	title := formatTitle(n)
	color := urgencyColor(n)

	embed := DiscordEmbed{
		Title:       title,
		Description: buildDiscordDescription(n),
		Color:       color,
		Fields:      buildDiscordFields(n),
		Footer: &DiscordFooter{
			Text: fmt.Sprintf("WatchPoint Alerts | %s | %s", string(n.Urgency), string(n.EventType)),
		},
	}

	payload := DiscordPayload{
		Username:  "WatchPoint",
		AvatarURL: "",
		Content:   fmt.Sprintf("[%s] %s", strings.ToUpper(string(n.Urgency)), title),
		Embeds:    []DiscordEmbed{embed},
	}

	return json.Marshal(payload)
}

// ValidateResponse checks the Discord webhook response. Discord returns 204
// No Content on success for webhook messages.
func (f *DiscordFormatter) ValidateResponse(statusCode int, body []byte) error {
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}

	// Check for Discord-specific error responses.
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err == nil {
		if msg, ok := resp["message"].(string); ok {
			return fmt.Errorf("discord: API error: %s", msg)
		}
	}

	return fmt.Errorf("discord: unexpected status %d: %s", statusCode, truncateBody(body))
}

// urgencyColor returns the Discord embed color based on urgency and event type.
func urgencyColor(n *types.Notification) int {
	// Cleared events get green regardless of urgency.
	if n.EventType == types.EventThresholdCleared {
		return colorCleared
	}

	switch n.Urgency {
	case types.UrgencyCritical:
		return colorCritical
	case types.UrgencyWarning:
		return colorWarning
	case types.UrgencyWatch:
		return colorWatch
	default:
		return colorRoutine
	}
}

// buildDiscordDescription creates the embed description from payload data.
func buildDiscordDescription(n *types.Notification) string {
	if n.Payload == nil {
		return string(n.EventType)
	}

	var parts []string

	if name, ok := n.Payload["watchpoint_name"].(string); ok && name != "" {
		parts = append(parts, fmt.Sprintf("**WatchPoint**: %s", name))
	}

	if loc, ok := n.Payload["location"].(map[string]interface{}); ok {
		if label, ok := loc["label"].(string); ok && label != "" {
			parts = append(parts, fmt.Sprintf("**Location**: %s", label))
		}
	}

	if len(parts) == 0 {
		return string(n.EventType)
	}

	return strings.Join(parts, "\n")
}

// buildDiscordFields creates embed fields from notification conditions.
func buildDiscordFields(n *types.Notification) []DiscordField {
	var fields []DiscordField

	if n.Payload == nil {
		return fields
	}

	conditions, ok := n.Payload["conditions"].([]interface{})
	if !ok {
		return fields
	}

	for _, c := range conditions {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		variable, _ := cm["variable"].(string)
		currentVal, _ := cm["current_value"].(float64)
		threshold, _ := cm["threshold"].(float64)
		operator, _ := cm["operator"].(string)
		if variable != "" {
			fields = append(fields, DiscordField{
				Name:   variable,
				Value:  fmt.Sprintf("%.1f (%s %.1f)", currentVal, operator, threshold),
				Inline: true,
			})
		}
	}

	return fields
}
