// Package queue provides SQS-based message producers for dispatching evaluation
// and notification payloads to downstream workers.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqsTypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"watchpoint/internal/config"
	"watchpoint/internal/types"
)

// SQSSender abstracts the SQS SendMessage operation for testability.
// Production code uses the *sqs.Client from aws-sdk-go-v2.
type SQSSender interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// EvalTrigger implements the EvaluationTrigger interface defined in
// architecture/05b-api-watchpoints.md Section 3.2. It serializes an EvalMessage
// and sends it to the appropriate SQS queue based on ForecastType.
//
// Queue routing:
//   - ForecastNowcast ("nowcast")       -> Urgent queue
//   - ForecastMediumRange ("medium_range") -> Standard queue
//   - Targeted evaluations (Resume/Update with SpecificWatchPointIDs) -> Standard queue
type EvalTrigger struct {
	client           SQSSender
	urgentQueueURL   string
	standardQueueURL string
	logger           *slog.Logger
}

// NewEvalTrigger creates a new EvalTrigger with the given SQS client and
// configuration. It reads queue URLs from the AWSConfig.
func NewEvalTrigger(client SQSSender, awsCfg config.AWSConfig, logger *slog.Logger) *EvalTrigger {
	return &EvalTrigger{
		client:           client,
		urgentQueueURL:   awsCfg.EvalQueueUrgent,
		standardQueueURL: awsCfg.EvalQueueStandard,
		logger:           logger,
	}
}

// TriggerEvaluation enqueues an EvalMessage for a specific WatchPoint.
// This is used for targeted evaluations (e.g., on Resume) where a single
// WatchPoint needs immediate re-evaluation. The wpID is mapped to the
// SpecificWatchPointIDs field in the message to ensure targeted execution.
//
// Targeted evaluations are always routed to the Standard queue because the
// API does not know the WatchPoint's associated ForecastType at call time.
func (t *EvalTrigger) TriggerEvaluation(ctx context.Context, wpID string, reason string) error {
	msg := types.EvalMessage{
		BatchID:               fmt.Sprintf("targeted_%s", uuid.New().String()),
		TraceID:               uuid.New().String(),
		ForecastType:          "", // Unknown for targeted; worker handles discovery
		RunTimestamp:          time.Now().UTC(),
		TileID:                "", // Unknown for targeted; worker resolves from WatchPoint
		Page:                  1,
		PageSize:              1,
		TotalItems:            1,
		Action:                types.EvalActionEvaluate,
		SpecificWatchPointIDs: []string{wpID},
	}

	return t.sendMessage(ctx, msg, reason)
}

// SendEvalMessage sends an EvalMessage to the appropriate SQS queue based on
// the ForecastType in the message. This is the general-purpose method used by
// the Batcher for batch dispatches.
//
// Queue routing:
//   - ForecastNowcast -> Urgent queue
//   - ForecastMediumRange -> Standard queue
//   - Empty/unknown ForecastType (targeted) -> Standard queue
func (t *EvalTrigger) SendEvalMessage(ctx context.Context, msg types.EvalMessage, reason string) error {
	return t.sendMessage(ctx, msg, reason)
}

// queueURLForMessage selects the appropriate queue URL based on the message's
// ForecastType. Nowcast messages go to the Urgent queue; everything else
// (including targeted evaluations with empty ForecastType) goes to Standard.
func (t *EvalTrigger) queueURLForMessage(msg types.EvalMessage) string {
	if msg.ForecastType == types.ForecastNowcast {
		return t.urgentQueueURL
	}
	return t.standardQueueURL
}

// sendMessage serializes the EvalMessage to JSON and dispatches it to the
// selected SQS queue.
func (t *EvalTrigger) sendMessage(ctx context.Context, msg types.EvalMessage, reason string) error {
	queueURL := t.queueURLForMessage(msg)

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("queue: failed to marshal EvalMessage: %w", err)
	}

	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String(queueURL),
		MessageBody: aws.String(string(body)),
		MessageAttributes: map[string]sqsTypes.MessageAttributeValue{
			"reason": {
				DataType:    aws.String("String"),
				StringValue: aws.String(reason),
			},
		},
	}

	_, err = t.client.SendMessage(ctx, input)
	if err != nil {
		return fmt.Errorf("queue: failed to send EvalMessage to %s: %w", queueURL, err)
	}

	t.logger.InfoContext(ctx, "evaluation message sent",
		"queue_url", queueURL,
		"batch_id", msg.BatchID,
		"trace_id", msg.TraceID,
		"forecast_type", string(msg.ForecastType),
		"tile_id", msg.TileID,
		"specific_wp_ids", msg.SpecificWatchPointIDs,
		"reason", reason,
	)

	return nil
}
