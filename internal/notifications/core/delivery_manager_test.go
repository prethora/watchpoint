package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// mockDeliveryRepo implements DeliveryRepository for testing.
type mockDeliveryRepo struct {
	// Tracking calls
	insertCalled    bool
	insertDelivery  *types.NotificationDelivery
	insertReturnID  string
	insertCreated   bool
	insertErr       error

	updateCalled    bool
	updateID        string
	updateStatus    string
	updateReason    string
	updateErr       error

	successCalled   bool
	successID       string
	successProvider string
	successErr      error

	deferredCalled  bool
	deferredID      string
	deferredResume  time.Time
	deferredErr     error

	attemptCalled   bool
	attemptID       string
	attemptErr      error

	attemptCount    int
	attemptCountErr error

	nonFailedCount  int
	nonFailedErr    error

	resetCalled     bool
	resetWPID       string
	resetErr        error

	cancelCalled    bool
	cancelWPID      string
	cancelErr       error
}

func (m *mockDeliveryRepo) InsertDeliveryIfNotExists(_ context.Context, d *types.NotificationDelivery) (string, bool, error) {
	m.insertCalled = true
	m.insertDelivery = d
	if m.insertErr != nil {
		return "", false, m.insertErr
	}
	id := m.insertReturnID
	if id == "" {
		id = d.ID
	}
	return id, m.insertCreated, nil
}

func (m *mockDeliveryRepo) UpdateDeliveryStatus(_ context.Context, id, status, reason string) error {
	m.updateCalled = true
	m.updateID = id
	m.updateStatus = status
	m.updateReason = reason
	return m.updateErr
}

func (m *mockDeliveryRepo) SetDeliverySuccess(_ context.Context, id, providerMsgID string) error {
	m.successCalled = true
	m.successID = id
	m.successProvider = providerMsgID
	return m.successErr
}

func (m *mockDeliveryRepo) SetDeliveryDeferred(_ context.Context, id string, resumeAt time.Time) error {
	m.deferredCalled = true
	m.deferredID = id
	m.deferredResume = resumeAt
	return m.deferredErr
}

func (m *mockDeliveryRepo) IncrementAttempt(_ context.Context, id string) error {
	m.attemptCalled = true
	m.attemptID = id
	return m.attemptErr
}

func (m *mockDeliveryRepo) GetDeliveryAttemptCount(_ context.Context, _ string) (int, error) {
	return m.attemptCount, m.attemptCountErr
}

func (m *mockDeliveryRepo) CountNonFailedDeliveries(_ context.Context, _ string) (int, error) {
	return m.nonFailedCount, m.nonFailedErr
}

func (m *mockDeliveryRepo) ResetEvaluationNotificationState(_ context.Context, wpID string) error {
	m.resetCalled = true
	m.resetWPID = wpID
	return m.resetErr
}

func (m *mockDeliveryRepo) CancelDeferredDeliveries(_ context.Context, wpID string) error {
	m.cancelCalled = true
	m.cancelWPID = wpID
	return m.cancelErr
}

func TestDeliveryManager_EnsureDeliveryExists_NewRecord(t *testing.T) {
	repo := &mockDeliveryRepo{
		insertCreated: true,
	}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	id, created, err := mgr.EnsureDeliveryExists(context.Background(), "notif_123", types.ChannelEmail, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !created {
		t.Error("expected created=true for new record")
	}
	expectedID := "del_notif_123_email_0"
	if id != expectedID {
		t.Errorf("expected ID %q, got %q", expectedID, id)
	}
	if !repo.insertCalled {
		t.Error("expected insert to be called")
	}
	if repo.insertDelivery.NotificationID != "notif_123" {
		t.Errorf("expected notif_123, got %s", repo.insertDelivery.NotificationID)
	}
	if repo.insertDelivery.ChannelType != types.ChannelEmail {
		t.Errorf("expected email channel, got %s", repo.insertDelivery.ChannelType)
	}
}

func TestDeliveryManager_EnsureDeliveryExists_AlreadyExists(t *testing.T) {
	repo := &mockDeliveryRepo{
		insertCreated: false,
	}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	_, created, err := mgr.EnsureDeliveryExists(context.Background(), "notif_456", types.ChannelWebhook, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created {
		t.Error("expected created=false for existing record")
	}
}

func TestDeliveryManager_EnsureDeliveryExists_Error(t *testing.T) {
	repo := &mockDeliveryRepo{
		insertErr: errors.New("db connection lost"),
	}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	_, _, err := mgr.EnsureDeliveryExists(context.Background(), "notif_789", types.ChannelEmail, 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeliveryManager_MarkSuccess(t *testing.T) {
	repo := &mockDeliveryRepo{}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	err := mgr.MarkSuccess(context.Background(), "del_1", "msg_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !repo.successCalled {
		t.Error("expected SetDeliverySuccess to be called")
	}
	if repo.successID != "del_1" {
		t.Errorf("expected del_1, got %s", repo.successID)
	}
	if repo.successProvider != "msg_abc" {
		t.Errorf("expected msg_abc, got %s", repo.successProvider)
	}
}

func TestDeliveryManager_MarkFailure_ShouldRetry(t *testing.T) {
	repo := &mockDeliveryRepo{
		attemptCount: 1, // below max of 3
	}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	shouldRetry, err := mgr.MarkFailure(context.Background(), "del_1", "timeout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !shouldRetry {
		t.Error("expected shouldRetry=true when under max attempts")
	}
	if !repo.updateCalled {
		t.Error("expected UpdateDeliveryStatus to be called")
	}
	if repo.updateStatus != string(types.DeliveryStatusRetrying) {
		t.Errorf("expected retrying status, got %s", repo.updateStatus)
	}
}

func TestDeliveryManager_MarkFailure_MaxRetriesExhausted(t *testing.T) {
	repo := &mockDeliveryRepo{
		attemptCount: 3, // equal to max of 3 for EmailRetryPolicy
	}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	shouldRetry, err := mgr.MarkFailure(context.Background(), "del_1", "permanent error")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shouldRetry {
		t.Error("expected shouldRetry=false when max attempts exhausted")
	}
	if repo.updateStatus != string(types.DeliveryStatusFailed) {
		t.Errorf("expected failed status, got %s", repo.updateStatus)
	}
}

func TestDeliveryManager_MarkSkipped(t *testing.T) {
	repo := &mockDeliveryRepo{}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	err := mgr.MarkSkipped(context.Background(), "del_1", "test_mode")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !repo.updateCalled {
		t.Error("expected UpdateDeliveryStatus to be called")
	}
	if repo.updateStatus != string(types.DeliveryStatusSkipped) {
		t.Errorf("expected skipped status, got %s", repo.updateStatus)
	}
	if repo.updateReason != "test_mode" {
		t.Errorf("expected reason test_mode, got %s", repo.updateReason)
	}
}

func TestDeliveryManager_MarkDeferred(t *testing.T) {
	repo := &mockDeliveryRepo{}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	resumeAt := time.Date(2026, 2, 3, 8, 0, 0, 0, time.UTC)
	err := mgr.MarkDeferred(context.Background(), "del_1", resumeAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !repo.deferredCalled {
		t.Error("expected SetDeliveryDeferred to be called")
	}
	if !repo.deferredResume.Equal(resumeAt) {
		t.Errorf("expected resume at %s, got %s", resumeAt, repo.deferredResume)
	}
}

func TestDeliveryManager_CheckAggregateFailure_AllFailed(t *testing.T) {
	repo := &mockDeliveryRepo{
		nonFailedCount: 0, // no non-failed deliveries
	}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	allFailed, err := mgr.CheckAggregateFailure(context.Background(), "notif_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allFailed {
		t.Error("expected allFailed=true when no non-failed deliveries exist")
	}
}

func TestDeliveryManager_CheckAggregateFailure_SomeRemaining(t *testing.T) {
	repo := &mockDeliveryRepo{
		nonFailedCount: 1, // one non-failed delivery remains
	}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	allFailed, err := mgr.CheckAggregateFailure(context.Background(), "notif_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allFailed {
		t.Error("expected allFailed=false when non-failed deliveries exist")
	}
}

func TestDeliveryManager_ResetNotificationState(t *testing.T) {
	repo := &mockDeliveryRepo{}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	err := mgr.ResetNotificationState(context.Background(), "wp_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !repo.resetCalled {
		t.Error("expected ResetEvaluationNotificationState to be called")
	}
	if repo.resetWPID != "wp_123" {
		t.Errorf("expected wp_123, got %s", repo.resetWPID)
	}
}

func TestDeliveryManager_CancelDeferred(t *testing.T) {
	repo := &mockDeliveryRepo{}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	err := mgr.CancelDeferred(context.Background(), "wp_456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !repo.cancelCalled {
		t.Error("expected CancelDeferredDeliveries to be called")
	}
	if repo.cancelWPID != "wp_456" {
		t.Errorf("expected wp_456, got %s", repo.cancelWPID)
	}
}

func TestDeliveryManager_RecordAttempt(t *testing.T) {
	repo := &mockDeliveryRepo{}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	err := mgr.RecordAttempt(context.Background(), "del_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !repo.attemptCalled {
		t.Error("expected IncrementAttempt to be called")
	}
	if repo.attemptID != "del_1" {
		t.Errorf("expected del_1, got %s", repo.attemptID)
	}
}

func TestDeliveryManager_RecordAttempt_Error(t *testing.T) {
	repo := &mockDeliveryRepo{
		attemptErr: errors.New("db timeout"),
	}
	mgr := NewDeliveryManager(repo, EmailRetryPolicy, &mockLogger{})

	err := mgr.RecordAttempt(context.Background(), "del_1")
	if err == nil {
		t.Fatal("expected error")
	}
}
