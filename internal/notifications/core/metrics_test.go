package core

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"watchpoint/internal/types"
)

// mockCloudWatchClient records PutMetricData calls for verification.
type mockCloudWatchClient struct {
	calls     []*cloudwatch.PutMetricDataInput
	returnErr error
}

func (m *mockCloudWatchClient) PutMetricData(_ context.Context, params *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
	m.calls = append(m.calls, params)
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	return &cloudwatch.PutMetricDataOutput{}, nil
}

func TestCloudWatchNotificationMetrics_RecordDelivery_Success(t *testing.T) {
	cw := &mockCloudWatchClient{}
	logger := &mockLogger{}
	metrics := NewCloudWatchNotificationMetrics(cw, logger)

	metrics.RecordDelivery(context.Background(), types.ChannelEmail, MetricSuccess)

	if len(cw.calls) != 1 {
		t.Fatalf("expected 1 PutMetricData call, got %d", len(cw.calls))
	}

	input := cw.calls[0]
	if *input.Namespace != types.MetricNamespace {
		t.Errorf("expected namespace %q, got %q", types.MetricNamespace, *input.Namespace)
	}

	if len(input.MetricData) != 1 {
		t.Fatalf("expected 1 metric datum, got %d", len(input.MetricData))
	}

	datum := input.MetricData[0]
	if *datum.MetricName != types.MetricDeliveryAttempt {
		t.Errorf("expected metric name %q, got %q", types.MetricDeliveryAttempt, *datum.MetricName)
	}
	if *datum.Value != 1.0 {
		t.Errorf("expected value 1.0, got %f", *datum.Value)
	}
	if datum.Unit != cwtypes.StandardUnitCount {
		t.Errorf("expected unit Count, got %s", datum.Unit)
	}

	// Verify dimensions match OBS-010: {Channel: "email", Result: "success"}
	assertDimension(t, datum.Dimensions, types.DimChannel, string(types.ChannelEmail))
	assertDimension(t, datum.Dimensions, types.DimResult, string(MetricSuccess))
}

func TestCloudWatchNotificationMetrics_RecordDelivery_WebhookFailed(t *testing.T) {
	cw := &mockCloudWatchClient{}
	logger := &mockLogger{}
	metrics := NewCloudWatchNotificationMetrics(cw, logger)

	metrics.RecordDelivery(context.Background(), types.ChannelWebhook, MetricFailed)

	if len(cw.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(cw.calls))
	}

	datum := cw.calls[0].MetricData[0]
	assertDimension(t, datum.Dimensions, types.DimChannel, string(types.ChannelWebhook))
	assertDimension(t, datum.Dimensions, types.DimResult, string(MetricFailed))
}

func TestCloudWatchNotificationMetrics_RecordDelivery_Skipped(t *testing.T) {
	cw := &mockCloudWatchClient{}
	logger := &mockLogger{}
	metrics := NewCloudWatchNotificationMetrics(cw, logger)

	metrics.RecordDelivery(context.Background(), types.ChannelEmail, MetricSkipped)

	if len(cw.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(cw.calls))
	}

	datum := cw.calls[0].MetricData[0]
	assertDimension(t, datum.Dimensions, types.DimResult, string(MetricSkipped))
}

func TestCloudWatchNotificationMetrics_RecordDelivery_CloudWatchError(t *testing.T) {
	// CloudWatch errors should be logged but not returned (fire-and-forget).
	cw := &mockCloudWatchClient{returnErr: fmt.Errorf("cloudwatch unavailable")}
	logger := &mockLogger{}
	metrics := NewCloudWatchNotificationMetrics(cw, logger)

	// Should not panic or return error.
	metrics.RecordDelivery(context.Background(), types.ChannelEmail, MetricSuccess)

	if len(cw.calls) != 1 {
		t.Errorf("expected 1 call attempt, got %d", len(cw.calls))
	}
}

func TestCloudWatchNotificationMetrics_RecordLatency(t *testing.T) {
	cw := &mockCloudWatchClient{}
	logger := &mockLogger{}
	metrics := NewCloudWatchNotificationMetrics(cw, logger)

	metrics.RecordLatency(context.Background(), types.ChannelWebhook, 250*time.Millisecond)

	if len(cw.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(cw.calls))
	}

	datum := cw.calls[0].MetricData[0]
	if *datum.Value != 250.0 {
		t.Errorf("expected latency value 250.0ms, got %f", *datum.Value)
	}
	if datum.Unit != cwtypes.StandardUnitMilliseconds {
		t.Errorf("expected unit Milliseconds, got %s", datum.Unit)
	}
	assertDimension(t, datum.Dimensions, types.DimChannel, string(types.ChannelWebhook))
}

func TestCloudWatchNotificationMetrics_RecordQueueLag(t *testing.T) {
	cw := &mockCloudWatchClient{}
	logger := &mockLogger{}
	metrics := NewCloudWatchNotificationMetrics(cw, logger)

	metrics.RecordQueueLag(context.Background(), 3*time.Second)

	if len(cw.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(cw.calls))
	}

	datum := cw.calls[0].MetricData[0]
	if *datum.MetricName != "NotificationQueueLag" {
		t.Errorf("expected metric name NotificationQueueLag, got %s", *datum.MetricName)
	}
	if *datum.Value != 3000.0 {
		t.Errorf("expected lag value 3000.0ms, got %f", *datum.Value)
	}
	if datum.Unit != cwtypes.StandardUnitMilliseconds {
		t.Errorf("expected unit Milliseconds, got %s", datum.Unit)
	}
}

// assertDimension verifies a specific dimension exists with the expected value.
func assertDimension(t *testing.T, dims []cwtypes.Dimension, name, expectedValue string) {
	t.Helper()
	for _, d := range dims {
		if *d.Name == name {
			if *d.Value != expectedValue {
				t.Errorf("dimension %q: expected value %q, got %q", name, expectedValue, *d.Value)
			}
			return
		}
	}
	t.Errorf("dimension %q not found in %v", name, dims)
}
