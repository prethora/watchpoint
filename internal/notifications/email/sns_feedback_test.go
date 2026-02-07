package email

import (
	"encoding/json"
	"testing"
	"time"
)

// buildSNSMessage creates a realistic SNS notification JSON body wrapping an
// SES notification. This mirrors the actual format AWS SNS delivers.
func buildSNSMessage(t *testing.T, sesNotif SESNotification) []byte {
	t.Helper()

	sesJSON, err := json.Marshal(sesNotif)
	if err != nil {
		t.Fatalf("failed to marshal SES notification: %v", err)
	}

	snsMsg := SNSNotification{
		Type:      "Notification",
		MessageId: "sns-msg-001",
		TopicArn:  "arn:aws:sns:us-east-1:123456789012:ses-feedback",
		Message:   string(sesJSON),
		Timestamp: "2026-02-07T12:00:00.000Z",
	}

	snsJSON, err := json.Marshal(snsMsg)
	if err != nil {
		t.Fatalf("failed to marshal SNS notification: %v", err)
	}

	return snsJSON
}

func TestParseSNSBounceEvent_PermanentBounce_SingleRecipient(t *testing.T) {
	snsBody := buildSNSMessage(t, SESNotification{
		NotificationType: "Bounce",
		Bounce: &SESBounce{
			BounceType:    "Permanent",
			BounceSubType: "General",
			BouncedRecipients: []SESBouncedRecipient{
				{
					EmailAddress:   "bad@example.com",
					Action:         "failed",
					Status:         "5.1.1",
					DiagnosticCode: "smtp; 550 5.1.1 The email account that you tried to reach does not exist",
				},
			},
			Timestamp: "2026-02-07T10:30:00Z",
		},
		Mail: SESMail{
			MessageId: "ses-msg-aaa-111",
		},
	})

	events, err := ParseSNSBounceEvent(snsBody)
	if err != nil {
		t.Fatalf("ParseSNSBounceEvent() error: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	if ev.ProviderMessageID != "ses-msg-aaa-111" {
		t.Errorf("ProviderMessageID = %q, want ses-msg-aaa-111", ev.ProviderMessageID)
	}
	if ev.EmailAddress != "bad@example.com" {
		t.Errorf("EmailAddress = %q, want bad@example.com", ev.EmailAddress)
	}
	if ev.Type != FeedbackBounce {
		t.Errorf("Type = %q, want %q", ev.Type, FeedbackBounce)
	}
	if ev.Reason != "smtp; 550 5.1.1 The email account that you tried to reach does not exist" {
		t.Errorf("Reason = %q, want diagnostic code", ev.Reason)
	}

	expectedTime, _ := time.Parse(time.RFC3339, "2026-02-07T10:30:00Z")
	if !ev.Timestamp.Equal(expectedTime) {
		t.Errorf("Timestamp = %v, want %v", ev.Timestamp, expectedTime)
	}
}

func TestParseSNSBounceEvent_PermanentBounce_MultipleRecipients(t *testing.T) {
	snsBody := buildSNSMessage(t, SESNotification{
		NotificationType: "Bounce",
		Bounce: &SESBounce{
			BounceType:    "Permanent",
			BounceSubType: "General",
			BouncedRecipients: []SESBouncedRecipient{
				{
					EmailAddress:   "user1@example.com",
					Action:         "failed",
					Status:         "5.1.1",
					DiagnosticCode: "smtp; 550 user unknown",
				},
				{
					EmailAddress:   "user2@example.com",
					Action:         "failed",
					Status:         "5.1.1",
					DiagnosticCode: "smtp; 550 mailbox not found",
				},
				{
					EmailAddress:   "user3@example.com",
					Action:         "failed",
					Status:         "5.2.1",
					DiagnosticCode: "", // Empty diagnostic -- should fall back
				},
			},
			Timestamp: "2026-02-07T11:00:00Z",
		},
		Mail: SESMail{
			MessageId: "ses-msg-bbb-222",
		},
	})

	events, err := ParseSNSBounceEvent(snsBody)
	if err != nil {
		t.Fatalf("ParseSNSBounceEvent() error: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// All events should share the same provider message ID.
	for i, ev := range events {
		if ev.ProviderMessageID != "ses-msg-bbb-222" {
			t.Errorf("event[%d].ProviderMessageID = %q, want ses-msg-bbb-222", i, ev.ProviderMessageID)
		}
		if ev.Type != FeedbackBounce {
			t.Errorf("event[%d].Type = %q, want %q", i, ev.Type, FeedbackBounce)
		}
	}

	// Check individual email addresses.
	wantEmails := []string{"user1@example.com", "user2@example.com", "user3@example.com"}
	for i, want := range wantEmails {
		if events[i].EmailAddress != want {
			t.Errorf("event[%d].EmailAddress = %q, want %q", i, events[i].EmailAddress, want)
		}
	}

	// Third recipient has empty DiagnosticCode -- should fall back to SubType (Status).
	if events[2].Reason != "General (5.2.1)" {
		t.Errorf("event[2].Reason = %q, want fallback 'General (5.2.1)'", events[2].Reason)
	}
}

func TestParseSNSBounceEvent_Complaint_SingleRecipient(t *testing.T) {
	snsBody := buildSNSMessage(t, SESNotification{
		NotificationType: "Complaint",
		Complaint: &SESComplaint{
			ComplainedRecipients: []SESComplainedRecipient{
				{EmailAddress: "complainer@example.com"},
			},
			ComplaintFeedbackType: "abuse",
			Timestamp:             "2026-02-07T09:00:00Z",
		},
		Mail: SESMail{
			MessageId: "ses-msg-ccc-333",
		},
	})

	events, err := ParseSNSBounceEvent(snsBody)
	if err != nil {
		t.Fatalf("ParseSNSBounceEvent() error: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	if ev.ProviderMessageID != "ses-msg-ccc-333" {
		t.Errorf("ProviderMessageID = %q, want ses-msg-ccc-333", ev.ProviderMessageID)
	}
	if ev.EmailAddress != "complainer@example.com" {
		t.Errorf("EmailAddress = %q, want complainer@example.com", ev.EmailAddress)
	}
	if ev.Type != FeedbackComplaint {
		t.Errorf("Type = %q, want %q", ev.Type, FeedbackComplaint)
	}
	if ev.Reason != "abuse" {
		t.Errorf("Reason = %q, want 'abuse'", ev.Reason)
	}

	expectedTime, _ := time.Parse(time.RFC3339, "2026-02-07T09:00:00Z")
	if !ev.Timestamp.Equal(expectedTime) {
		t.Errorf("Timestamp = %v, want %v", ev.Timestamp, expectedTime)
	}
}

func TestParseSNSBounceEvent_TransientBounce_ReturnsEmpty(t *testing.T) {
	snsBody := buildSNSMessage(t, SESNotification{
		NotificationType: "Bounce",
		Bounce: &SESBounce{
			BounceType:    "Transient",
			BounceSubType: "MailboxFull",
			BouncedRecipients: []SESBouncedRecipient{
				{
					EmailAddress:   "full@example.com",
					Action:         "delayed",
					Status:         "4.2.2",
					DiagnosticCode: "smtp; 452 mailbox full",
				},
			},
			Timestamp: "2026-02-07T08:00:00Z",
		},
		Mail: SESMail{
			MessageId: "ses-msg-ddd-444",
		},
	})

	events, err := ParseSNSBounceEvent(snsBody)
	if err != nil {
		t.Fatalf("ParseSNSBounceEvent() error: %v, want nil", err)
	}

	if len(events) != 0 {
		t.Errorf("expected 0 events for transient bounce, got %d", len(events))
	}
}

func TestParseSNSBounceEvent_UnknownNotificationType_ReturnsEmpty(t *testing.T) {
	snsBody := buildSNSMessage(t, SESNotification{
		NotificationType: "Delivery",
		Mail: SESMail{
			MessageId: "ses-msg-eee-555",
		},
	})

	events, err := ParseSNSBounceEvent(snsBody)
	if err != nil {
		t.Fatalf("ParseSNSBounceEvent() error: %v, want nil", err)
	}

	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown notification type, got %d", len(events))
	}
}

func TestParseSNSBounceEvent_MalformedSNSJSON_ReturnsError(t *testing.T) {
	_, err := ParseSNSBounceEvent([]byte(`{not valid json`))
	if err == nil {
		t.Fatal("expected error for malformed SNS JSON, got nil")
	}
}

func TestParseSNSBounceEvent_MalformedSESMessage_ReturnsError(t *testing.T) {
	// Valid SNS envelope but the Message field contains invalid JSON.
	snsMsg := SNSNotification{
		Type:      "Notification",
		MessageId: "sns-msg-bad",
		TopicArn:  "arn:aws:sns:us-east-1:123456789012:ses-feedback",
		Message:   `{this is not valid json}`,
		Timestamp: "2026-02-07T12:00:00.000Z",
	}

	snsJSON, err := json.Marshal(snsMsg)
	if err != nil {
		t.Fatalf("failed to marshal SNS envelope: %v", err)
	}

	_, err = ParseSNSBounceEvent(snsJSON)
	if err == nil {
		t.Fatal("expected error for malformed SES message JSON, got nil")
	}
}

func TestParseSNSBounceEvent_EmptySNSBody_ReturnsError(t *testing.T) {
	_, err := ParseSNSBounceEvent([]byte{})
	if err == nil {
		t.Fatal("expected error for empty SNS body, got nil")
	}

	_, err = ParseSNSBounceEvent(nil)
	if err == nil {
		t.Fatal("expected error for nil SNS body, got nil")
	}
}

func TestParseSNSBounceEvent_EmptyMessageField_ReturnsError(t *testing.T) {
	// SNS envelope with empty Message field.
	snsMsg := SNSNotification{
		Type:      "Notification",
		MessageId: "sns-msg-empty",
		TopicArn:  "arn:aws:sns:us-east-1:123456789012:ses-feedback",
		Message:   "",
		Timestamp: "2026-02-07T12:00:00.000Z",
	}

	snsJSON, err := json.Marshal(snsMsg)
	if err != nil {
		t.Fatalf("failed to marshal SNS envelope: %v", err)
	}

	_, err = ParseSNSBounceEvent(snsJSON)
	if err == nil {
		t.Fatal("expected error for empty Message field, got nil")
	}
}

func TestParseSNSBounceEvent_Complaint_EmptyFeedbackType_DefaultsToComplaint(t *testing.T) {
	// When ComplaintFeedbackType is empty, reason should default to "complaint".
	snsBody := buildSNSMessage(t, SESNotification{
		NotificationType: "Complaint",
		Complaint: &SESComplaint{
			ComplainedRecipients: []SESComplainedRecipient{
				{EmailAddress: "user@example.com"},
			},
			ComplaintFeedbackType: "",
			Timestamp:             "2026-02-07T09:00:00Z",
		},
		Mail: SESMail{
			MessageId: "ses-msg-fff-666",
		},
	})

	events, err := ParseSNSBounceEvent(snsBody)
	if err != nil {
		t.Fatalf("ParseSNSBounceEvent() error: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	if events[0].Reason != "complaint" {
		t.Errorf("Reason = %q, want 'complaint' as default", events[0].Reason)
	}
}

func TestParseSNSBounceEvent_Bounce_MissingBounceDetails_ReturnsError(t *testing.T) {
	// Bounce notification type but no Bounce struct.
	snsBody := buildSNSMessage(t, SESNotification{
		NotificationType: "Bounce",
		Bounce:           nil,
		Mail: SESMail{
			MessageId: "ses-msg-ggg-777",
		},
	})

	_, err := ParseSNSBounceEvent(snsBody)
	if err == nil {
		t.Fatal("expected error when bounce details are nil, got nil")
	}
}

func TestParseSNSBounceEvent_Complaint_MissingComplaintDetails_ReturnsError(t *testing.T) {
	// Complaint notification type but no Complaint struct.
	snsBody := buildSNSMessage(t, SESNotification{
		NotificationType: "Complaint",
		Complaint:        nil,
		Mail: SESMail{
			MessageId: "ses-msg-hhh-888",
		},
	})

	_, err := ParseSNSBounceEvent(snsBody)
	if err == nil {
		t.Fatal("expected error when complaint details are nil, got nil")
	}
}
