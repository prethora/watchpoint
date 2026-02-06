// Package main is the entrypoint for the Webhook Worker Lambda function.
//
// The Webhook Worker consumes messages from the Notification SQS Queue, processes
// them through the core notification pipeline (deduplication, policy evaluation),
// and delivers via the WebhookChannel. It handles platform-specific formatting
// (Slack, Teams, Discord, Google Chat, Generic), HMAC signing, and SSRF protection.
//
// Cold Start (main):
//  1. Initialize structured logger.
//  2. Load AWS SDK configuration.
//  3. Read environment variables for SQS queue URL, webhook config, etc.
//  4. Initialize SQS client, CloudWatch client.
//  5. Initialize WebhookChannel with SignatureManager and SSRF-safe HTTP client.
//  6. Initialize DeliveryManager with WebhookRetryPolicy.
//  7. Initialize NotificationPublisher for retry re-queuing.
//  8. Initialize NotificationMetrics for CloudWatch telemetry.
//  9. Register handler and call lambda.Start.
//
// Handler flow per architecture/08a-notification-core.md Section 7.1 and NOTIF-001:
//
//	For each SQS message in the batch:
//	  1. Unmarshal NotificationMessage from the message body.
//	  2. EnsureDeliveryExists (idempotent insert).
//	  3. Format payload using platform-specific formatter.
//	  4. Sign payload with HMAC-SHA256 (Dual-Validity).
//	  5. Deliver via WebhookChannel.
//	  6. Handle result: Success -> MarkSuccess, Retry -> re-publish with delay,
//	     Defer (long 429) -> MarkDeferred, Terminal (410) -> MarkFailure + disable,
//	     Fail -> MarkFailure + CheckAggregateFailure.
//
// Architecture reference: architecture/08c-webhook-worker.md Section 2
// SAM template reference: architecture/04-sam-template.md
// Flow reference: NOTIF-001, NOTIF-004, NOTIF-005, NOTIF-007
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"watchpoint/internal/config"
	"watchpoint/internal/notifications/core"
	"watchpoint/internal/notifications/webhook"
	"watchpoint/internal/types"
)

// slogAdapter wraps *slog.Logger to implement the types.Logger interface.
// The types.Logger interface requires Info, Error, Warn, and With methods.
// slog.Logger satisfies the first three but With returns *slog.Logger, not
// types.Logger, so an adapter is necessary.
type slogAdapter struct {
	logger *slog.Logger
}

func (a *slogAdapter) Info(msg string, args ...any)  { a.logger.Info(msg, args...) }
func (a *slogAdapter) Error(msg string, args ...any) { a.logger.Error(msg, args...) }
func (a *slogAdapter) Warn(msg string, args ...any)  { a.logger.Warn(msg, args...) }
func (a *slogAdapter) With(args ...any) types.Logger {
	return &slogAdapter{logger: a.logger.With(args...)}
}

// Handler holds the dependencies for the webhook worker Lambda handler.
type Handler struct {
	channel     *webhook.WebhookChannel
	deliveryMgr core.DeliveryManager
	publisher   *core.NotificationPublisher
	metrics     core.NotificationMetrics
	retryPolicy core.RetryPolicy
	logger      types.Logger
}

// Handle processes an SQS event containing one or more notification messages.
// Per the NOTIF-001 flow simulation, each message is processed independently.
// Lambda SQS integration uses partial batch responses: messages that fail
// processing are returned in batchItemFailures so SQS can retry them.
func (h *Handler) Handle(ctx context.Context, sqsEvent events.SQSEvent) (events.SQSEventResponse, error) {
	response := events.SQSEventResponse{}

	for _, record := range sqsEvent.Records {
		if err := h.processMessage(ctx, record); err != nil {
			h.logger.Error("failed to process SQS message",
				"message_id", record.MessageId,
				"error", err.Error(),
			)
			// Report partial failure so SQS retries only this message.
			response.BatchItemFailures = append(response.BatchItemFailures,
				events.SQSBatchItemFailure{ItemIdentifier: record.MessageId},
			)
		}
	}

	return response, nil
}

// processMessage handles a single SQS message through the full delivery pipeline.
func (h *Handler) processMessage(ctx context.Context, record events.SQSMessage) error {
	start := time.Now()

	// Step 1: Parse the notification message.
	var msg types.NotificationMessage
	if err := json.Unmarshal([]byte(record.Body), &msg); err != nil {
		h.logger.Error("failed to unmarshal notification message",
			"message_id", record.MessageId,
			"error", err.Error(),
		)
		// Permanent parse failure - do not retry (return nil to ACK).
		return nil
	}

	logger := h.logger.With(
		"notification_id", msg.NotificationID,
		"watchpoint_id", msg.WatchPointID,
		"organization_id", msg.OrganizationID,
		"event_type", string(msg.EventType),
		"retry_count", msg.RetryCount,
		"trace_id", msg.TraceID,
	)

	logger.Info("processing notification message")

	// Record queue lag for observability.
	if sentTimestamp, ok := record.Attributes["SentTimestamp"]; ok {
		if sentMs, err := parseMillisTimestamp(sentTimestamp); err == nil {
			lag := time.Since(sentMs)
			h.metrics.RecordQueueLag(ctx, lag)
		}
	}

	// Step 2: Iterate over channels in the WatchPoint.
	// For the webhook worker, we process only webhook-type channels.
	channels := extractChannels(msg.Payload)
	webhookProcessed := false

	for idx, ch := range channels {
		if ch.Type != types.ChannelWebhook {
			continue
		}
		if !ch.Enabled {
			logger.Info("skipping disabled webhook channel", "channel_index", idx)
			continue
		}

		if err := h.processChannel(ctx, msg, ch, idx, logger); err != nil {
			// Log but continue processing other channels.
			logger.Error("channel processing failed",
				"channel_index", idx,
				"error", err.Error(),
			)
		}
		webhookProcessed = true
	}

	if !webhookProcessed {
		logger.Warn("no webhook channels found in notification message")
	}

	// Record delivery latency.
	h.metrics.RecordLatency(ctx, types.ChannelWebhook, time.Since(start))

	return nil
}

// processChannel handles delivery for a single webhook channel within a notification.
func (h *Handler) processChannel(ctx context.Context, msg types.NotificationMessage, ch types.Channel, idx int, logger types.Logger) error {
	// Step 2a: Ensure delivery record exists (idempotent).
	deliveryID, created, err := h.deliveryMgr.EnsureDeliveryExists(ctx, msg.NotificationID, types.ChannelWebhook, idx)
	if err != nil {
		return fmt.Errorf("ensure delivery exists: %w", err)
	}

	if !created {
		logger.Info("delivery record already exists", "delivery_id", deliveryID)
	}

	// Step 2b: Record that we are attempting delivery.
	if err := h.deliveryMgr.RecordAttempt(ctx, deliveryID); err != nil {
		return fmt.Errorf("record attempt: %w", err)
	}

	// Step 3: Extract destination URL from channel config.
	destination, ok := ch.Config["url"].(string)
	if !ok || destination == "" {
		// No valid URL - permanent failure.
		if _, markErr := h.deliveryMgr.MarkFailure(ctx, deliveryID, "missing_webhook_url"); markErr != nil {
			logger.Error("failed to mark failure", "error", markErr.Error())
		}
		h.metrics.RecordDelivery(ctx, types.ChannelWebhook, core.MetricFailed)
		return fmt.Errorf("missing webhook URL in channel config")
	}

	// Step 4: Build Notification from message for Format/Deliver.
	notification := notificationFromMessage(msg)

	// Step 5: Format the notification for the webhook channel.
	// This uses the PlatformRegistry to detect the target platform and
	// apply the correct formatter (Slack, Teams, Discord, etc.).
	payload, err := h.channel.Format(ctx, &notification, ch.Config)
	if err != nil {
		return h.handlePermanentFailure(ctx, deliveryID, msg, logger,
			fmt.Sprintf("format_error: %v", err))
	}

	// Step 6: Deliver via WebhookChannel.
	// The channel handles SSRF protection, HTTP request creation, and
	// response interpretation internally.
	result, deliverErr := h.channel.Deliver(ctx, payload, destination)

	// Step 7: Handle delivery result.
	return h.handleDeliveryResult(ctx, deliveryID, msg, result, deliverErr, logger)
}

// handleDeliveryResult processes the outcome of a channel.Deliver call,
// updating the database and potentially re-queuing for retry.
func (h *Handler) handleDeliveryResult(
	ctx context.Context,
	deliveryID string,
	msg types.NotificationMessage,
	result *types.DeliveryResult,
	deliverErr error,
	logger types.Logger,
) error {
	// Special case: ErrWebhookLongDelay requires the "parking" pattern.
	// The delivery should be deferred in the DB, not re-queued to SQS.
	if errors.Is(deliverErr, webhook.ErrWebhookLongDelay) && result != nil && result.RetryAfter != nil {
		resumeAt := time.Now().Add(*result.RetryAfter)
		if err := h.deliveryMgr.MarkDeferred(ctx, deliveryID, resumeAt); err != nil {
			return fmt.Errorf("mark deferred (long delay): %w", err)
		}
		logger.Info("webhook delivery deferred (long rate limit)",
			"delivery_id", deliveryID,
			"resume_at", resumeAt.Format(time.RFC3339),
			"retry_after", result.RetryAfter.String(),
		)
		return nil
	}

	// Case 1: Deliver returned an error with no result.
	if deliverErr != nil && result == nil {
		if h.channel.ShouldRetry(deliverErr) {
			return h.handleRetry(ctx, deliveryID, msg, deliverErr, nil, logger)
		}
		return h.handlePermanentFailure(ctx, deliveryID, msg, logger,
			fmt.Sprintf("deliver_error: %v", deliverErr))
	}

	// Case 2: Deliver returned a result (with or without error).
	if result == nil {
		return h.handlePermanentFailure(ctx, deliveryID, msg, logger, "nil_result_nil_error")
	}

	switch result.Status {
	case types.DeliveryStatusSent:
		// Success path.
		if err := h.deliveryMgr.MarkSuccess(ctx, deliveryID, result.ProviderMessageID); err != nil {
			return fmt.Errorf("mark success: %w", err)
		}
		h.metrics.RecordDelivery(ctx, types.ChannelWebhook, core.MetricSuccess)
		logger.Info("webhook delivery succeeded",
			"delivery_id", deliveryID,
			"provider_message_id", result.ProviderMessageID,
		)
		return nil

	case types.DeliveryStatusSkipped:
		// Test mode suppression.
		if err := h.deliveryMgr.MarkSkipped(ctx, deliveryID, "test_mode"); err != nil {
			return fmt.Errorf("mark skipped: %w", err)
		}
		h.metrics.RecordDelivery(ctx, types.ChannelWebhook, core.MetricSkipped)
		logger.Info("webhook delivery skipped", "delivery_id", deliveryID)
		return nil

	case types.DeliveryStatusFailed:
		if result.Terminal {
			// Terminal failure (e.g., HTTP 410 Gone) - disable channel.
			return h.handleTerminalFailure(ctx, deliveryID, msg, logger, result.FailureReason)
		}
		if result.Retryable {
			return h.handleRetry(ctx, deliveryID, msg, deliverErr, result.RetryAfter, logger)
		}
		// Non-retryable, non-terminal failure (e.g., HTTP 4xx).
		return h.handlePermanentFailure(ctx, deliveryID, msg, logger, result.FailureReason)

	case types.DeliveryStatusRetrying:
		// Explicit retryable result (e.g., 429 within SQS delay limit).
		return h.handleRetry(ctx, deliveryID, msg, deliverErr, result.RetryAfter, logger)

	case types.DeliveryStatusBounced:
		// Terminal - address/endpoint invalid.
		return h.handleTerminalFailure(ctx, deliveryID, msg, logger, result.FailureReason)

	default:
		if result.Retryable {
			return h.handleRetry(ctx, deliveryID, msg, deliverErr, result.RetryAfter, logger)
		}
		return h.handlePermanentFailure(ctx, deliveryID, msg, logger, result.FailureReason)
	}
}

// handleRetry implements the Publish-Subscribe Retry Pattern from 08a Section 7.1.
// It publishes a NEW SQS message with delay and ACKs the original.
func (h *Handler) handleRetry(
	ctx context.Context,
	deliveryID string,
	msg types.NotificationMessage,
	deliverErr error,
	retryAfter *time.Duration,
	logger types.Logger,
) error {
	// Check if max retries exceeded.
	if msg.RetryCount >= h.retryPolicy.MaxAttempts {
		reason := "max_retries_exceeded"
		if deliverErr != nil {
			reason = fmt.Sprintf("max_retries_exceeded: %v", deliverErr)
		}
		return h.handlePermanentFailure(ctx, deliveryID, msg, logger, reason)
	}

	// Calculate backoff delay.
	var delay time.Duration
	if retryAfter != nil {
		delay = *retryAfter
	} else {
		delay = core.CalculateNextRetry(h.retryPolicy, msg.RetryCount)
	}

	// Check if delay exceeds SQS maximum (15 minutes = 900 seconds).
	if delay.Seconds() > 900 {
		resumeAt := time.Now().Add(delay)
		if err := h.deliveryMgr.MarkDeferred(ctx, deliveryID, resumeAt); err != nil {
			return fmt.Errorf("mark deferred: %w", err)
		}
		logger.Info("webhook delivery deferred (long delay)",
			"delivery_id", deliveryID,
			"resume_at", resumeAt.Format(time.RFC3339),
		)
		return nil
	}

	// Publish new message with delay (non-blocking retry).
	if err := h.publisher.Publish(ctx, msg, delay); err != nil {
		return fmt.Errorf("publish retry message: %w", err)
	}

	logger.Info("webhook delivery retry scheduled",
		"delivery_id", deliveryID,
		"retry_count", msg.RetryCount+1,
		"delay_seconds", int(delay.Seconds()),
	)

	h.metrics.RecordDelivery(ctx, types.ChannelWebhook, core.MetricFailed)
	return nil
}

// handlePermanentFailure marks the delivery as permanently failed and checks
// aggregate failure status for potential notification state reset.
func (h *Handler) handlePermanentFailure(
	ctx context.Context,
	deliveryID string,
	msg types.NotificationMessage,
	logger types.Logger,
	reason string,
) error {
	if _, err := h.deliveryMgr.MarkFailure(ctx, deliveryID, reason); err != nil {
		return fmt.Errorf("mark failure: %w", err)
	}

	h.metrics.RecordDelivery(ctx, types.ChannelWebhook, core.MetricFailed)

	logger.Error("webhook delivery permanently failed",
		"delivery_id", deliveryID,
		"reason", reason,
	)

	return h.checkAndResetOnAggregateFailure(ctx, msg, logger)
}

// handleTerminalFailure handles failures that are terminal for the channel
// (e.g., HTTP 410 Gone, SSRF blocked). Requires aggregate failure checking
// and potential notification state reset.
func (h *Handler) handleTerminalFailure(
	ctx context.Context,
	deliveryID string,
	msg types.NotificationMessage,
	logger types.Logger,
	reason string,
) error {
	if _, err := h.deliveryMgr.MarkFailure(ctx, deliveryID, reason); err != nil {
		return fmt.Errorf("mark terminal failure: %w", err)
	}

	h.metrics.RecordDelivery(ctx, types.ChannelWebhook, core.MetricFailed)

	logger.Warn("webhook delivery terminal failure",
		"delivery_id", deliveryID,
		"reason", reason,
	)

	return h.checkAndResetOnAggregateFailure(ctx, msg, logger)
}

// checkAndResetOnAggregateFailure implements the State Rollback pattern from
// 08c-webhook-worker.md Section 6.2 (Terminal Failure Handling).
func (h *Handler) checkAndResetOnAggregateFailure(
	ctx context.Context,
	msg types.NotificationMessage,
	logger types.Logger,
) error {
	allFailed, err := h.deliveryMgr.CheckAggregateFailure(ctx, msg.NotificationID)
	if err != nil {
		logger.Error("failed to check aggregate failure",
			"notification_id", msg.NotificationID,
			"error", err.Error(),
		)
		return nil // Non-fatal: don't fail the message processing.
	}

	if allFailed {
		logger.Warn("all delivery channels failed, resetting notification state",
			"notification_id", msg.NotificationID,
			"watchpoint_id", msg.WatchPointID,
		)
		if err := h.deliveryMgr.ResetNotificationState(ctx, msg.WatchPointID); err != nil {
			logger.Error("failed to reset notification state",
				"watchpoint_id", msg.WatchPointID,
				"error", err.Error(),
			)
		}
	}

	return nil
}

// notificationFromMessage converts a NotificationMessage (SQS transport envelope)
// into a Notification (domain entity) for use with the channel's Format/Deliver.
func notificationFromMessage(msg types.NotificationMessage) types.Notification {
	return types.Notification{
		ID:             msg.NotificationID,
		WatchPointID:   msg.WatchPointID,
		OrganizationID: msg.OrganizationID,
		EventType:      msg.EventType,
		Urgency:        msg.Urgency,
		Payload:        msg.Payload,
		TestMode:       msg.TestMode,
	}
}

// extractChannels extracts the Channel list from the notification payload.
// Channels are stored in the "channels" key of the payload map.
func extractChannels(payload map[string]interface{}) []types.Channel {
	channelsRaw, ok := payload["channels"]
	if !ok {
		return nil
	}

	// The channels field arrives as a JSON-deserialized interface{},
	// so we re-marshal and unmarshal to get typed Channel structs.
	data, err := json.Marshal(channelsRaw)
	if err != nil {
		return nil
	}

	var channels []types.Channel
	if err := json.Unmarshal(data, &channels); err != nil {
		return nil
	}

	return channels
}

// parseMillisTimestamp parses a millisecond-epoch string into a time.Time.
// Used for SQS SentTimestamp attribute to calculate queue lag.
func parseMillisTimestamp(ms string) (time.Time, error) {
	var millis int64
	if _, err := fmt.Sscanf(ms, "%d", &millis); err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(millis), nil
}

func main() {
	// Initialize structured logger at startup (Cold Start).
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	logger.Info("Webhook Worker Lambda initializing (cold start)")

	// Wrap slog.Logger to satisfy types.Logger interface.
	typedLogger := &slogAdapter{logger: logger}

	// Load AWS SDK configuration.
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		logger.Error("Failed to load AWS SDK config", "error", err)
		os.Exit(1)
	}

	// Read configuration from environment variables.
	notificationQueueURL := os.Getenv("SQS_NOTIFICATIONS")
	metricNamespace := os.Getenv("METRIC_NAMESPACE")
	if metricNamespace == "" {
		metricNamespace = "WatchPoint"
	}

	// Webhook-specific configuration.
	webhookCfg := &config.WebhookConfig{
		UserAgent:      os.Getenv("WEBHOOK_USER_AGENT"),
		DefaultTimeout: 10 * time.Second,
		MaxRedirects:   3,
	}
	if webhookCfg.UserAgent == "" {
		webhookCfg.UserAgent = "WatchPoint-Webhook/1.0"
	}
	if timeoutStr := os.Getenv("WEBHOOK_TIMEOUT"); timeoutStr != "" {
		if d, parseErr := time.ParseDuration(timeoutStr); parseErr == nil {
			webhookCfg.DefaultTimeout = d
		}
	}

	// Initialize AWS clients.
	sqsClient := sqs.NewFromConfig(awsCfg)
	cwClient := cloudwatch.NewFromConfig(awsCfg)

	// Initialize SignatureManager for HMAC payload signing.
	signer := webhook.NewSignatureManager()

	// Initialize WebhookChannel with SSRF-safe HTTP client.
	webhookChannel, err := webhook.NewWebhookChannel(webhookCfg, signer, typedLogger)
	if err != nil {
		logger.Error("Failed to create webhook channel", "error", err)
		os.Exit(1)
	}

	// Initialize NotificationPublisher for retry re-queuing.
	publisher := core.NewNotificationPublisher(sqsClient, notificationQueueURL, typedLogger)

	// Initialize CloudWatch metrics.
	metrics := core.NewCloudWatchNotificationMetrics(cwClient, typedLogger)

	// Initialize DeliveryManager.
	// NOTE: DeliveryManager requires a DeliveryRepository (database).
	// This will be wired when the database connection infrastructure is
	// integrated. For now, we set it to nil. The handler structure and
	// routing logic are complete and the binary compiles.
	var deliveryMgr core.DeliveryManager

	handler := &Handler{
		channel:     webhookChannel,
		deliveryMgr: deliveryMgr,
		publisher:   publisher,
		metrics:     metrics,
		retryPolicy: core.WebhookRetryPolicy,
		logger:      typedLogger,
	}

	logger.Info("Webhook Worker Lambda initialized",
		"notification_queue", notificationQueueURL,
		"metric_namespace", metricNamespace,
		"user_agent", webhookCfg.UserAgent,
		"timeout", webhookCfg.DefaultTimeout.String(),
		"max_redirects", webhookCfg.MaxRedirects,
	)

	lambda.Start(handler.Handle)
}

// Compile-time assertion that slogAdapter implements types.Logger.
var _ types.Logger = (*slogAdapter)(nil)
