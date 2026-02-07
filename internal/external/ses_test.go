package external

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"watchpoint/internal/types"
)

// mockSESAPI implements SESAPI for testing.
type mockSESAPI struct {
	sendEmailFunc func(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

func (m *mockSESAPI) SendEmail(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	return m.sendEmailFunc(ctx, params, optFns...)
}

// ---------------------------------------------------------------------------
// Send Tests - Success Path
// ---------------------------------------------------------------------------

func TestSESSend_Success(t *testing.T) {
	var capturedInput *sesv2.SendEmailInput

	mock := &mockSESAPI{
		sendEmailFunc: func(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
			capturedInput = params
			return &sesv2.SendEmailOutput{
				MessageId: aws.String("ses-msg-abc123"),
			}, nil
		},
	}

	client := NewSESClientWithAPI(mock, SESClientConfig{
		ConfigSetName: "watchpoint-tracking",
	})

	input := types.SendInput{
		To: "recipient@example.com",
		From: types.SenderIdentity{
			Name:    "WatchPoint Alerts",
			Address: "alerts@watchpoint.io",
		},
		Subject:     "Threshold Crossed: Downtown Sensor",
		BodyHTML:     "<h1>Alert</h1>",
		BodyText:     "Alert: threshold crossed",
		ReferenceID: "notif_001",
	}

	msgID, err := client.Send(context.Background(), input)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if msgID != "ses-msg-abc123" {
		t.Errorf("expected message ID ses-msg-abc123, got %s", msgID)
	}

	// Verify from address format.
	wantFrom := "WatchPoint Alerts <alerts@watchpoint.io>"
	if aws.ToString(capturedInput.FromEmailAddress) != wantFrom {
		t.Errorf("from = %q, want %q", aws.ToString(capturedInput.FromEmailAddress), wantFrom)
	}

	// Verify destination.
	if len(capturedInput.Destination.ToAddresses) != 1 || capturedInput.Destination.ToAddresses[0] != "recipient@example.com" {
		t.Errorf("unexpected destination: %v", capturedInput.Destination.ToAddresses)
	}

	// Verify subject.
	if aws.ToString(capturedInput.Content.Simple.Subject.Data) != "Threshold Crossed: Downtown Sensor" {
		t.Errorf("subject = %q", aws.ToString(capturedInput.Content.Simple.Subject.Data))
	}

	// Verify HTML body.
	if aws.ToString(capturedInput.Content.Simple.Body.Html.Data) != "<h1>Alert</h1>" {
		t.Errorf("html body = %q", aws.ToString(capturedInput.Content.Simple.Body.Html.Data))
	}

	// Verify text body.
	if aws.ToString(capturedInput.Content.Simple.Body.Text.Data) != "Alert: threshold crossed" {
		t.Errorf("text body = %q", aws.ToString(capturedInput.Content.Simple.Body.Text.Data))
	}

	// Verify configuration set.
	if aws.ToString(capturedInput.ConfigurationSetName) != "watchpoint-tracking" {
		t.Errorf("config set = %q, want watchpoint-tracking", aws.ToString(capturedInput.ConfigurationSetName))
	}

	// Verify tags.
	if len(capturedInput.EmailTags) != 1 {
		t.Fatalf("expected 1 email tag, got %d", len(capturedInput.EmailTags))
	}
	if aws.ToString(capturedInput.EmailTags[0].Name) != "ReferenceID" {
		t.Errorf("tag name = %q", aws.ToString(capturedInput.EmailTags[0].Name))
	}
	if aws.ToString(capturedInput.EmailTags[0].Value) != "notif_001" {
		t.Errorf("tag value = %q", aws.ToString(capturedInput.EmailTags[0].Value))
	}
}

func TestSESSend_SuccessNoFromName(t *testing.T) {
	var capturedInput *sesv2.SendEmailInput

	mock := &mockSESAPI{
		sendEmailFunc: func(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
			capturedInput = params
			return &sesv2.SendEmailOutput{MessageId: aws.String("ses-msg-noname")}, nil
		},
	}

	client := NewSESClientWithAPI(mock, SESClientConfig{})

	_, err := client.Send(context.Background(), types.SendInput{
		To:      "recipient@example.com",
		From:    types.SenderIdentity{Address: "alerts@watchpoint.io"},
		Subject: "Test",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// When name is empty, from should be just the address.
	if aws.ToString(capturedInput.FromEmailAddress) != "alerts@watchpoint.io" {
		t.Errorf("from = %q, want bare address", aws.ToString(capturedInput.FromEmailAddress))
	}
}

func TestSESSend_NilBodyFields(t *testing.T) {
	var capturedInput *sesv2.SendEmailInput

	mock := &mockSESAPI{
		sendEmailFunc: func(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
			capturedInput = params
			return &sesv2.SendEmailOutput{MessageId: aws.String("ses-msg-nohtml")}, nil
		},
	}

	client := NewSESClientWithAPI(mock, SESClientConfig{})

	_, err := client.Send(context.Background(), types.SendInput{
		To:      "recipient@example.com",
		From:    types.SenderIdentity{Address: "alerts@watchpoint.io"},
		Subject: "Test",
		// No BodyHTML or BodyText
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if capturedInput.Content.Simple.Body.Html != nil {
		t.Error("expected nil HTML body when not provided")
	}
	if capturedInput.Content.Simple.Body.Text != nil {
		t.Error("expected nil text body when not provided")
	}
}

func TestSESSend_NoReferenceID(t *testing.T) {
	var capturedInput *sesv2.SendEmailInput

	mock := &mockSESAPI{
		sendEmailFunc: func(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
			capturedInput = params
			return &sesv2.SendEmailOutput{MessageId: aws.String("ses-msg-noref")}, nil
		},
	}

	client := NewSESClientWithAPI(mock, SESClientConfig{})

	_, err := client.Send(context.Background(), types.SendInput{
		To:      "recipient@example.com",
		From:    types.SenderIdentity{Address: "alerts@watchpoint.io"},
		Subject: "Test",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(capturedInput.EmailTags) != 0 {
		t.Errorf("expected no email tags when no reference ID, got %d", len(capturedInput.EmailTags))
	}
}

// ---------------------------------------------------------------------------
// Send Tests - Error Paths
// ---------------------------------------------------------------------------

func TestSESSend_MessageRejected(t *testing.T) {
	mock := &mockSESAPI{
		sendEmailFunc: func(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
			return nil, &sestypes.MessageRejected{Message: aws.String("Email address is on the suppression list")}
		},
	}

	client := NewSESClientWithAPI(mock, SESClientConfig{})

	_, err := client.Send(context.Background(), types.SendInput{
		To:      "blocked@example.com",
		From:    types.SenderIdentity{Address: "alerts@watchpoint.io"},
		Subject: "Test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeEmailBlocked {
		t.Errorf("expected %s, got %s", types.ErrCodeEmailBlocked, appErr.Code)
	}
}

func TestSESSend_TooManyRequests(t *testing.T) {
	mock := &mockSESAPI{
		sendEmailFunc: func(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
			return nil, &sestypes.TooManyRequestsException{Message: aws.String("Rate exceeded")}
		},
	}

	client := NewSESClientWithAPI(mock, SESClientConfig{})

	_, err := client.Send(context.Background(), types.SendInput{
		To:      "recipient@example.com",
		From:    types.SenderIdentity{Address: "alerts@watchpoint.io"},
		Subject: "Test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeUpstreamRateLimited {
		t.Errorf("expected %s, got %s", types.ErrCodeUpstreamRateLimited, appErr.Code)
	}
}

func TestSESSend_AccountSendingPaused(t *testing.T) {
	mock := &mockSESAPI{
		sendEmailFunc: func(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
			return nil, &sestypes.SendingPausedException{Message: aws.String("Account paused")}
		},
	}

	client := NewSESClientWithAPI(mock, SESClientConfig{})

	_, err := client.Send(context.Background(), types.SendInput{
		To:      "recipient@example.com",
		From:    types.SenderIdentity{Address: "alerts@watchpoint.io"},
		Subject: "Test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeUpstreamUnavailable {
		t.Errorf("expected %s, got %s", types.ErrCodeUpstreamUnavailable, appErr.Code)
	}
}

func TestSESSend_GenericError(t *testing.T) {
	mock := &mockSESAPI{
		sendEmailFunc: func(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
			return nil, fmt.Errorf("network timeout")
		},
	}

	client := NewSESClientWithAPI(mock, SESClientConfig{})

	_, err := client.Send(context.Background(), types.SendInput{
		To:      "recipient@example.com",
		From:    types.SenderIdentity{Address: "alerts@watchpoint.io"},
		Subject: "Test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeUpstreamEmailProvider {
		t.Errorf("expected %s, got %s", types.ErrCodeUpstreamEmailProvider, appErr.Code)
	}
}

// ---------------------------------------------------------------------------
// Interface Compliance
// ---------------------------------------------------------------------------

var _ EmailProvider = (*SESClient)(nil)
