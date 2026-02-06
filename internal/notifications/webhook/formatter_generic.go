package webhook

import (
	"context"
	"encoding/json"
	"fmt"

	"watchpoint/internal/types"
)

// GenericFormatter outputs the strict notification payload structure as-is,
// ensuring downstream consumers receive a stable contract.
//
// This is the default formatter for webhook URLs that do not match any
// known platform pattern. It serializes the notification as a standardized
// JSON envelope.
//
// Architecture reference: 08c-webhook-worker.md Section 4.5
type GenericFormatter struct{}

// Platform returns the platform identifier.
func (f *GenericFormatter) Platform() Platform {
	return PlatformGeneric
}

// GenericPayload is the standard webhook payload envelope for generic endpoints.
type GenericPayload struct {
	EventType      string                 `json:"event_type"`
	WatchPointID   string                 `json:"watchpoint_id"`
	OrganizationID string                 `json:"organization_id"`
	NotificationID string                 `json:"notification_id"`
	Urgency        string                 `json:"urgency"`
	TestMode       bool                   `json:"test_mode"`
	Payload        map[string]interface{} `json:"payload"`
}

// Format transforms a Notification into generic JSON.
func (f *GenericFormatter) Format(_ context.Context, n *types.Notification, _ map[string]any) ([]byte, error) {
	if n == nil {
		return nil, fmt.Errorf("generic formatter: notification is nil")
	}

	payload := GenericPayload{
		EventType:      string(n.EventType),
		WatchPointID:   n.WatchPointID,
		OrganizationID: n.OrganizationID,
		NotificationID: n.ID,
		Urgency:        string(n.Urgency),
		TestMode:       n.TestMode,
		Payload:        n.Payload,
	}

	return json.Marshal(payload)
}

// ValidateResponse for generic webhooks simply checks the HTTP status code.
// Generic webhooks have no platform-specific "soft failure" patterns.
func (f *GenericFormatter) ValidateResponse(statusCode int, body []byte) error {
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}
	return fmt.Errorf("generic webhook: unexpected status %d: %s", statusCode, truncateBody(body))
}
