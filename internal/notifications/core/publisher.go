package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"watchpoint/internal/types"
)

// SQSSender abstracts the SQS SendMessage operation for testability.
// Production code uses the *sqs.Client from aws-sdk-go-v2.
type SQSSender interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// NotificationPublisher wraps an SQS client to publish NotificationMessages
// for retry or initial dispatch. It implements the Publish-Subscribe Retry
// Pattern from architecture/08a-notification-core.md Section 7.1.
//
// The key contract: Publish increments msg.RetryCount BEFORE serializing to
// JSON, ensuring the downstream consumer sees the updated retry state.
type NotificationPublisher struct {
	client   SQSSender
	queueURL string
	logger   types.Logger
}

// NewNotificationPublisher creates a new NotificationPublisher targeting the
// specified SQS notification queue.
func NewNotificationPublisher(client SQSSender, queueURL string, logger types.Logger) *NotificationPublisher {
	return &NotificationPublisher{
		client:   client,
		queueURL: queueURL,
		logger:   logger,
	}
}

// Publish increments the message's RetryCount, serializes it to JSON, and
// sends it to the notification SQS queue with the specified delay.
//
// The delay parameter controls the SQS DelaySeconds for exponential backoff.
// SQS enforces a maximum of 900 seconds (15 minutes). If delay exceeds this
// limit, it is clamped to 900 seconds. For longer delays, callers should use
// DeliveryManager.MarkDeferred instead (the "parking" pattern from NOTIF-005).
//
// RetryCount increment is critical: it ensures the next consumer of the
// message sees an accurate retry attempt number and can apply correct backoff
// calculations or determine when max retries have been exhausted.
func (p *NotificationPublisher) Publish(ctx context.Context, msg types.NotificationMessage, delay time.Duration) error {
	// Increment RetryCount BEFORE serialization. This is the key contract
	// documented in 08a-notification-core.md Section 7.1 step 3.
	msg.RetryCount++

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("notification publisher: failed to marshal message: %w", err)
	}

	// Clamp delay to SQS maximum of 900 seconds.
	delaySec := int32(delay.Seconds())
	if delaySec > 900 {
		delaySec = 900
	}
	if delaySec < 0 {
		delaySec = 0
	}

	input := &sqs.SendMessageInput{
		QueueUrl:     aws.String(p.queueURL),
		MessageBody:  aws.String(string(body)),
		DelaySeconds: delaySec,
	}

	_, err = p.client.SendMessage(ctx, input)
	if err != nil {
		return fmt.Errorf("notification publisher: failed to send message to %s: %w", p.queueURL, err)
	}

	p.logger.Info("notification message published",
		"notification_id", msg.NotificationID,
		"watchpoint_id", msg.WatchPointID,
		"retry_count", msg.RetryCount,
		"delay_seconds", delaySec,
		"trace_id", msg.TraceID,
	)

	return nil
}
