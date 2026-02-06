package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"watchpoint/internal/scheduler"
)

// =============================================================================
// Mock implementations for all service interfaces
// =============================================================================

// mockCleanup records which methods were called and their arguments.
type mockCleanup struct {
	purgeSoftDeletedOrgsCalled bool
	purgeExpiredInvitesCalled  bool
	purgeArchivedWPCalled      bool
	purgeNotificationsCalled   bool
	archiveAuditLogsCalled     bool
	purgeIdempotencyKeysCalled bool
	purgeSecurityEventsCalled  bool
	returnItems                int
	returnErr                  error
}

func (m *mockCleanup) PurgeSoftDeletedOrgs(_ context.Context, _ time.Time, _ time.Duration, _ int) (int, error) {
	m.purgeSoftDeletedOrgsCalled = true
	return m.returnItems, m.returnErr
}

func (m *mockCleanup) PurgeExpiredInvites(_ context.Context, _ time.Time) (int, error) {
	m.purgeExpiredInvitesCalled = true
	return m.returnItems, m.returnErr
}

func (m *mockCleanup) PurgeArchivedWatchPoints(_ context.Context, _ time.Time, _ time.Duration, _ int) (int, error) {
	m.purgeArchivedWPCalled = true
	return m.returnItems, m.returnErr
}

func (m *mockCleanup) PurgeNotifications(_ context.Context, _ time.Duration) (int, error) {
	m.purgeNotificationsCalled = true
	return m.returnItems, m.returnErr
}

func (m *mockCleanup) ArchiveAuditLogs(_ context.Context, _ time.Duration, _ int) (int, error) {
	m.archiveAuditLogsCalled = true
	return m.returnItems, m.returnErr
}

func (m *mockCleanup) PurgeExpiredIdempotencyKeys(_ context.Context, _ time.Time) (int, error) {
	m.purgeIdempotencyKeysCalled = true
	return m.returnItems, m.returnErr
}

func (m *mockCleanup) PurgeSecurityEvents(_ context.Context, _ time.Time, _ time.Duration) (int, error) {
	m.purgeSecurityEventsCalled = true
	return m.returnItems, m.returnErr
}

type mockArchiver struct {
	called      bool
	returnCount int64
	returnErr   error
}

func (m *mockArchiver) ArchiveExpired(_ context.Context, _ time.Time, _ time.Duration) (int64, error) {
	m.called = true
	return m.returnCount, m.returnErr
}

type mockTierTransition struct {
	called      bool
	returnCount int
	returnErr   error
}

func (m *mockTierTransition) EnforceRetention(_ context.Context, _ time.Time) (int, error) {
	m.called = true
	return m.returnCount, m.returnErr
}

type mockDeferredNotification struct {
	called      bool
	returnCount int
	returnErr   error
}

func (m *mockDeferredNotification) RequeueDeferredNotifications(_ context.Context, _ time.Time, _ int) (int, error) {
	m.called = true
	return m.returnCount, m.returnErr
}

type mockDigest struct {
	called      bool
	returnCount int
	returnErr   error
}

func (m *mockDigest) TriggerDigests(_ context.Context, _ time.Time) (int, error) {
	m.called = true
	return m.returnCount, m.returnErr
}

type mockUsage struct {
	called      bool
	returnCount int
	returnErr   error
}

func (m *mockUsage) SnapshotDailyUsage(_ context.Context, _ time.Time) (int, error) {
	m.called = true
	return m.returnCount, m.returnErr
}

type mockVerification struct {
	called      bool
	returnCount int
	returnErr   error
}

func (m *mockVerification) TriggerVerification(_ context.Context, _, _ time.Time) (int, error) {
	m.called = true
	return m.returnCount, m.returnErr
}

type mockReconciler struct {
	called      bool
	returnCount int64
	returnErr   error
}

func (m *mockReconciler) ReconcileStaleRuns(_ context.Context, _ time.Time, _ time.Duration) (int64, error) {
	m.called = true
	return m.returnCount, m.returnErr
}

type mockStripeSyncer struct {
	called      bool
	returnCount int
	returnErr   error
}

func (m *mockStripeSyncer) SyncAtRisk(_ context.Context, _ time.Time, _ time.Duration, _ int) (int, error) {
	m.called = true
	return m.returnCount, m.returnErr
}

type mockEnforcer struct {
	paymentCalled bool
	overageCalled bool
	returnCount   int
	returnErr     error
}

func (m *mockEnforcer) EnforcePaymentFailure(_ context.Context, _ time.Time) (int, error) {
	m.paymentCalled = true
	return m.returnCount, m.returnErr
}

func (m *mockEnforcer) EnforceOverage(_ context.Context, _ time.Time) (int, error) {
	m.overageCalled = true
	return m.returnCount, m.returnErr
}

// mockJobLocker returns a configurable lock acquisition result.
type mockJobLocker struct {
	acquired  bool
	acquireErr error
	lastLockID string
}

func (m *mockJobLocker) Acquire(_ context.Context, lockID string, _ string, _ time.Duration) (bool, error) {
	m.lastLockID = lockID
	return m.acquired, m.acquireErr
}

// mockJobHistorian tracks Start/Finish calls.
type mockJobHistorian struct {
	startCalled  bool
	finishCalled bool
	lastJobType  string
	lastStatus   string
	lastItems    int
	returnID     int64
	startErr     error
	finishErr    error
}

func (m *mockJobHistorian) Start(_ context.Context, jobType string) (int64, error) {
	m.startCalled = true
	m.lastJobType = jobType
	return m.returnID, m.startErr
}

func (m *mockJobHistorian) Finish(_ context.Context, _ int64, status string, items int, _ error) error {
	m.finishCalled = true
	m.lastStatus = status
	m.lastItems = items
	return m.finishErr
}

// =============================================================================
// Helper to build a fully-wired handler with all mock services
// =============================================================================

type testServices struct {
	cleanup              *mockCleanup
	archiver             *mockArchiver
	tierTransition       *mockTierTransition
	deferredNotification *mockDeferredNotification
	digest               *mockDigest
	usage                *mockUsage
	verification         *mockVerification
	reconciler           *mockReconciler
	stripeSyncer         *mockStripeSyncer
	enforcer             *mockEnforcer
	jobLocker            *mockJobLocker
	jobHistorian         *mockJobHistorian
}

func newTestHandler() (*Handler, *testServices) {
	ts := &testServices{
		cleanup:              &mockCleanup{returnItems: 3},
		archiver:             &mockArchiver{returnCount: 5},
		tierTransition:       &mockTierTransition{returnCount: 10},
		deferredNotification: &mockDeferredNotification{returnCount: 7},
		digest:               &mockDigest{returnCount: 12},
		usage:                &mockUsage{returnCount: 8},
		verification:         &mockVerification{returnCount: 1},
		reconciler:           &mockReconciler{returnCount: 4},
		stripeSyncer:         &mockStripeSyncer{returnCount: 6},
		enforcer:             &mockEnforcer{returnCount: 2},
		jobLocker:            &mockJobLocker{acquired: true},
		jobHistorian:         &mockJobHistorian{returnID: 42},
	}

	h := &Handler{
		Services: ServiceRegistry{
			Cleanup:              ts.cleanup,
			Archiver:             ts.archiver,
			TierTransition:       ts.tierTransition,
			DeferredNotification: ts.deferredNotification,
			Digest:               ts.digest,
			Usage:                ts.usage,
			Verification:         ts.verification,
			Reconciler:           ts.reconciler,
			StripeSyncer:         ts.stripeSyncer,
			Enforcer:             ts.enforcer,
		},
		JobLock:    ts.jobLocker,
		JobHistory: ts.jobHistorian,
		WorkerID:   "test-worker-001",
		Logger:     nil, // Uses slog.Default() in handler
	}

	return h, ts
}

// =============================================================================
// Routing Tests
// =============================================================================

func TestHandle_RoutesArchiveWatchPoints(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	result, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskArchiveWatchPoints,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.archiver.called {
		t.Error("expected ArchiverService.ArchiveExpired to be called")
	}
	if !strings.Contains(result, "archive_watchpoints") {
		t.Errorf("result should mention task name, got: %s", result)
	}
}

func TestHandle_RoutesRequeueDeferredNotifs(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	result, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskRequeueDeferredNotifs,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.deferredNotification.called {
		t.Error("expected DeferredNotificationService.RequeueDeferredNotifications to be called")
	}
	if !strings.Contains(result, "7 items") {
		t.Errorf("result should mention item count, got: %s", result)
	}
}

func TestHandle_RoutesCleanupSoftDeletes(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskCleanupSoftDeletes,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.cleanup.purgeSoftDeletedOrgsCalled {
		t.Error("expected PurgeSoftDeletedOrgs to be called")
	}
	if !ts.cleanup.purgeExpiredInvitesCalled {
		t.Error("expected PurgeExpiredInvites to be called")
	}
	if !ts.cleanup.purgeArchivedWPCalled {
		t.Error("expected PurgeArchivedWatchPoints to be called")
	}
	if !ts.cleanup.purgeNotificationsCalled {
		t.Error("expected PurgeNotifications to be called")
	}
	if !ts.cleanup.archiveAuditLogsCalled {
		t.Error("expected ArchiveAuditLogs to be called")
	}
}

func TestHandle_RoutesCleanupIdempotencyKeys(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskCleanupIdempotencyKeys,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.cleanup.purgeIdempotencyKeysCalled {
		t.Error("expected PurgeExpiredIdempotencyKeys to be called")
	}
}

func TestHandle_RoutesCleanupSecurityEvents(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskCleanupSecurityEvents,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.cleanup.purgeSecurityEventsCalled {
		t.Error("expected PurgeSecurityEvents to be called")
	}
}

func TestHandle_RoutesTriggerDigests(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskTriggerDigests,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.digest.called {
		t.Error("expected DigestScheduler.TriggerDigests to be called")
	}
}

func TestHandle_RoutesAggregateUsage(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskAggregateUsage,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.usage.called {
		t.Error("expected UsageAggregator.SnapshotDailyUsage to be called")
	}
}

func TestHandle_RoutesSyncStripe(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskSyncStripe,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.stripeSyncer.called {
		t.Error("expected StripeSyncer.SyncAtRisk to be called")
	}
	if !ts.enforcer.paymentCalled {
		t.Error("expected SubscriptionEnforcer.EnforcePaymentFailure to be called")
	}
	if !ts.enforcer.overageCalled {
		t.Error("expected SubscriptionEnforcer.EnforceOverage to be called")
	}
}

func TestHandle_RoutesVerification(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskVerification,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.verification.called {
		t.Error("expected VerificationService.TriggerVerification to be called")
	}
}

func TestHandle_RoutesReconcileForecasts(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskReconcileForecasts,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.reconciler.called {
		t.Error("expected ForecastReconciler.ReconcileStaleRuns to be called")
	}
}

func TestHandle_RoutesForecastTier(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskForecastTier,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.tierTransition.called {
		t.Error("expected TierTransitionService.EnforceRetention to be called")
	}
}

// =============================================================================
// Lock Behavior Tests
// =============================================================================

func TestHandle_SkipsWhenLockNotAcquired(t *testing.T) {
	h, ts := newTestHandler()
	ts.jobLocker.acquired = false
	ctx := context.Background()

	result, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskArchiveWatchPoints,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "skipped") {
		t.Errorf("expected skip message, got: %s", result)
	}
	if ts.archiver.called {
		t.Error("service should not be called when lock is not acquired")
	}
	if ts.jobHistorian.startCalled {
		t.Error("job history should not be started when lock is not acquired")
	}
}

func TestHandle_ReturnsErrorWhenLockFails(t *testing.T) {
	h, ts := newTestHandler()
	ts.jobLocker.acquireErr = errors.New("database connection lost")
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskArchiveWatchPoints,
	})

	if err == nil {
		t.Fatal("expected error when lock acquisition fails")
	}
	if !strings.Contains(err.Error(), "acquiring job lock") {
		t.Errorf("error should mention lock acquisition, got: %v", err)
	}
}

func TestHandle_LockIDFormatIsCorrect(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	refTime := time.Date(2026, 2, 6, 3, 15, 30, 0, time.UTC)
	_, _ = h.Handle(ctx, scheduler.MaintenancePayload{
		Task:          scheduler.TaskArchiveWatchPoints,
		ReferenceTime: &refTime,
	})

	// Lock ID should be "task:truncated_hour".
	expected := "archive_watchpoints:2026-02-06T03"
	if ts.jobLocker.lastLockID != expected {
		t.Errorf("lock ID = %q, want %q", ts.jobLocker.lastLockID, expected)
	}
}

// =============================================================================
// Reference Time Tests
// =============================================================================

func TestHandle_UsesReferenceTimeWhenProvided(t *testing.T) {
	h, _ := newTestHandler()
	ctx := context.Background()

	refTime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	result, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task:          scheduler.TaskTriggerDigests,
		ReferenceTime: &refTime,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

// =============================================================================
// Error Handling Tests
// =============================================================================

func TestHandle_EmptyTaskTypeReturnsError(t *testing.T) {
	h, _ := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: "",
	})

	if err == nil {
		t.Fatal("expected error for empty task type")
	}
	if !strings.Contains(err.Error(), "empty task type") {
		t.Errorf("error should mention empty task type, got: %v", err)
	}
}

func TestHandle_UnknownTaskTypeReturnsError(t *testing.T) {
	h, _ := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: "nonexistent_task",
	})

	if err == nil {
		t.Fatal("expected error for unknown task type")
	}
	if !strings.Contains(err.Error(), "unknown task type") {
		t.Errorf("error should mention unknown task, got: %v", err)
	}
}

func TestHandle_ServiceErrorRecordedInHistory(t *testing.T) {
	h, ts := newTestHandler()
	ts.archiver.returnErr = errors.New("database timeout")
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskArchiveWatchPoints,
	})

	if err == nil {
		t.Fatal("expected error from service failure")
	}
	if !ts.jobHistorian.finishCalled {
		t.Error("expected job history Finish to be called even on error")
	}
	if ts.jobHistorian.lastStatus != "failed" {
		t.Errorf("job history status = %q, want %q", ts.jobHistorian.lastStatus, "failed")
	}
}

func TestHandle_SuccessRecordedInHistory(t *testing.T) {
	h, ts := newTestHandler()
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskArchiveWatchPoints,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.jobHistorian.finishCalled {
		t.Error("expected job history Finish to be called on success")
	}
	if ts.jobHistorian.lastStatus != "success" {
		t.Errorf("job history status = %q, want %q", ts.jobHistorian.lastStatus, "success")
	}
	if ts.jobHistorian.lastItems != 5 {
		t.Errorf("job history items = %d, want %d", ts.jobHistorian.lastItems, 5)
	}
}

func TestHandle_JobHistoryStartFailureIsNonFatal(t *testing.T) {
	h, ts := newTestHandler()
	ts.jobHistorian.startErr = errors.New("history db error")
	ctx := context.Background()

	// Should still execute the task successfully.
	result, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskArchiveWatchPoints,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ts.archiver.called {
		t.Error("service should still be called when history start fails")
	}
	if ts.jobHistorian.finishCalled {
		t.Error("Finish should not be called when Start failed (jobID=0)")
	}
	if !strings.Contains(result, "complete") {
		t.Errorf("result should indicate completion, got: %s", result)
	}
}

// =============================================================================
// Comprehensive routing verification
// =============================================================================

func TestHandle_AllTaskTypesRouteCorrectly(t *testing.T) {
	// This test verifies that all defined TaskType constants are handled
	// by the multiplexer without returning an "unknown task type" error.
	allTasks := []scheduler.TaskType{
		scheduler.TaskArchiveWatchPoints,
		scheduler.TaskRequeueDeferredNotifs,
		scheduler.TaskCleanupSoftDeletes,
		scheduler.TaskCleanupIdempotencyKeys,
		scheduler.TaskCleanupSecurityEvents,
		scheduler.TaskTriggerDigests,
		scheduler.TaskSyncStripe,
		scheduler.TaskAggregateUsage,
		scheduler.TaskVerification,
		scheduler.TaskReconcileForecasts,
		scheduler.TaskForecastTier,
	}

	for _, task := range allTasks {
		t.Run(string(task), func(t *testing.T) {
			h, _ := newTestHandler()
			ctx := context.Background()

			_, err := h.Handle(ctx, scheduler.MaintenancePayload{
				Task: task,
			})

			if err != nil {
				t.Errorf("task %q returned unexpected error: %v", task, err)
			}
		})
	}
}

// =============================================================================
// TaskType and MaintenancePayload type tests
// =============================================================================

func TestTaskTypeConstants_MatchArchitectureSpec(t *testing.T) {
	// Verify all constants defined in 09-scheduled-jobs.md Section 6.1 exist
	// and have the correct string values.
	tests := []struct {
		constant scheduler.TaskType
		expected string
	}{
		{scheduler.TaskArchiveWatchPoints, "archive_watchpoints"},
		{scheduler.TaskRequeueDeferredNotifs, "requeue_deferred"},
		{scheduler.TaskCleanupSoftDeletes, "cleanup_soft_deletes"},
		{scheduler.TaskCleanupIdempotencyKeys, "cleanup_idempotency_keys"},
		{scheduler.TaskCleanupSecurityEvents, "cleanup_security_events"},
		{scheduler.TaskTriggerDigests, "trigger_digests"},
		{scheduler.TaskSyncStripe, "sync_stripe"},
		{scheduler.TaskAggregateUsage, "aggregate_usage"},
		{scheduler.TaskVerification, "verification"},
		{scheduler.TaskReconcileForecasts, "reconcile_forecasts"},
		{scheduler.TaskForecastTier, "transition_forecasts"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if string(tt.constant) != tt.expected {
				t.Errorf("TaskType = %q, want %q", tt.constant, tt.expected)
			}
		})
	}
}

func TestMaintenancePayload_NilReferenceTime(t *testing.T) {
	payload := scheduler.MaintenancePayload{
		Task: scheduler.TaskArchiveWatchPoints,
	}
	if payload.ReferenceTime != nil {
		t.Error("ReferenceTime should be nil by default")
	}
}

func TestMaintenancePayload_WithReferenceTime(t *testing.T) {
	refTime := time.Date(2026, 2, 6, 3, 0, 0, 0, time.UTC)
	payload := scheduler.MaintenancePayload{
		Task:          scheduler.TaskArchiveWatchPoints,
		ReferenceTime: &refTime,
	}
	if payload.ReferenceTime == nil {
		t.Fatal("ReferenceTime should not be nil")
	}
	if !payload.ReferenceTime.Equal(refTime) {
		t.Errorf("ReferenceTime = %v, want %v", *payload.ReferenceTime, refTime)
	}
}

// =============================================================================
// Integration-like test for cleanup_soft_deletes aggregation
// =============================================================================

func TestHandle_CleanupSoftDeletesAggregatesItemCounts(t *testing.T) {
	h, ts := newTestHandler()
	// Each sub-operation returns 3 items; 5 operations total => 15.
	ctx := context.Background()

	result, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskCleanupSoftDeletes,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Total should be 3*5=15 (5 cleanup operations, each returning 3).
	expectedItems := "15 items"
	if !strings.Contains(result, expectedItems) {
		t.Errorf("result should contain %q, got: %s", expectedItems, result)
	}
	if ts.jobHistorian.lastItems != 15 {
		t.Errorf("job history items = %d, want 15", ts.jobHistorian.lastItems)
	}
}

// =============================================================================
// Integration-like test for sync_stripe aggregation
// =============================================================================

func TestHandle_SyncStripeAggregatesAllEnforcement(t *testing.T) {
	h, _ := newTestHandler()
	// StripeSyncer returns 6, Enforcer returns 2 for each method.
	ctx := context.Background()

	result, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskSyncStripe,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Total: 6 (sync) + 2 (payment) + 2 (overage) = 10.
	expectedItems := "10 items"
	if !strings.Contains(result, expectedItems) {
		t.Errorf("result should contain %q, got: %s", expectedItems, result)
	}
}

func TestHandle_SyncStripeStopsOnSyncError(t *testing.T) {
	h, mocks := newTestHandler()
	mocks.stripeSyncer.returnErr = fmt.Errorf("stripe API error")
	ctx := context.Background()

	_, err := h.Handle(ctx, scheduler.MaintenancePayload{
		Task: scheduler.TaskSyncStripe,
	})

	if err == nil {
		t.Fatal("expected error from sync failure")
	}
	// Enforcer should NOT have been called since sync failed first.
	if mocks.enforcer.paymentCalled {
		t.Error("enforcer should not be called when sync fails")
	}
}
