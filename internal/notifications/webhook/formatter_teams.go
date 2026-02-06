package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"watchpoint/internal/types"
)

// TeamsFormatter formats notifications as Microsoft Teams Adaptive Card JSON
// targeting the Power Automate Workflow schema.
//
// Architecture reference: 08c-webhook-worker.md Section 4.2
type TeamsFormatter struct{}

// Platform returns the platform identifier.
func (f *TeamsFormatter) Platform() Platform {
	return PlatformTeams
}

// Format transforms a Notification into Teams Adaptive Card JSON.
func (f *TeamsFormatter) Format(_ context.Context, n *types.Notification, _ map[string]any) ([]byte, error) {
	if n == nil {
		return nil, fmt.Errorf("teams formatter: notification is nil")
	}

	title := formatTitle(n)

	// Build Adaptive Card body items.
	body := []AdaptiveItem{
		{
			Type:   "TextBlock",
			Text:   title,
			Size:   "Large",
			Weight: "Bolder",
			Wrap:   true,
		},
	}

	// Add fact set with notification details.
	facts := buildTeamsFacts(n)
	if len(facts) > 0 {
		body = append(body, AdaptiveItem{
			Type:  "FactSet",
			Facts: facts,
		})
	}

	// Add condition details if present.
	if desc := buildConditionDescription(n); desc != "" {
		body = append(body, AdaptiveItem{
			Type: "TextBlock",
			Text: desc,
			Wrap: true,
		})
	}

	// Add footer with urgency context.
	body = append(body, AdaptiveItem{
		Type: "TextBlock",
		Text: fmt.Sprintf("Urgency: %s | Event: %s", string(n.Urgency), string(n.EventType)),
		Size: "Small",
		Wrap: true,
	})

	payload := TeamsPayload{
		Type: "message",
		Attachments: []TeamsAttachment{
			{
				ContentType: "application/vnd.microsoft.card.adaptive",
				Content: AdaptiveCard{
					Type:    "AdaptiveCard",
					Version: "1.4",
					Body:    body,
				},
			},
		},
	}

	return json.Marshal(payload)
}

// ValidateResponse checks the Teams webhook response. Teams Power Automate
// Workflows respond with 202 Accepted on success.
func (f *TeamsFormatter) ValidateResponse(statusCode int, body []byte) error {
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}
	return fmt.Errorf("teams: unexpected status %d: %s", statusCode, truncateBody(body))
}

// buildTeamsFacts creates fact pairs from notification data for the FactSet.
func buildTeamsFacts(n *types.Notification) []Fact {
	var facts []Fact

	if n.Payload == nil {
		return facts
	}

	if name, ok := n.Payload["watchpoint_name"].(string); ok && name != "" {
		facts = append(facts, Fact{Title: "WatchPoint", Value: name})
	}

	if loc, ok := n.Payload["location"].(map[string]interface{}); ok {
		if label, ok := loc["label"].(string); ok && label != "" {
			facts = append(facts, Fact{Title: "Location", Value: label})
		}
	}

	facts = append(facts, Fact{Title: "Urgency", Value: capitalizeFirst(string(n.Urgency))})
	facts = append(facts, Fact{Title: "Event Type", Value: string(n.EventType)})

	return facts
}

// buildConditionDescription builds a text summary of conditions from the payload.
func buildConditionDescription(n *types.Notification) string {
	if n.Payload == nil {
		return ""
	}

	conditions, ok := n.Payload["conditions"].([]interface{})
	if !ok || len(conditions) == 0 {
		return ""
	}

	var parts []string
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
			parts = append(parts, fmt.Sprintf("%s: %.1f (%s %.1f)", variable, currentVal, operator, threshold))
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return "Conditions: " + strings.Join(parts, ", ")
}

// truncateBody returns a truncated version of the response body for error messages.
func truncateBody(body []byte) string {
	const maxLen = 200
	if len(body) > maxLen {
		return string(body[:maxLen]) + "..."
	}
	return string(body)
}
