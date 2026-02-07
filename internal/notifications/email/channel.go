package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"watchpoint/internal/external"
	"watchpoint/internal/types"
)

// EmailChannel implements the types.NotificationChannel interface for email
// delivery. It renders templates client-side and delivers via an external
// EmailProvider (AWS SES).
//
// Architecture reference: 08b-email-worker.md Section 2, 5
type EmailChannel struct {
	provider  external.EmailProvider
	templates TemplateService
	logger    types.Logger
}

// EmailChannelConfig holds the dependencies needed to create an EmailChannel.
type EmailChannelConfig struct {
	Provider  external.EmailProvider
	Templates TemplateService
	Logger    types.Logger
}

// NewEmailChannel creates a new EmailChannel with the given dependencies.
func NewEmailChannel(cfg EmailChannelConfig) *EmailChannel {
	return &EmailChannel{
		provider:  cfg.Provider,
		templates: cfg.Templates,
		logger:    cfg.Logger,
	}
}

// Type returns the channel type identifier for email.
func (e *EmailChannel) Type() types.ChannelType {
	return types.ChannelEmail
}

// ValidateConfig checks if the email channel configuration is valid.
// For email channels, the config must contain a valid "address" field.
func (e *EmailChannel) ValidateConfig(config map[string]any) error {
	addr, ok := config["address"]
	if !ok {
		return fmt.Errorf("email channel: missing required 'address' field")
	}

	addrStr, ok := addr.(string)
	if !ok || addrStr == "" {
		return fmt.Errorf("email channel: 'address' must be a non-empty string")
	}

	return nil
}

// Format transforms a Notification into a channel-specific JSON payload that
// can be later consumed by Deliver. For email, this is the serialized
// Notification itself (since template resolution happens in Deliver).
func (e *EmailChannel) Format(ctx context.Context, n *types.Notification, config map[string]any) ([]byte, error) {
	if n == nil {
		return nil, fmt.Errorf("email channel: notification is nil")
	}

	payload, err := json.Marshal(n)
	if err != nil {
		return nil, fmt.Errorf("email channel: failed to marshal notification: %w", err)
	}

	return payload, nil
}

// Deliver executes the email transmission. It implements the full delivery
// pipeline from 08b-email-worker.md Section 5:
//
//  1. Redact destination for logging
//  2. Parse Notification from payload
//  3. Test Mode bypass
//  4. Render template with soft-fail fallback (VERT-003)
//  5. Send pre-rendered email via provider
//  6. Handle synchronous failures (blocklist -> terminal, others -> retry)
func (e *EmailChannel) Deliver(ctx context.Context, payload []byte, destination string) (*types.DeliveryResult, error) {
	// 1. Redact destination for logging.
	e.logger.Info("attempting email delivery", "dest", RedactEmail(destination))

	// 2. Parse Notification Payload.
	var n types.Notification
	if err := json.Unmarshal(payload, &n); err != nil {
		return nil, fmt.Errorf("email channel: failed to unmarshal notification: %w", err)
	}

	// 3. Test Mode Bypass.
	if n.TestMode {
		e.logger.Info("test mode: suppressing email", "notification_id", n.ID)
		return &types.DeliveryResult{
			Status:            types.DeliveryStatusSkipped,
			ProviderMessageID: "test-simulated",
		}, nil
	}

	// 4. Render template with soft-fail fallback (VERT-003/VERT-004).
	rendered, sender, err := e.templates.Render(n.TemplateSet, n.EventType, &n)
	if err != nil {
		e.logger.Error("template rendering failed",
			"requested_set", n.TemplateSet,
			"event_type", string(n.EventType),
			"error", err.Error(),
		)
		return nil, err
	}

	// 5. Send pre-rendered email via provider.
	msgID, err := e.provider.Send(ctx, types.SendInput{
		To:          destination,
		From:        sender,
		Subject:     rendered.Subject,
		BodyHTML:    rendered.BodyHTML,
		BodyText:    rendered.BodyText,
		ReferenceID: n.ID,
	})

	// 7. Handle synchronous failures.
	if err != nil {
		if IsBlocklistError(err) {
			// Treat as Hard Bounce immediately (Permanent Failure).
			e.logger.Warn("recipient blocked by provider",
				"dest", RedactEmail(destination),
				"notification_id", n.ID,
			)
			return &types.DeliveryResult{
				Status:        types.DeliveryStatusBounced,
				FailureReason: "address_blocked",
				Retryable:     false,
			}, nil
		}
		// Other errors trigger retry logic in core.
		return nil, err
	}

	return &types.DeliveryResult{
		ProviderMessageID: msgID,
		Status:            types.DeliveryStatusSent,
	}, nil
}

// ShouldRetry inspects an error to determine if it is transient and the
// delivery should be retried. Email-specific blocklist errors are terminal
// (non-retryable). Other errors are considered transient by default.
func (e *EmailChannel) ShouldRetry(err error) bool {
	if err == nil {
		return false
	}

	// Blocklist errors are terminal - do not retry.
	if IsBlocklistError(err) {
		return false
	}

	// Check for AppError-specific non-retryable codes.
	var appErr *types.AppError
	if errors.As(err, &appErr) {
		switch appErr.Code {
		case types.ErrCodeEmailBlocked:
			return false
		case types.ErrCodeUpstreamRateLimited:
			// Rate limited - should retry with delay.
			return true
		case types.ErrCodeUpstreamUnavailable:
			// Provider temporarily down - should retry.
			return true
		}
	}

	// Default: assume transient, allow retry.
	return true
}

// Compile-time assertion that EmailChannel implements types.NotificationChannel.
var _ types.NotificationChannel = (*EmailChannel)(nil)
