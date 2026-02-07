package email

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// mockEmailProvider implements external.EmailProvider for testing.
type mockEmailProvider struct {
	sendCalled bool
	sendInput  types.SendInput
	sendMsgID  string
	sendErr    error
}

func (m *mockEmailProvider) Send(ctx context.Context, input types.SendInput) (string, error) {
	m.sendCalled = true
	m.sendInput = input
	if m.sendErr != nil {
		return "", m.sendErr
	}
	return m.sendMsgID, nil
}

// mockTemplateService implements TemplateService for testing.
type mockTemplateService struct {
	rendered  *RenderedEmail
	sender    types.SenderIdentity
	renderErr error
}

func (m *mockTemplateService) Render(set string, eventType types.EventType, n *types.Notification) (*RenderedEmail, types.SenderIdentity, error) {
	if m.renderErr != nil {
		return nil, types.SenderIdentity{}, m.renderErr
	}
	rendered := m.rendered
	if rendered == nil {
		rendered = &RenderedEmail{
			Subject:  "Test Subject",
			BodyHTML: "<p>Test</p>",
			BodyText: "Test",
		}
	}
	return rendered, m.sender, nil
}

// --- EmailChannel.Type Tests ---

func TestEmailChannelType(t *testing.T) {
	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  &mockEmailProvider{},
		Templates: &mockTemplateService{},
		Logger:    newTestLogger(),
	})

	if ch.Type() != types.ChannelEmail {
		t.Errorf("Type() = %v, want %v", ch.Type(), types.ChannelEmail)
	}
}

// --- EmailChannel.ValidateConfig Tests ---

func TestEmailChannelValidateConfig(t *testing.T) {
	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  &mockEmailProvider{},
		Templates: &mockTemplateService{},
		Logger:    newTestLogger(),
	})

	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{
			name:    "valid config",
			config:  map[string]any{"address": "user@example.com"},
			wantErr: false,
		},
		{
			name:    "missing address",
			config:  map[string]any{},
			wantErr: true,
		},
		{
			name:    "empty address",
			config:  map[string]any{"address": ""},
			wantErr: true,
		},
		{
			name:    "non-string address",
			config:  map[string]any{"address": 42},
			wantErr: true,
		},
		{
			name:    "nil config",
			config:  nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ch.ValidateConfig(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// --- EmailChannel.Format Tests ---

func TestEmailChannelFormat(t *testing.T) {
	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  &mockEmailProvider{},
		Templates: &mockTemplateService{},
		Logger:    newTestLogger(),
	})

	t.Run("format valid notification", func(t *testing.T) {
		n := &types.Notification{
			ID:        "notif-1",
			EventType: types.EventThresholdCrossed,
		}

		payload, err := ch.Format(context.Background(), n, nil)
		if err != nil {
			t.Fatalf("Format() error: %v", err)
		}

		// Should be valid JSON.
		var decoded types.Notification
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("Format() produced invalid JSON: %v", err)
		}

		if decoded.ID != "notif-1" {
			t.Errorf("decoded ID = %q, want notif-1", decoded.ID)
		}
	})

	t.Run("format nil notification", func(t *testing.T) {
		_, err := ch.Format(context.Background(), nil, nil)
		if err == nil {
			t.Error("expected error for nil notification")
		}
	})
}

// --- EmailChannel.Deliver Tests ---

func TestEmailChannelDeliver_Success(t *testing.T) {
	provider := &mockEmailProvider{sendMsgID: "msg-provider-123"}
	tmplService := &mockTemplateService{
		rendered: &RenderedEmail{
			Subject:  "Threshold Crossed: Downtown Sensor",
			BodyHTML: "<h1>Alert</h1><p>Temperature is 35.0</p>",
			BodyText: "Alert: Temperature is 35.0",
		},
		sender: types.SenderIdentity{
			Name:    "WatchPoint Alerts",
			Address: "alerts@watchpoint.io",
		},
	}

	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  provider,
		Templates: tmplService,
		Logger:    newTestLogger(),
	})

	n := &types.Notification{
		ID:          "notif-success",
		EventType:   types.EventThresholdCrossed,
		TemplateSet: "default",
		Payload:     map[string]interface{}{"temperature": 35.0},
	}
	payload, _ := json.Marshal(n)

	result, err := ch.Deliver(context.Background(), payload, "user@example.com")
	if err != nil {
		t.Fatalf("Deliver() error: %v", err)
	}

	if result.Status != types.DeliveryStatusSent {
		t.Errorf("Status = %v, want %v", result.Status, types.DeliveryStatusSent)
	}
	if result.ProviderMessageID != "msg-provider-123" {
		t.Errorf("ProviderMessageID = %q, want msg-provider-123", result.ProviderMessageID)
	}

	// Verify provider was called with correct inputs.
	if !provider.sendCalled {
		t.Error("provider.Send was not called")
	}
	if provider.sendInput.To != "user@example.com" {
		t.Errorf("provider To = %q, want user@example.com", provider.sendInput.To)
	}
	if provider.sendInput.Subject != "Threshold Crossed: Downtown Sensor" {
		t.Errorf("provider Subject = %q, want Threshold Crossed: Downtown Sensor", provider.sendInput.Subject)
	}
	if provider.sendInput.BodyHTML != "<h1>Alert</h1><p>Temperature is 35.0</p>" {
		t.Errorf("provider BodyHTML = %q", provider.sendInput.BodyHTML)
	}
	if provider.sendInput.ReferenceID != "notif-success" {
		t.Errorf("provider ReferenceID = %q, want notif-success", provider.sendInput.ReferenceID)
	}
	if provider.sendInput.From.Name != "WatchPoint Alerts" {
		t.Errorf("provider From.Name = %q, want WatchPoint Alerts", provider.sendInput.From.Name)
	}
	if provider.sendInput.From.Address != "alerts@watchpoint.io" {
		t.Errorf("provider From.Address = %q, want alerts@watchpoint.io", provider.sendInput.From.Address)
	}
}

func TestEmailChannelDeliver_TestMode(t *testing.T) {
	provider := &mockEmailProvider{}
	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  provider,
		Templates: &mockTemplateService{},
		Logger:    newTestLogger(),
	})

	n := &types.Notification{
		ID:       "notif-test",
		TestMode: true,
	}
	payload, _ := json.Marshal(n)

	result, err := ch.Deliver(context.Background(), payload, "user@example.com")
	if err != nil {
		t.Fatalf("Deliver() error: %v", err)
	}

	if result.Status != types.DeliveryStatusSkipped {
		t.Errorf("Status = %v, want %v", result.Status, types.DeliveryStatusSkipped)
	}
	if result.ProviderMessageID != "test-simulated" {
		t.Errorf("ProviderMessageID = %q, want test-simulated", result.ProviderMessageID)
	}

	// Provider should NOT have been called.
	if provider.sendCalled {
		t.Error("provider.Send should not be called in test mode")
	}
}

func TestEmailChannelDeliver_BlocklistError_Sentinel(t *testing.T) {
	provider := &mockEmailProvider{sendErr: ErrRecipientBlocked}
	ch := NewEmailChannel(EmailChannelConfig{
		Provider: provider,
		Templates: &mockTemplateService{
			sender: types.SenderIdentity{
				Name:    "Test",
				Address: "test@test.com",
			},
		},
		Logger: newTestLogger(),
	})

	n := &types.Notification{
		ID:          "notif-blocked",
		EventType:   types.EventThresholdCrossed,
		TemplateSet: "default",
	}
	payload, _ := json.Marshal(n)

	result, err := ch.Deliver(context.Background(), payload, "blocked@example.com")
	if err != nil {
		t.Fatalf("Deliver() should not return error for blocklist, got: %v", err)
	}

	if result.Status != types.DeliveryStatusBounced {
		t.Errorf("Status = %v, want %v", result.Status, types.DeliveryStatusBounced)
	}
	if result.FailureReason != "address_blocked" {
		t.Errorf("FailureReason = %q, want address_blocked", result.FailureReason)
	}
	if result.Retryable {
		t.Error("blocklist result should not be retryable")
	}
}

func TestEmailChannelDeliver_BlocklistError_AppError(t *testing.T) {
	provider := &mockEmailProvider{
		sendErr: types.NewAppError(
			types.ErrCodeEmailBlocked,
			"provider blocked",
			nil,
		),
	}
	ch := NewEmailChannel(EmailChannelConfig{
		Provider: provider,
		Templates: &mockTemplateService{
			sender: types.SenderIdentity{
				Name:    "Test",
				Address: "test@test.com",
			},
		},
		Logger: newTestLogger(),
	})

	n := &types.Notification{
		ID:          "notif-blocked-app",
		EventType:   types.EventThresholdCrossed,
		TemplateSet: "default",
	}
	payload, _ := json.Marshal(n)

	result, err := ch.Deliver(context.Background(), payload, "blocked@example.com")
	if err != nil {
		t.Fatalf("Deliver() should not return error for blocklist, got: %v", err)
	}

	if result.Status != types.DeliveryStatusBounced {
		t.Errorf("Status = %v, want %v", result.Status, types.DeliveryStatusBounced)
	}
	if result.Retryable {
		t.Error("blocklist result should not be retryable")
	}
}

func TestEmailChannelDeliver_TransientError(t *testing.T) {
	provider := &mockEmailProvider{
		sendErr: types.NewAppError(
			types.ErrCodeUpstreamUnavailable,
			"provider 500",
			nil,
		),
	}
	ch := NewEmailChannel(EmailChannelConfig{
		Provider: provider,
		Templates: &mockTemplateService{
			sender: types.SenderIdentity{
				Name:    "Test",
				Address: "test@test.com",
			},
		},
		Logger: newTestLogger(),
	})

	n := &types.Notification{
		ID:          "notif-transient",
		EventType:   types.EventThresholdCrossed,
		TemplateSet: "default",
	}
	payload, _ := json.Marshal(n)

	result, err := ch.Deliver(context.Background(), payload, "user@example.com")
	if result != nil {
		t.Error("Deliver() should return nil result for transient errors")
	}
	if err == nil {
		t.Fatal("Deliver() should return error for transient failures")
	}
}

func TestEmailChannelDeliver_InvalidPayload(t *testing.T) {
	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  &mockEmailProvider{},
		Templates: &mockTemplateService{},
		Logger:    newTestLogger(),
	})

	_, err := ch.Deliver(context.Background(), []byte("not json"), "user@example.com")
	if err == nil {
		t.Error("expected error for invalid JSON payload")
	}
}

func TestEmailChannelDeliver_TemplateRenderFailure(t *testing.T) {
	ch := NewEmailChannel(EmailChannelConfig{
		Provider: &mockEmailProvider{},
		Templates: &mockTemplateService{
			renderErr: errors.New("template not found"),
		},
		Logger: newTestLogger(),
	})

	n := &types.Notification{
		ID:          "notif-no-tmpl",
		EventType:   types.EventBillingWarning,
		TemplateSet: "unknown",
	}
	payload, _ := json.Marshal(n)

	_, err := ch.Deliver(context.Background(), payload, "user@example.com")
	if err == nil {
		t.Error("expected error for template resolution failure")
	}
}

// --- EmailChannel.ShouldRetry Tests ---

func TestEmailChannelShouldRetry(t *testing.T) {
	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  &mockEmailProvider{},
		Templates: &mockTemplateService{},
		Logger:    newTestLogger(),
	})

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "sentinel blocklist",
			err:  ErrRecipientBlocked,
			want: false,
		},
		{
			name: "app error email blocked",
			err:  types.NewAppError(types.ErrCodeEmailBlocked, "blocked", nil),
			want: false,
		},
		{
			name: "rate limited - should retry",
			err:  types.NewAppError(types.ErrCodeUpstreamRateLimited, "429", nil),
			want: true,
		},
		{
			name: "upstream unavailable - should retry",
			err:  types.NewAppError(types.ErrCodeUpstreamUnavailable, "500", nil),
			want: true,
		},
		{
			name: "generic error - should retry",
			err:  errors.New("network timeout"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ch.ShouldRetry(tt.err)
			if got != tt.want {
				t.Errorf("ShouldRetry(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// --- Integration Test ---
// This test simulates the full flow: create a notification payload,
// deliver through EmailChannel, and verify the mock provider receives
// the correct inputs.

func TestEmailChannelIntegration_FullDeliveryFlow(t *testing.T) {
	// Setup: Real Renderer + Mock Provider.
	logger := newTestLogger()

	renderer, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}

	provider := &mockEmailProvider{sendMsgID: "msg-integration-001"}

	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  provider,
		Templates: renderer,
		Logger:    logger,
	})

	// Build a notification as the eval worker would.
	notification := &types.Notification{
		ID:             "notif-integration-1",
		WatchPointID:   "wp-int-123",
		OrganizationID: "org-int-456",
		EventType:      types.EventThresholdCrossed,
		Urgency:        types.UrgencyWarning,
		TemplateSet:    "default",
		Payload: map[string]interface{}{
			"timezone":                  "America/New_York",
			"temperature":               38.2,
			"precipitation_probability": 0.75,
			"location":                  "Central Park, NY",
			"watchpoint_name":           "Downtown Sensor",
		},
		CreatedAt: time.Date(2026, 2, 6, 15, 0, 0, 0, time.UTC),
	}

	// Simulate what the worker does: Format then Deliver.
	payload, err := ch.Format(context.Background(), notification, nil)
	if err != nil {
		t.Fatalf("Format() error: %v", err)
	}

	destination := "user@example.com"
	result, err := ch.Deliver(context.Background(), payload, destination)
	if err != nil {
		t.Fatalf("Deliver() error: %v", err)
	}

	// Verify delivery result.
	if result.Status != types.DeliveryStatusSent {
		t.Errorf("result.Status = %v, want %v", result.Status, types.DeliveryStatusSent)
	}
	if result.ProviderMessageID != "msg-integration-001" {
		t.Errorf("result.ProviderMessageID = %q, want msg-integration-001", result.ProviderMessageID)
	}

	// Verify provider received the correct payload.
	if !provider.sendCalled {
		t.Fatal("provider.Send was not called")
	}

	input := provider.sendInput
	if input.To != destination {
		t.Errorf("input.To = %q, want %q", input.To, destination)
	}
	if input.Subject == "" {
		t.Error("input.Subject should not be empty")
	}
	if input.BodyHTML == "" {
		t.Error("input.BodyHTML should not be empty")
	}
	if input.BodyText == "" {
		t.Error("input.BodyText should not be empty")
	}
	if input.ReferenceID != "notif-integration-1" {
		t.Errorf("input.ReferenceID = %q, want notif-integration-1", input.ReferenceID)
	}
	if input.From.Address != "alerts@watchpoint.io" {
		t.Errorf("input.From.Address = %q, want alerts@watchpoint.io", input.From.Address)
	}
	if input.From.Name != "WatchPoint Alerts" {
		t.Errorf("input.From.Name = %q, want WatchPoint Alerts", input.From.Name)
	}
}

func TestEmailChannelIntegration_CustomSetSenderName(t *testing.T) {
	// Test that a custom template set appends the set name to the sender.
	logger := newTestLogger()

	renderer, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}

	provider := &mockEmailProvider{sendMsgID: "msg-custom-001"}

	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  provider,
		Templates: renderer,
		Logger:    logger,
	})

	notification := &types.Notification{
		ID:          "notif-custom",
		EventType:   types.EventThresholdCrossed,
		TemplateSet: "wedding",
		Payload:     map[string]interface{}{},
	}

	payload, err := ch.Format(context.Background(), notification, nil)
	if err != nil {
		t.Fatalf("Format() error: %v", err)
	}

	result, err := ch.Deliver(context.Background(), payload, "user@example.com")
	if err != nil {
		t.Fatalf("Deliver() error: %v", err)
	}

	if result.Status != types.DeliveryStatusSent {
		t.Errorf("Status = %v, want %v", result.Status, types.DeliveryStatusSent)
	}

	// Sender should include the wedding set name.
	if provider.sendInput.From.Name != "WatchPoint Alerts (wedding)" {
		t.Errorf("From.Name = %q, want WatchPoint Alerts (wedding)", provider.sendInput.From.Name)
	}
}

func TestEmailChannelIntegration_TestModeBypass(t *testing.T) {
	// Verify that test mode notifications never reach the provider.
	provider := &mockEmailProvider{sendMsgID: "should-not-use"}

	renderer, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          newTestLogger(),
	})
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}

	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  provider,
		Templates: renderer,
		Logger:    newTestLogger(),
	})

	notification := &types.Notification{
		ID:          "notif-test-mode",
		EventType:   types.EventThresholdCrossed,
		TemplateSet: "default",
		TestMode:    true,
		Payload:     map[string]interface{}{},
	}

	payload, _ := ch.Format(context.Background(), notification, nil)

	result, err := ch.Deliver(context.Background(), payload, "user@example.com")
	if err != nil {
		t.Fatalf("Deliver() error: %v", err)
	}

	if result.Status != types.DeliveryStatusSkipped {
		t.Errorf("Status = %v, want %v", result.Status, types.DeliveryStatusSkipped)
	}

	if provider.sendCalled {
		t.Error("provider should not be called in test mode")
	}
}

func TestEmailChannelIntegration_BlockedRecipient(t *testing.T) {
	// Simulate a provider blocked delivery via AppError.
	provider := &mockEmailProvider{
		sendErr: types.NewAppError(
			types.ErrCodeEmailBlocked,
			"provider blocked delivery",
			nil,
		),
	}

	renderer, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          newTestLogger(),
	})
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}

	ch := NewEmailChannel(EmailChannelConfig{
		Provider:  provider,
		Templates: renderer,
		Logger:    newTestLogger(),
	})

	notification := &types.Notification{
		ID:          "notif-blocked",
		EventType:   types.EventThresholdCrossed,
		TemplateSet: "default",
		Payload:     map[string]interface{}{},
	}

	payload, _ := ch.Format(context.Background(), notification, nil)

	result, err := ch.Deliver(context.Background(), payload, "blocked@example.com")
	if err != nil {
		t.Fatalf("Deliver() should not return error for blocklist, got: %v", err)
	}

	if result.Status != types.DeliveryStatusBounced {
		t.Errorf("Status = %v, want %v", result.Status, types.DeliveryStatusBounced)
	}
	if result.FailureReason != "address_blocked" {
		t.Errorf("FailureReason = %q, want address_blocked", result.FailureReason)
	}
	if result.Retryable {
		t.Error("blocked result should not be retryable")
	}

	// Verify ShouldRetry also returns false for this error type.
	if ch.ShouldRetry(provider.sendErr) {
		t.Error("ShouldRetry should return false for blocklist errors")
	}
}
