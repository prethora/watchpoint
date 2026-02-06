package webhook

import (
	"context"
	"encoding/json"
	"fmt"

	"watchpoint/internal/types"
)

// GoogleChatFormatter formats notifications as Google Chat Cards v2 JSON.
//
// Architecture reference: 08c-webhook-worker.md Section 4.4
type GoogleChatFormatter struct{}

// Platform returns the platform identifier.
func (f *GoogleChatFormatter) Platform() Platform {
	return PlatformGoogleChat
}

// Format transforms a Notification into Google Chat Cards v2 JSON.
func (f *GoogleChatFormatter) Format(_ context.Context, n *types.Notification, _ map[string]any) ([]byte, error) {
	if n == nil {
		return nil, fmt.Errorf("google chat formatter: notification is nil")
	}

	title := formatTitle(n)
	subtitle := fmt.Sprintf("Urgency: %s | Event: %s", string(n.Urgency), string(n.EventType))

	// Build widgets from notification data.
	widgets := buildGoogleChatWidgets(n)

	card := GoogleCard{
		Header: GoogleHeader{
			Title:    title,
			Subtitle: subtitle,
		},
		Sections: []GoogleSection{
			{
				Widgets: widgets,
			},
		},
	}

	payload := GoogleChatPayload{
		Cards: []GoogleCard{card},
	}

	return json.Marshal(payload)
}

// ValidateResponse checks the Google Chat webhook response.
func (f *GoogleChatFormatter) ValidateResponse(statusCode int, body []byte) error {
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err == nil {
		if errObj, ok := resp["error"].(map[string]interface{}); ok {
			if msg, ok := errObj["message"].(string); ok {
				return fmt.Errorf("google chat: API error: %s", msg)
			}
		}
	}

	return fmt.Errorf("google chat: unexpected status %d: %s", statusCode, truncateBody(body))
}

// buildGoogleChatWidgets creates card widgets from notification data.
func buildGoogleChatWidgets(n *types.Notification) []GoogleWidget {
	var widgets []GoogleWidget

	if n.Payload == nil {
		return widgets
	}

	if name, ok := n.Payload["watchpoint_name"].(string); ok && name != "" {
		widgets = append(widgets, GoogleWidget{
			KeyValue: &GoogleKeyValue{
				TopLabel: "WatchPoint",
				Content:  name,
			},
		})
	}

	if loc, ok := n.Payload["location"].(map[string]interface{}); ok {
		if label, ok := loc["label"].(string); ok && label != "" {
			widgets = append(widgets, GoogleWidget{
				KeyValue: &GoogleKeyValue{
					TopLabel: "Location",
					Content:  label,
				},
			})
		}
	}

	// Add condition details.
	if conditions, ok := n.Payload["conditions"].([]interface{}); ok {
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
				widgets = append(widgets, GoogleWidget{
					KeyValue: &GoogleKeyValue{
						TopLabel: variable,
						Content:  fmt.Sprintf("%.1f (%s %.1f)", currentVal, operator, threshold),
					},
				})
			}
		}
	}

	// If no data widgets were added, provide a basic text description.
	if len(widgets) == 0 {
		widgets = append(widgets, GoogleWidget{
			TextParagraph: &GoogleTextParagraph{
				Text: fmt.Sprintf("<b>%s</b>: %s",
					capitalizeFirst(string(n.EventType)),
					string(n.Urgency)),
			},
		})
	}

	return widgets
}
