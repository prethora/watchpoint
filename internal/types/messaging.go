package types

// NotificationMessage represents the SQS payload sent from the Python Evaluation
// Worker to the Go Notification Workers. This struct is the transport envelope
// that carries all information needed for notification routing, policy evaluation,
// and delivery. JSON tags use snake_case to match the Python Pydantic model.
type NotificationMessage struct {
	// Core Identity
	NotificationID string `json:"notification_id"`
	WatchPointID   string `json:"watchpoint_id"`
	OrganizationID string `json:"organization_id"`

	// Routing & Logic
	EventType EventType    `json:"event_type"`
	Urgency   UrgencyLevel `json:"urgency"`
	TestMode  bool         `json:"test_mode"`

	// Ordering Metadata (Crucial for client-side sorting of asynchronous webhooks)
	Ordering OrderingMetadata `json:"ordering"`

	// Retry State: Carries retry count across the SQS Publish-Subscribe cycle.
	// Incremented by workers on transient failures before re-publishing.
	RetryCount int `json:"retry_count"`

	// Observability
	TraceID string `json:"trace_id"` // For X-Ray context propagation

	// Raw data snapshot for templating. Contains the notification payload
	// data that workers use to render templates and format channel-specific content.
	Payload map[string]interface{} `json:"payload"`
}
