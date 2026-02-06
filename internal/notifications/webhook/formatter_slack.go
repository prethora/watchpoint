package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"watchpoint/internal/types"
)

// maxSlackTriggeredPeriods is the maximum number of triggered periods to include
// in a Slack message. Excess periods are summarized in a footer (NOTIF-007 flow).
const maxSlackTriggeredPeriods = 5

// SlackFormatter formats notifications as Slack Block Kit JSON.
//
// Architecture reference: 08c-webhook-worker.md Section 4.1
type SlackFormatter struct{}

// Platform returns the platform identifier.
func (f *SlackFormatter) Platform() Platform {
	return PlatformSlack
}

// Format transforms a Notification into Slack Block Kit JSON.
func (f *SlackFormatter) Format(_ context.Context, n *types.Notification, _ map[string]any) ([]byte, error) {
	if n == nil {
		return nil, fmt.Errorf("slack formatter: notification is nil")
	}

	title := formatTitle(n)
	fallbackText := fmt.Sprintf("[%s] %s", strings.ToUpper(string(n.Urgency)), title)

	payload := SlackPayload{
		Text: fallbackText,
		Blocks: []SlackBlock{
			{
				Type: "header",
				Text: &SlackText{
					Type: "plain_text",
					Text: title,
				},
			},
		},
	}

	// Add event details section.
	fields := buildSlackFields(n)
	if len(fields) > 0 {
		payload.Blocks = append(payload.Blocks, SlackBlock{
			Type:   "section",
			Fields: fields,
		})
	}

	// Add triggered periods if present in payload.
	if periods, ok := extractTriggeredPeriods(n.Payload); ok && len(periods) > 0 {
		total := len(periods)
		if total > maxSlackTriggeredPeriods {
			periods = periods[:maxSlackTriggeredPeriods]
		}

		for _, period := range periods {
			payload.Blocks = append(payload.Blocks, SlackBlock{
				Type: "section",
				Text: &SlackText{
					Type: "mrkdwn",
					Text: period,
				},
			})
		}

		if total > maxSlackTriggeredPeriods {
			remaining := total - maxSlackTriggeredPeriods
			payload.Blocks = append(payload.Blocks, SlackBlock{
				Type: "context",
				Elements: []*SlackText{
					{
						Type: "mrkdwn",
						Text: fmt.Sprintf("...and %d more events.", remaining),
					},
				},
			})
		}
	}

	// Add context footer with urgency and event type.
	payload.Blocks = append(payload.Blocks, SlackBlock{
		Type: "context",
		Elements: []*SlackText{
			{
				Type: "mrkdwn",
				Text: fmt.Sprintf("*Urgency*: %s | *Event*: %s | WatchPoint Alerts", string(n.Urgency), string(n.EventType)),
			},
		},
	})

	return json.Marshal(payload)
}

// ValidateResponse checks for Slack's "soft failure" pattern where the API
// returns HTTP 200 but the body indicates an error (e.g., "ok": false or
// a plain text error message).
func (f *SlackFormatter) ValidateResponse(statusCode int, body []byte) error {
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("slack: unexpected status %d", statusCode)
	}

	bodyStr := strings.TrimSpace(string(body))

	// Slack incoming webhooks return "ok" as plain text on success.
	if bodyStr == "ok" {
		return nil
	}

	// Check for JSON response with "ok": false.
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err == nil {
		if ok, exists := resp["ok"]; exists {
			if okBool, isBool := ok.(bool); isBool && !okBool {
				errMsg := "unknown error"
				if e, exists := resp["error"]; exists {
					if eStr, isStr := e.(string); isStr {
						errMsg = eStr
					}
				}
				return fmt.Errorf("slack: API error: %s", errMsg)
			}
		}
	}

	// Check for common plain-text error responses.
	knownErrors := []string{
		"no_text",
		"channel_not_found",
		"channel_is_archived",
		"invalid_payload",
		"too_many_attachments",
	}
	for _, known := range knownErrors {
		if bodyStr == known {
			return fmt.Errorf("slack: API error: %s", bodyStr)
		}
	}

	return nil
}

// buildSlackFields creates field pairs from notification data.
func buildSlackFields(n *types.Notification) []*SlackText {
	var fields []*SlackText

	if n.Payload == nil {
		return fields
	}

	// Add WatchPoint name if available.
	if name, ok := n.Payload["watchpoint_name"].(string); ok && name != "" {
		fields = append(fields, &SlackText{Type: "mrkdwn", Text: fmt.Sprintf("*WatchPoint*\n%s", name)})
	}

	// Add location if available.
	if loc, ok := n.Payload["location"].(map[string]interface{}); ok {
		if label, ok := loc["label"].(string); ok && label != "" {
			fields = append(fields, &SlackText{Type: "mrkdwn", Text: fmt.Sprintf("*Location*\n%s", label)})
		}
	}

	// Add condition details for threshold events.
	if conditions, ok := n.Payload["conditions"].([]interface{}); ok {
		for _, c := range conditions {
			if cm, ok := c.(map[string]interface{}); ok {
				variable, _ := cm["variable"].(string)
				currentVal, _ := cm["current_value"].(float64)
				threshold, _ := cm["threshold"].(float64)
				operator, _ := cm["operator"].(string)
				if variable != "" {
					fields = append(fields, &SlackText{
						Type: "mrkdwn",
						Text: fmt.Sprintf("*%s*\n%.1f (%s %.1f)", variable, currentVal, operator, threshold),
					})
				}
			}
		}
	}

	return fields
}
