package email

import (
	"encoding/json"
	"fmt"
	"time"
)

// SNSNotification represents the top-level SNS message envelope.
type SNSNotification struct {
	Type      string `json:"Type"`
	MessageId string `json:"MessageId"`
	TopicArn  string `json:"TopicArn"`
	Message   string `json:"Message"` // JSON-encoded SES notification
	Timestamp string `json:"Timestamp"`
}

// SESNotification represents the SES event notification embedded in the SNS message.
type SESNotification struct {
	NotificationType string        `json:"notificationType"` // "Bounce" or "Complaint"
	Bounce           *SESBounce    `json:"bounce,omitempty"`
	Complaint        *SESComplaint `json:"complaint,omitempty"`
	Mail             SESMail       `json:"mail"`
}

// SESBounce represents the bounce details from SES.
type SESBounce struct {
	BounceType        string                `json:"bounceType"` // "Permanent" or "Transient"
	BounceSubType     string                `json:"bounceSubType"`
	BouncedRecipients []SESBouncedRecipient `json:"bouncedRecipients"`
	Timestamp         string                `json:"timestamp"`
}

// SESBouncedRecipient represents a single bounced recipient.
type SESBouncedRecipient struct {
	EmailAddress   string `json:"emailAddress"`
	Action         string `json:"action"`
	Status         string `json:"status"`
	DiagnosticCode string `json:"diagnosticCode"`
}

// SESComplaint represents complaint details from SES.
type SESComplaint struct {
	ComplainedRecipients  []SESComplainedRecipient `json:"complainedRecipients"`
	ComplaintFeedbackType string                   `json:"complaintFeedbackType"` // e.g., "abuse"
	Timestamp             string                   `json:"timestamp"`
}

// SESComplainedRecipient represents a single complaint recipient.
type SESComplainedRecipient struct {
	EmailAddress string `json:"emailAddress"`
}

// SESMail represents the original mail metadata from SES.
type SESMail struct {
	MessageId string `json:"messageId"` // The SES message ID
}

// ParseSNSBounceEvent parses an SNS message body containing an SES bounce or
// complaint notification and converts it into BounceEvent structs for the
// BounceProcessor.
//
// It handles both "Bounce" and "Complaint" notification types.
// For bounces, only "Permanent" bounce types are converted (transient bounces
// are ignored as SES handles retries for those).
//
// Returns an empty slice (not an error) for:
//   - Transient bounces (SES retries these automatically)
//   - Unknown notification types
func ParseSNSBounceEvent(snsBody []byte) ([]BounceEvent, error) {
	if len(snsBody) == 0 {
		return nil, fmt.Errorf("sns feedback: empty SNS body")
	}

	// 1. Parse the SNS envelope.
	var snsMsg SNSNotification
	if err := json.Unmarshal(snsBody, &snsMsg); err != nil {
		return nil, fmt.Errorf("sns feedback: failed to parse SNS envelope: %w", err)
	}

	// 2. Parse the SES notification from the Message field.
	if snsMsg.Message == "" {
		return nil, fmt.Errorf("sns feedback: SNS Message field is empty")
	}

	var sesNotif SESNotification
	if err := json.Unmarshal([]byte(snsMsg.Message), &sesNotif); err != nil {
		return nil, fmt.Errorf("sns feedback: failed to parse SES notification from SNS Message: %w", err)
	}

	// 3. Convert to BounceEvent(s) based on notification type.
	switch sesNotif.NotificationType {
	case "Bounce":
		return parseBounceEvents(sesNotif)
	case "Complaint":
		return parseComplaintEvents(sesNotif)
	default:
		// Unknown notification type -- return empty slice, not an error.
		return nil, nil
	}
}

// parseBounceEvents converts an SES bounce notification into BounceEvent structs.
// Only "Permanent" bounces are converted; transient bounces return an empty slice
// because SES automatically retries delivery for those.
func parseBounceEvents(sesNotif SESNotification) ([]BounceEvent, error) {
	if sesNotif.Bounce == nil {
		return nil, fmt.Errorf("sns feedback: bounce notification missing bounce details")
	}

	// Transient bounces are retried by SES -- ignore them.
	if sesNotif.Bounce.BounceType != "Permanent" {
		return nil, nil
	}

	// Parse the bounce timestamp.
	ts, err := parseTimestamp(sesNotif.Bounce.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("sns feedback: failed to parse bounce timestamp: %w", err)
	}

	events := make([]BounceEvent, 0, len(sesNotif.Bounce.BouncedRecipients))
	for _, recipient := range sesNotif.Bounce.BouncedRecipients {
		reason := recipient.DiagnosticCode
		if reason == "" {
			reason = fmt.Sprintf("%s (%s)", sesNotif.Bounce.BounceSubType, recipient.Status)
		}

		events = append(events, BounceEvent{
			ProviderMessageID: sesNotif.Mail.MessageId,
			EmailAddress:      recipient.EmailAddress,
			Reason:            reason,
			Type:              FeedbackBounce,
			Timestamp:         ts,
		})
	}

	return events, nil
}

// parseComplaintEvents converts an SES complaint notification into BounceEvent structs.
// Each complained recipient produces one BounceEvent with Type=FeedbackComplaint.
func parseComplaintEvents(sesNotif SESNotification) ([]BounceEvent, error) {
	if sesNotif.Complaint == nil {
		return nil, fmt.Errorf("sns feedback: complaint notification missing complaint details")
	}

	// Parse the complaint timestamp.
	ts, err := parseTimestamp(sesNotif.Complaint.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("sns feedback: failed to parse complaint timestamp: %w", err)
	}

	events := make([]BounceEvent, 0, len(sesNotif.Complaint.ComplainedRecipients))
	for _, recipient := range sesNotif.Complaint.ComplainedRecipients {
		reason := sesNotif.Complaint.ComplaintFeedbackType
		if reason == "" {
			reason = "complaint"
		}

		events = append(events, BounceEvent{
			ProviderMessageID: sesNotif.Mail.MessageId,
			EmailAddress:      recipient.EmailAddress,
			Reason:            reason,
			Type:              FeedbackComplaint,
			Timestamp:         ts,
		})
	}

	return events, nil
}

// parseTimestamp attempts to parse a timestamp string in RFC3339 format,
// falling back to the current time if parsing fails. SES timestamps are
// typically in ISO 8601 / RFC3339 format.
func parseTimestamp(raw string) (time.Time, error) {
	if raw == "" {
		return time.Now().UTC(), nil
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// SES sometimes uses a format without the 'Z' timezone marker.
		// Try a few common layouts before giving up.
		t, err = time.Parse("2006-01-02T15:04:05.000Z", raw)
		if err != nil {
			return time.Now().UTC(), nil
		}
	}

	return t, nil
}
