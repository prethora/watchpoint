package core

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"watchpoint/internal/types"
)

// CloudWatchClient abstracts the CloudWatch PutMetricData operation for testability.
type CloudWatchClient interface {
	PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

// CloudWatchNotificationMetrics implements the NotificationMetrics interface
// by emitting metrics to AWS CloudWatch. It matches the OBS-010 specification
// for notification delivery metrics.
//
// Metrics emitted:
//   - DeliveryAttempt: Dims {Channel, Result} -- on every delivery outcome
//   - DeliveryLatency: Dims {Channel} -- time taken for delivery attempt
//   - QueueLag: No dims -- time between message enqueue and processing start
//
// Compile-time assertion that CloudWatchNotificationMetrics implements NotificationMetrics.
var _ NotificationMetrics = (*CloudWatchNotificationMetrics)(nil)

type CloudWatchNotificationMetrics struct {
	client    CloudWatchClient
	namespace string
	logger    types.Logger
}

// NewCloudWatchNotificationMetrics creates a new CloudWatchNotificationMetrics
// that publishes to the specified CloudWatch namespace.
func NewCloudWatchNotificationMetrics(client CloudWatchClient, logger types.Logger) *CloudWatchNotificationMetrics {
	return &CloudWatchNotificationMetrics{
		client:    client,
		namespace: types.MetricNamespace,
		logger:    logger,
	}
}

// RecordDelivery emits a DeliveryAttempt metric with Channel and Result dimensions.
// This matches the OBS-010 flow simulation:
//
//	Metric: DeliveryAttempt, Dims: {Channel: "email", Result: "success"}
func (m *CloudWatchNotificationMetrics) RecordDelivery(ctx context.Context, channel types.ChannelType, result MetricResult) {
	input := &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(m.namespace),
		MetricData: []cwtypes.MetricDatum{
			{
				MetricName: aws.String(types.MetricDeliveryAttempt),
				Value:      aws.Float64(1),
				Unit:       cwtypes.StandardUnitCount,
				Dimensions: []cwtypes.Dimension{
					{
						Name:  aws.String(types.DimChannel),
						Value: aws.String(string(channel)),
					},
					{
						Name:  aws.String(types.DimResult),
						Value: aws.String(string(result)),
					},
				},
			},
		},
	}

	if _, err := m.client.PutMetricData(ctx, input); err != nil {
		m.logger.Error("failed to record delivery metric",
			"error", err.Error(),
			"channel", string(channel),
			"result", string(result),
		)
	}
}

// RecordLatency emits a delivery latency metric with the Channel dimension.
// Duration is recorded in milliseconds for CloudWatch precision.
func (m *CloudWatchNotificationMetrics) RecordLatency(ctx context.Context, channel types.ChannelType, duration time.Duration) {
	input := &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(m.namespace),
		MetricData: []cwtypes.MetricDatum{
			{
				MetricName: aws.String(fmt.Sprintf("%sLatency", types.MetricDeliveryAttempt)),
				Value:      aws.Float64(float64(duration.Milliseconds())),
				Unit:       cwtypes.StandardUnitMilliseconds,
				Dimensions: []cwtypes.Dimension{
					{
						Name:  aws.String(types.DimChannel),
						Value: aws.String(string(channel)),
					},
				},
			},
		},
	}

	if _, err := m.client.PutMetricData(ctx, input); err != nil {
		m.logger.Error("failed to record latency metric",
			"error", err.Error(),
			"channel", string(channel),
			"duration_ms", duration.Milliseconds(),
		)
	}
}

// RecordQueueLag emits a metric tracking the time between SQS message
// enqueue and worker processing start. This measures the end-to-end
// queue delay including SQS visibility timeout and any backlog.
func (m *CloudWatchNotificationMetrics) RecordQueueLag(ctx context.Context, lag time.Duration) {
	input := &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(m.namespace),
		MetricData: []cwtypes.MetricDatum{
			{
				MetricName: aws.String("NotificationQueueLag"),
				Value:      aws.Float64(float64(lag.Milliseconds())),
				Unit:       cwtypes.StandardUnitMilliseconds,
			},
		},
	}

	if _, err := m.client.PutMetricData(ctx, input); err != nil {
		m.logger.Error("failed to record queue lag metric",
			"error", err.Error(),
			"lag_ms", lag.Milliseconds(),
		)
	}
}
