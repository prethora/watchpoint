package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwTypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqsTypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"watchpoint/internal/types"
)

// --- Mock SQS API ---

type mockSQSAPI struct {
	calls     []mockSendBatchCall
	failNext  bool
	failPartial bool
}

type mockSendBatchCall struct {
	queueURL string
	entries  []sqsTypes.SendMessageBatchRequestEntry
}

func (m *mockSQSAPI) SendMessageBatch(ctx context.Context, params *sqs.SendMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error) {
	if m.failNext {
		m.failNext = false
		return nil, fmt.Errorf("simulated SQS API failure")
	}

	m.calls = append(m.calls, mockSendBatchCall{
		queueURL: aws.ToString(params.QueueUrl),
		entries:  params.Entries,
	})

	output := &sqs.SendMessageBatchOutput{}
	if m.failPartial {
		m.failPartial = false
		output.Failed = []sqsTypes.BatchResultErrorEntry{
			{
				Id:      aws.String("msg-0"),
				Code:    aws.String("InternalError"),
				Message: aws.String("simulated partial failure"),
			},
		}
	}

	return output, nil
}

// --- Mock CloudWatch API ---

type mockCloudWatchAPI struct {
	calls    []mockPutMetricCall
	failNext bool
}

type mockPutMetricCall struct {
	namespace  string
	metricData []cwTypes.MetricDatum
}

func (m *mockCloudWatchAPI) PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
	if m.failNext {
		m.failNext = false
		return nil, fmt.Errorf("simulated CloudWatch failure")
	}
	m.calls = append(m.calls, mockPutMetricCall{
		namespace:  aws.ToString(params.Namespace),
		metricData: params.MetricData,
	})
	return &cloudwatch.PutMetricDataOutput{}, nil
}

// --- liveSQSClient Tests ---

func TestLiveSQSClient_SendBatch_SingleChunk(t *testing.T) {
	mock := &mockSQSAPI{}
	client := &liveSQSClient{client: mock}

	messages := []types.EvalMessage{
		{BatchID: "b1", TileID: "tile_1", ForecastType: types.ForecastNowcast},
		{BatchID: "b1", TileID: "tile_2", ForecastType: types.ForecastNowcast},
	}

	err := client.SendBatch(context.Background(), "https://sqs/test", messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 API call, got %d", len(mock.calls))
	}
	if mock.calls[0].queueURL != "https://sqs/test" {
		t.Errorf("expected queue URL 'https://sqs/test', got %q", mock.calls[0].queueURL)
	}
	if len(mock.calls[0].entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(mock.calls[0].entries))
	}

	// Verify message body is valid JSON
	var decoded types.EvalMessage
	body := aws.ToString(mock.calls[0].entries[0].MessageBody)
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("failed to unmarshal message body: %v", err)
	}
	if decoded.TileID != "tile_1" {
		t.Errorf("expected TileID 'tile_1', got %q", decoded.TileID)
	}
}

func TestLiveSQSClient_SendBatch_MultipleChunks(t *testing.T) {
	mock := &mockSQSAPI{}
	client := &liveSQSClient{client: mock}

	// Create 25 messages -- should result in 3 API calls (10 + 10 + 5)
	messages := make([]types.EvalMessage, 25)
	for i := range messages {
		messages[i] = types.EvalMessage{
			BatchID: "b1",
			TileID:  fmt.Sprintf("tile_%d", i),
		}
	}

	err := client.SendBatch(context.Background(), "https://sqs/test", messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.calls) != 3 {
		t.Fatalf("expected 3 API calls for 25 messages, got %d", len(mock.calls))
	}

	if len(mock.calls[0].entries) != 10 {
		t.Errorf("first chunk: expected 10 entries, got %d", len(mock.calls[0].entries))
	}
	if len(mock.calls[1].entries) != 10 {
		t.Errorf("second chunk: expected 10 entries, got %d", len(mock.calls[1].entries))
	}
	if len(mock.calls[2].entries) != 5 {
		t.Errorf("third chunk: expected 5 entries, got %d", len(mock.calls[2].entries))
	}
}

func TestLiveSQSClient_SendBatch_APIFailure(t *testing.T) {
	mock := &mockSQSAPI{failNext: true}
	client := &liveSQSClient{client: mock}

	messages := []types.EvalMessage{
		{BatchID: "b1", TileID: "tile_1"},
	}

	err := client.SendBatch(context.Background(), "https://sqs/test", messages)
	if err == nil {
		t.Fatal("expected error for API failure, got nil")
	}
}

func TestLiveSQSClient_SendBatch_PartialFailure(t *testing.T) {
	mock := &mockSQSAPI{failPartial: true}
	client := &liveSQSClient{client: mock}

	messages := []types.EvalMessage{
		{BatchID: "b1", TileID: "tile_1"},
	}

	err := client.SendBatch(context.Background(), "https://sqs/test", messages)
	if err == nil {
		t.Fatal("expected error for partial failure, got nil")
	}
}

func TestLiveSQSClient_SendBatch_ContextCancellation(t *testing.T) {
	mock := &mockSQSAPI{}
	client := &liveSQSClient{client: mock}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	messages := []types.EvalMessage{
		{BatchID: "b1", TileID: "tile_1"},
	}

	err := client.SendBatch(ctx, "https://sqs/test", messages)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestLiveSQSClient_SendBatch_Empty(t *testing.T) {
	mock := &mockSQSAPI{}
	client := &liveSQSClient{client: mock}

	err := client.SendBatch(context.Background(), "https://sqs/test", nil)
	if err != nil {
		t.Fatalf("unexpected error for empty messages: %v", err)
	}

	if len(mock.calls) != 0 {
		t.Errorf("expected 0 API calls for empty messages, got %d", len(mock.calls))
	}
}

func TestLiveSQSClient_SendBatch_MessageIDs(t *testing.T) {
	// Verify that message IDs are unique and sequential.
	mock := &mockSQSAPI{}
	client := &liveSQSClient{client: mock}

	messages := make([]types.EvalMessage, 3)
	for i := range messages {
		messages[i] = types.EvalMessage{BatchID: "b1", TileID: fmt.Sprintf("tile_%d", i)}
	}

	err := client.SendBatch(context.Background(), "https://sqs/test", messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedIDs := []string{"msg-0", "msg-1", "msg-2"}
	for i, entry := range mock.calls[0].entries {
		if aws.ToString(entry.Id) != expectedIDs[i] {
			t.Errorf("entry %d: expected ID %q, got %q", i, expectedIDs[i], aws.ToString(entry.Id))
		}
	}
}

// --- liveMetricPublisher Tests ---

func TestLiveMetricPublisher_PublishForecastReady(t *testing.T) {
	mock := &mockCloudWatchAPI{}
	pub := &liveMetricPublisher{client: mock, namespace: "WatchPoint"}

	err := pub.PublishForecastReady(context.Background(), types.ForecastNowcast)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 CloudWatch call, got %d", len(mock.calls))
	}

	call := mock.calls[0]
	if call.namespace != "WatchPoint" {
		t.Errorf("expected namespace 'WatchPoint', got %q", call.namespace)
	}
	if len(call.metricData) != 1 {
		t.Fatalf("expected 1 metric datum, got %d", len(call.metricData))
	}

	datum := call.metricData[0]
	if aws.ToString(datum.MetricName) != "ForecastReady" {
		t.Errorf("expected metric name 'ForecastReady', got %q", aws.ToString(datum.MetricName))
	}
	if aws.ToFloat64(datum.Value) != 1.0 {
		t.Errorf("expected value 1.0, got %f", aws.ToFloat64(datum.Value))
	}
	if datum.Unit != cwTypes.StandardUnitCount {
		t.Errorf("expected unit Count, got %v", datum.Unit)
	}

	// Verify ForecastType dimension
	if len(datum.Dimensions) != 1 {
		t.Fatalf("expected 1 dimension, got %d", len(datum.Dimensions))
	}
	if aws.ToString(datum.Dimensions[0].Name) != "ForecastType" {
		t.Errorf("expected dimension name 'ForecastType', got %q", aws.ToString(datum.Dimensions[0].Name))
	}
	if aws.ToString(datum.Dimensions[0].Value) != "nowcast" {
		t.Errorf("expected dimension value 'nowcast', got %q", aws.ToString(datum.Dimensions[0].Value))
	}
}

func TestLiveMetricPublisher_PublishForecastReady_Failure(t *testing.T) {
	mock := &mockCloudWatchAPI{failNext: true}
	pub := &liveMetricPublisher{client: mock, namespace: "WatchPoint"}

	err := pub.PublishForecastReady(context.Background(), types.ForecastMediumRange)
	if err == nil {
		t.Fatal("expected error for CloudWatch failure, got nil")
	}
}

func TestLiveMetricPublisher_PublishStats(t *testing.T) {
	mock := &mockCloudWatchAPI{}
	pub := &liveMetricPublisher{client: mock, namespace: "WatchPoint"}

	err := pub.PublishStats(context.Background(), types.ForecastMediumRange, 5, 1200)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 CloudWatch call, got %d", len(mock.calls))
	}

	call := mock.calls[0]
	if len(call.metricData) != 2 {
		t.Fatalf("expected 2 metric data points, got %d", len(call.metricData))
	}

	// Find BatchSizeTiles and BatchSizeWatchPoints
	var foundTiles, foundWPs bool
	for _, datum := range call.metricData {
		name := aws.ToString(datum.MetricName)
		switch name {
		case "BatchSizeTiles":
			foundTiles = true
			if aws.ToFloat64(datum.Value) != 5.0 {
				t.Errorf("BatchSizeTiles value: expected 5.0, got %f", aws.ToFloat64(datum.Value))
			}
		case "BatchSizeWatchPoints":
			foundWPs = true
			if aws.ToFloat64(datum.Value) != 1200.0 {
				t.Errorf("BatchSizeWatchPoints value: expected 1200.0, got %f", aws.ToFloat64(datum.Value))
			}
		default:
			t.Errorf("unexpected metric name: %q", name)
		}

		// Verify ForecastType dimension
		if len(datum.Dimensions) != 1 {
			t.Errorf("metric %q: expected 1 dimension, got %d", name, len(datum.Dimensions))
		} else if aws.ToString(datum.Dimensions[0].Value) != "medium_range" {
			t.Errorf("metric %q: expected dimension value 'medium_range', got %q",
				name, aws.ToString(datum.Dimensions[0].Value))
		}
	}

	if !foundTiles {
		t.Error("missing BatchSizeTiles metric")
	}
	if !foundWPs {
		t.Error("missing BatchSizeWatchPoints metric")
	}
}

func TestLiveMetricPublisher_PublishStats_Failure(t *testing.T) {
	mock := &mockCloudWatchAPI{failNext: true}
	pub := &liveMetricPublisher{client: mock, namespace: "WatchPoint"}

	err := pub.PublishStats(context.Background(), types.ForecastNowcast, 10, 500)
	if err == nil {
		t.Fatal("expected error for CloudWatch failure, got nil")
	}
}
