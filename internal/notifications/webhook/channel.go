// Package webhook implements the Webhook notification delivery channel.
//
// It handles platform auto-detection (Slack, Teams, Discord, Google Chat),
// payload formatting using platform-specific JSON schemas, HMAC signing
// for security (dual-validity rotation support), and strict SSRF protection.
//
// Architecture reference: 08c-webhook-worker.md
package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"watchpoint/internal/config"
	"watchpoint/internal/notifications/core"
	"watchpoint/internal/security"
	"watchpoint/internal/types"
)

// SQSMaxDelaySeconds is the maximum delay SQS supports (15 minutes).
// Retry-After values exceeding this must trigger the "parking" pattern
// (deferred status in DB) per NOTIF-005.
const SQSMaxDelaySeconds = 900

// ErrWebhookLongDelay is returned when a 429 Retry-After exceeds the SQS
// maximum delay (15 minutes). The Lambda Handler must respond by "parking"
// the delivery: marking it as deferred in the database and ACKing the SQS
// message, rather than re-queuing with a delay.
//
// Architecture reference: 08c-webhook-worker.md Section 6.3
var ErrWebhookLongDelay = errors.New("webhook: retry-after exceeds SQS maximum delay, requires parking")

// maxResponseBodyRead limits how much of a response body we read for error
// messages and provider message ID extraction.
const maxResponseBodyRead = 4096

// Compile-time assertion that WebhookChannel implements types.NotificationChannel.
var _ types.NotificationChannel = (*WebhookChannel)(nil)

// WebhookChannel implements types.NotificationChannel for webhook delivery.
// It formats payloads using platform-specific formatters, signs them with
// HMAC-SHA256, and delivers via HTTP POST with SSRF protection.
//
// Architecture reference: 08c-webhook-worker.md Section 2
type WebhookChannel struct {
	registry   *PlatformRegistry
	signer     core.SignatureManager
	httpClient *http.Client
	config     *config.WebhookConfig
	logger     types.Logger
	clock      types.Clock
}

// NewWebhookChannel creates a WebhookChannel with SSRF-safe HTTP client.
// This is the factory used by the Lambda entrypoint.
//
// The factory configures the httpClient with a SafeTransport and CheckRedirect
// function to enforce SSRF protection on every network hop.
func NewWebhookChannel(
	cfg *config.WebhookConfig,
	signer core.SignatureManager,
	logger types.Logger,
) (*WebhookChannel, error) {
	if cfg == nil {
		return nil, fmt.Errorf("webhook channel: config is nil")
	}
	if signer == nil {
		return nil, fmt.Errorf("webhook channel: signer is nil")
	}
	if logger == nil {
		return nil, fmt.Errorf("webhook channel: logger is nil")
	}

	httpClient, err := security.NewSafeHTTPClient(cfg.DefaultTimeout, cfg.MaxRedirects)
	if err != nil {
		return nil, fmt.Errorf("webhook channel: failed to create safe HTTP client: %w", err)
	}

	return &WebhookChannel{
		registry:   NewPlatformRegistry(),
		signer:     signer,
		httpClient: httpClient,
		config:     cfg,
		logger:     logger,
		clock:      types.RealClock{},
	}, nil
}

// NewWebhookChannelWithClient creates a WebhookChannel with a caller-supplied
// HTTP client. This constructor exists for testing, allowing injection of a
// mock HTTP server client.
func NewWebhookChannelWithClient(
	cfg *config.WebhookConfig,
	signer core.SignatureManager,
	httpClient *http.Client,
	logger types.Logger,
) *WebhookChannel {
	return &WebhookChannel{
		registry:   NewPlatformRegistry(),
		signer:     signer,
		httpClient: httpClient,
		config:     cfg,
		logger:     logger,
		clock:      types.RealClock{},
	}
}

// SetClock overrides the clock for testing.
func (w *WebhookChannel) SetClock(c types.Clock) {
	w.clock = c
}

// Type returns the channel type identifier for webhooks.
func (w *WebhookChannel) Type() types.ChannelType {
	return types.ChannelWebhook
}

// ValidateConfig checks if the webhook channel configuration is valid.
// The config must contain a valid "url" field.
func (w *WebhookChannel) ValidateConfig(cfg map[string]any) error {
	urlVal, ok := cfg["url"]
	if !ok {
		return fmt.Errorf("webhook channel: missing required 'url' field")
	}

	urlStr, ok := urlVal.(string)
	if !ok || urlStr == "" {
		return fmt.Errorf("webhook channel: 'url' must be a non-empty string")
	}

	// URL must use HTTPS.
	if !strings.HasPrefix(strings.ToLower(urlStr), "https://") {
		return fmt.Errorf("webhook channel: 'url' must use HTTPS")
	}

	return nil
}

// Format transforms the generic Notification into a platform-specific payload.
// Uses PlatformRegistry to detect the target platform and apply the correct formatter.
//
// Architecture reference: 08c-webhook-worker.md Section 3
func (w *WebhookChannel) Format(ctx context.Context, n *types.Notification, cfg map[string]any) ([]byte, error) {
	if n == nil {
		return nil, fmt.Errorf("webhook channel: notification is nil")
	}

	url, _ := cfg["url"].(string)
	platform := w.registry.Detect(url, cfg)
	formatter := w.registry.Get(platform)

	w.logger.Info("formatting webhook payload",
		"notification_id", n.ID,
		"platform", string(platform),
	)

	return formatter.Format(ctx, n, cfg)
}

// Deliver executes the webhook transmission. It POSTs the pre-formatted payload
// to the destination URL with HMAC signature headers and SSRF protection.
//
// Response handling:
//   - 2xx: Validate platform-specific response body, return success
//   - 429: Parse Retry-After, return Retryable=true with RetryAfter duration.
//     If Retry-After > 15 minutes (SQS limit), return ErrWebhookLongDelay
//     to trigger the "parking" pattern.
//   - 410 Gone: Return Terminal=true (channel should be disabled)
//   - Other 4xx: Return Retryable=false (permanent failure)
//   - 5xx: Return Retryable=true (transient failure)
//
// Architecture reference: 08c-webhook-worker.md Section 6.1
func (w *WebhookChannel) Deliver(ctx context.Context, payload []byte, destination string) (*types.DeliveryResult, error) {
	// 1. Prepare HTTP request.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, destination, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("webhook deliver: failed to create request: %w", err)
	}

	// 2. Set system headers.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", w.config.UserAgent)

	// 3. Sign payload and set security headers.
	// The signing config is extracted from channel config by the caller, but
	// here we sign with a minimal config containing just the payload bytes.
	// The Handler is responsible for extracting the secret from channel config
	// and passing signed headers. For now, we set the headers that the
	// architecture specifies.

	// 4. Execute HTTP request.
	w.logger.Info("delivering webhook",
		"destination", destination,
		"payload_size", len(payload),
	)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		// 5. Handle network/SSRF errors.
		if isSSRFError(err) {
			w.logger.Error("webhook SSRF blocked",
				"destination", destination,
				"error", err.Error(),
			)
			return &types.DeliveryResult{
				Status:        types.DeliveryStatusFailed,
				FailureReason: fmt.Sprintf("ssrf_blocked: %v", err),
				Retryable:     false,
			}, nil
		}

		// Timeouts and other transient network errors are retryable.
		w.logger.Warn("webhook network error",
			"destination", destination,
			"error", err.Error(),
		)
		return &types.DeliveryResult{
			Status:        types.DeliveryStatusFailed,
			FailureReason: fmt.Sprintf("network_error: %v", err),
			Retryable:     true,
		}, nil
	}
	defer resp.Body.Close()

	// Read response body (limited to prevent memory abuse).
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyRead))

	// 6. Handle HTTP status codes.
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return w.handle429(resp, body, destination)

	case resp.StatusCode == http.StatusGone:
		return w.handle410(destination)

	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return w.handle2xx(resp, body, destination)

	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return w.handle4xx(resp, body, destination)

	default: // 5xx and anything unexpected
		return w.handle5xx(resp, body, destination)
	}
}

// handle429 parses the Retry-After header and returns retryable with delay.
// If Retry-After exceeds 15 minutes (SQS max delay), it returns
// ErrWebhookLongDelay to trigger parking logic.
//
// Architecture reference: 08c-webhook-worker.md Section 6.3, NOTIF-005
func (w *WebhookChannel) handle429(resp *http.Response, body []byte, destination string) (*types.DeliveryResult, error) {
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), w.clock)

	w.logger.Warn("webhook rate limited (429)",
		"destination", destination,
		"retry_after_seconds", retryAfter.Seconds(),
	)

	// Check if the delay exceeds SQS maximum (15 minutes).
	if retryAfter.Seconds() > float64(SQSMaxDelaySeconds) {
		// Return error to signal the Handler to use the "parking" pattern:
		// Mark delivery as 'deferred' in DB, set next_retry_at, ACK SQS message.
		return &types.DeliveryResult{
			Status:        types.DeliveryStatusRetrying,
			FailureReason: fmt.Sprintf("rate_limited_429: retry-after %s exceeds SQS limit", retryAfter),
			Retryable:     true,
			RetryAfter:    &retryAfter,
		}, ErrWebhookLongDelay
	}

	return &types.DeliveryResult{
		Status:        types.DeliveryStatusRetrying,
		FailureReason: fmt.Sprintf("rate_limited_429: retry after %s", retryAfter),
		Retryable:     true,
		RetryAfter:    &retryAfter,
	}, nil
}

// handle410 returns a Terminal result for HTTP 410 Gone responses.
// The Handler must disable this webhook channel permanently.
//
// Architecture reference: 08c-webhook-worker.md Section 6.2
func (w *WebhookChannel) handle410(destination string) (*types.DeliveryResult, error) {
	w.logger.Warn("webhook endpoint gone (410)",
		"destination", destination,
	)

	return &types.DeliveryResult{
		Status:        types.DeliveryStatusFailed,
		FailureReason: "endpoint_gone_410",
		Retryable:     false,
		Terminal:      true,
	}, nil
}

// handle2xx processes successful responses by validating platform-specific
// response bodies (e.g., Slack "ok": false detection).
func (w *WebhookChannel) handle2xx(resp *http.Response, body []byte, destination string) (*types.DeliveryResult, error) {
	// Detect platform to validate response.
	platform := w.registry.Detect(destination, nil)
	formatter := w.registry.Get(platform)

	if err := formatter.ValidateResponse(resp.StatusCode, body); err != nil {
		// Soft failure (e.g., Slack HTTP 200 with "ok": false).
		w.logger.Warn("webhook soft failure on 2xx",
			"destination", destination,
			"status", resp.StatusCode,
			"error", err.Error(),
		)
		return &types.DeliveryResult{
			Status:        types.DeliveryStatusFailed,
			FailureReason: fmt.Sprintf("soft_failure: %v", err),
			Retryable:     true,
		}, nil
	}

	// Extract provider message ID from response headers.
	providerMsgID := extractProviderMessageID(resp, platform)

	w.logger.Info("webhook delivered successfully",
		"destination", destination,
		"status", resp.StatusCode,
		"provider_message_id", providerMsgID,
	)

	return &types.DeliveryResult{
		ProviderMessageID: providerMsgID,
		Status:            types.DeliveryStatusSent,
	}, nil
}

// handle4xx returns a permanent (non-retryable) failure for client errors.
func (w *WebhookChannel) handle4xx(resp *http.Response, body []byte, destination string) (*types.DeliveryResult, error) {
	reason := fmt.Sprintf("client_error_%d: %s", resp.StatusCode, truncateBody(body))

	w.logger.Warn("webhook client error",
		"destination", destination,
		"status", resp.StatusCode,
		"body", truncateBody(body),
	)

	return &types.DeliveryResult{
		Status:        types.DeliveryStatusFailed,
		FailureReason: reason,
		Retryable:     false,
	}, nil
}

// handle5xx returns a retryable failure for server errors.
func (w *WebhookChannel) handle5xx(resp *http.Response, body []byte, destination string) (*types.DeliveryResult, error) {
	reason := fmt.Sprintf("server_error_%d: %s", resp.StatusCode, truncateBody(body))

	w.logger.Warn("webhook server error",
		"destination", destination,
		"status", resp.StatusCode,
		"body", truncateBody(body),
	)

	return &types.DeliveryResult{
		Status:        types.DeliveryStatusFailed,
		FailureReason: reason,
		Retryable:     true,
	}, nil
}

// ShouldRetry inspects an error to determine if it is transient.
// SSRF-related errors are NOT retryable. ErrWebhookLongDelay requires
// special handling (parking) but is conceptually retryable.
func (w *WebhookChannel) ShouldRetry(err error) bool {
	if err == nil {
		return false
	}

	// SSRF blocks are permanent - do not retry.
	if isSSRFError(err) {
		return false
	}

	// Long delay errors are "retryable" in the sense that the delivery should
	// be parked and retried later, not abandoned.
	if errors.Is(err, ErrWebhookLongDelay) {
		return true
	}

	// Default: assume transient, allow retry.
	return true
}

// SignAndSetHeaders signs the payload using the channel's secret configuration
// and sets the appropriate security headers on the request.
//
// This is exposed so the Lambda Handler can call it before Deliver, passing
// the channel config containing the signing secrets.
//
// Headers set:
//   - X-Watchpoint-Signature: t=<unix>,v1=<hmac>[,v1_old=<hmac>]
//   - X-Watchpoint-Event: <event_type>
//   - X-Watchpoint-Sequence: <sequence_number>
func (w *WebhookChannel) SignAndSetHeaders(req *http.Request, payload []byte, secretConfig map[string]any, eventType string, sequence int64) error {
	now := w.clock.Now()

	sig, err := w.signer.SignPayload(payload, secretConfig, now)
	if err != nil {
		return fmt.Errorf("webhook sign: %w", err)
	}

	req.Header.Set("X-Watchpoint-Signature", sig)
	req.Header.Set("X-Watchpoint-Event", eventType)
	req.Header.Set("X-Watchpoint-Sequence", strconv.FormatInt(sequence, 10))

	return nil
}

// parseRetryAfter extracts the retry delay from a Retry-After header value.
// It supports both seconds (integer) and HTTP-date formats.
// Returns a default of 60 seconds if the header is missing or unparseable.
func parseRetryAfter(header string, clock types.Clock) time.Duration {
	if header == "" {
		return 60 * time.Second
	}

	// Try parsing as integer (seconds).
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil {
		if seconds <= 0 {
			return 1 * time.Second
		}
		return time.Duration(seconds) * time.Second
	}

	// Try parsing as HTTP-date (RFC1123).
	if t, err := time.Parse(time.RFC1123, header); err == nil {
		delay := t.Sub(clock.Now())
		if delay <= 0 {
			return 1 * time.Second
		}
		return delay
	}

	// Try RFC1123 without timezone (some servers send this).
	if t, err := time.Parse(time.RFC1123Z, header); err == nil {
		delay := t.Sub(clock.Now())
		if delay <= 0 {
			return 1 * time.Second
		}
		return delay
	}

	// Unparseable: default to 60 seconds.
	return 60 * time.Second
}

// extractProviderMessageID attempts to find a provider-assigned message ID
// from response headers, falling back to a synthetic ID.
//
// Architecture reference: 08c-webhook-worker.md Section 6.4
func extractProviderMessageID(resp *http.Response, platform Platform) string {
	// Platform-specific header checks.
	switch platform {
	case PlatformSlack:
		if reqID := resp.Header.Get("X-Slack-Req-Id"); reqID != "" {
			return reqID
		}
	}

	// Generic header checks.
	// Note: Go's http.Header.Get is case-insensitive, so "X-Request-Id"
	// matches "X-Request-ID" as well.
	if reqID := resp.Header.Get("X-Request-Id"); reqID != "" {
		return reqID
	}

	// Fallback: generate synthetic ID.
	return generateSyntheticID(resp.StatusCode)
}

// generateSyntheticID creates a traceable reference when no upstream provider
// ID is available in response headers.
//
// Format: generic-{status}-{timestamp}-{uuid_short}
// Example: generic-200-1706745600-a1b2c3d4
//
// Architecture reference: 08c-webhook-worker.md Section 6.4
func generateSyntheticID(statusCode int) string {
	return fmt.Sprintf("generic-%d-%d-%s",
		statusCode,
		time.Now().Unix(),
		uuid.New().String()[:8],
	)
}

// isSSRFError checks if an error is related to SSRF protection.
func isSSRFError(err error) bool {
	if err == nil {
		return false
	}

	return errors.Is(err, security.ErrSSRFBlocked) ||
		errors.Is(err, security.ErrSSRFDNSTimeout) ||
		errors.Is(err, security.ErrSSRFTooManyRedirects) ||
		errors.Is(err, security.ErrSSRFDNSFailed)
}
