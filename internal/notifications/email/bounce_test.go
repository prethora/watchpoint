package email

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// --- Mock Dependencies ---

// mockDeliveryLookup implements DeliveryLookup for testing.
type mockDeliveryLookup struct {
	findResult *DeliveryInfo
	findErr    error

	updateBouncedCalls []updateBouncedCall
	updateBouncedErr   error
}

type updateBouncedCall struct {
	DeliveryID string
	Reason     string
}

func (m *mockDeliveryLookup) FindDeliveryByProviderMsgID(ctx context.Context, providerMsgID string) (*DeliveryInfo, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	return m.findResult, nil
}

func (m *mockDeliveryLookup) UpdateDeliveryBounced(ctx context.Context, deliveryID string, reason string) error {
	m.updateBouncedCalls = append(m.updateBouncedCalls, updateBouncedCall{
		DeliveryID: deliveryID,
		Reason:     reason,
	})
	return m.updateBouncedErr
}

// mockChannelHealthRepo implements ChannelHealthRepository for testing.
type mockChannelHealthRepo struct {
	// failureCounts tracks the current failure count per watchpoint+channel.
	// Key: "wpID:channelIdx"
	failureCounts map[string]int

	incrementCalls []incrementCall
	incrementErr   error

	disableCalls []disableCall
	disableErr   error

	resetCalls []resetCall
	resetErr   error
}

type incrementCall struct {
	WpID       string
	ChannelIdx int
}

type disableCall struct {
	WpID       string
	ChannelIdx int
	Reason     string
}

type resetCall struct {
	WpID      string
	ChannelID string
}

func newMockChannelHealthRepo() *mockChannelHealthRepo {
	return &mockChannelHealthRepo{
		failureCounts: make(map[string]int),
	}
}

func (m *mockChannelHealthRepo) IncrementChannelFailure(ctx context.Context, wpID string, channelIdx int) (int, error) {
	m.incrementCalls = append(m.incrementCalls, incrementCall{WpID: wpID, ChannelIdx: channelIdx})
	if m.incrementErr != nil {
		return 0, m.incrementErr
	}

	key := fmt.Sprintf("%s:%d", wpID, channelIdx)
	m.failureCounts[key]++
	return m.failureCounts[key], nil
}

func (m *mockChannelHealthRepo) DisableChannel(ctx context.Context, wpID string, channelIdx int, reason string) error {
	m.disableCalls = append(m.disableCalls, disableCall{WpID: wpID, ChannelIdx: channelIdx, Reason: reason})
	return m.disableErr
}

func (m *mockChannelHealthRepo) ResetChannelFailureCount(ctx context.Context, wpID string, channelID string) error {
	m.resetCalls = append(m.resetCalls, resetCall{WpID: wpID, ChannelID: channelID})
	return m.resetErr
}

// mockUserEmailLookup implements UserEmailLookup for testing.
type mockUserEmailLookup struct {
	email string
	err   error
}

func (m *mockUserEmailLookup) GetOwnerEmail(ctx context.Context, orgID string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.email, nil
}

// --- BounceProcessor.Process Unit Tests ---

func TestBounceProcessor_Process_HardBounce_BelowThreshold(t *testing.T) {
	// A single hard bounce should increment the failure count but NOT disable.
	deliveryLookup := &mockDeliveryLookup{
		findResult: &DeliveryInfo{
			DeliveryID:     "del-1",
			NotificationID: "notif-1",
			WatchPointID:   "wp-1",
			OrganizationID: "org-1",
			ChannelIndex:   0,
		},
	}
	healthRepo := newMockChannelHealthRepo()
	logger := newTestLogger()

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: deliveryLookup,
		HealthRepo:     healthRepo,
		UserLookup:     &mockUserEmailLookup{email: "owner@test.com"},
		EmailProvider:  &mockEmailProvider{sendMsgID: "alert-1"},
		Templates:      &mockTemplateService{resolveID: "d-system-alert"},
		Logger:         logger,
	})

	event := BounceEvent{
		ProviderMessageID: "pmsg-1",
		EmailAddress:      "bad@example.com",
		Reason:            "mailbox full",
		Type:              FeedbackBounce,
		Timestamp:         time.Now(),
	}

	err := bp.Process(context.Background(), event)
	if err != nil {
		t.Fatalf("Process() error: %v", err)
	}

	// Delivery should have been marked as bounced.
	if len(deliveryLookup.updateBouncedCalls) != 1 {
		t.Fatalf("expected 1 UpdateDeliveryBounced call, got %d", len(deliveryLookup.updateBouncedCalls))
	}
	if deliveryLookup.updateBouncedCalls[0].DeliveryID != "del-1" {
		t.Errorf("UpdateDeliveryBounced deliveryID = %q, want del-1", deliveryLookup.updateBouncedCalls[0].DeliveryID)
	}
	if deliveryLookup.updateBouncedCalls[0].Reason != "mailbox full" {
		t.Errorf("UpdateDeliveryBounced reason = %q, want 'mailbox full'", deliveryLookup.updateBouncedCalls[0].Reason)
	}

	// Failure count should have been incremented.
	if len(healthRepo.incrementCalls) != 1 {
		t.Fatalf("expected 1 IncrementChannelFailure call, got %d", len(healthRepo.incrementCalls))
	}

	// Channel should NOT be disabled (only 1 failure, threshold is 3).
	if len(healthRepo.disableCalls) != 0 {
		t.Errorf("expected 0 DisableChannel calls (below threshold), got %d", len(healthRepo.disableCalls))
	}
}

func TestBounceProcessor_Process_SpamComplaint_ImmediateDisable(t *testing.T) {
	// A spam complaint should immediately disable the channel.
	deliveryLookup := &mockDeliveryLookup{
		findResult: &DeliveryInfo{
			DeliveryID:     "del-2",
			NotificationID: "notif-2",
			WatchPointID:   "wp-2",
			OrganizationID: "org-2",
			ChannelIndex:   1,
		},
	}
	healthRepo := newMockChannelHealthRepo()
	provider := &mockEmailProvider{sendMsgID: "alert-msg"}

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: deliveryLookup,
		HealthRepo:     healthRepo,
		UserLookup:     &mockUserEmailLookup{email: "owner@org2.com"},
		EmailProvider:  provider,
		Templates:      &mockTemplateService{resolveID: "d-system-alert"},
		Logger:         newTestLogger(),
	})

	event := BounceEvent{
		ProviderMessageID: "pmsg-2",
		EmailAddress:      "complainer@example.com",
		Reason:            "marked as spam",
		Type:              FeedbackComplaint,
		Timestamp:         time.Now(),
	}

	err := bp.Process(context.Background(), event)
	if err != nil {
		t.Fatalf("Process() error: %v", err)
	}

	// Delivery should have been marked as bounced.
	if len(deliveryLookup.updateBouncedCalls) != 1 {
		t.Fatalf("expected 1 UpdateDeliveryBounced call, got %d", len(deliveryLookup.updateBouncedCalls))
	}

	// Channel should be disabled IMMEDIATELY with "spam_complaint" reason.
	if len(healthRepo.disableCalls) != 1 {
		t.Fatalf("expected 1 DisableChannel call for complaint, got %d", len(healthRepo.disableCalls))
	}
	if healthRepo.disableCalls[0].Reason != "spam_complaint" {
		t.Errorf("DisableChannel reason = %q, want spam_complaint", healthRepo.disableCalls[0].Reason)
	}
	if healthRepo.disableCalls[0].WpID != "wp-2" {
		t.Errorf("DisableChannel wpID = %q, want wp-2", healthRepo.disableCalls[0].WpID)
	}
	if healthRepo.disableCalls[0].ChannelIdx != 1 {
		t.Errorf("DisableChannel channelIdx = %d, want 1", healthRepo.disableCalls[0].ChannelIdx)
	}

	// IncrementChannelFailure should NOT have been called for complaints.
	if len(healthRepo.incrementCalls) != 0 {
		t.Errorf("expected 0 IncrementChannelFailure calls for complaint, got %d", len(healthRepo.incrementCalls))
	}

	// Owner should have been notified.
	if !provider.sendCalled {
		t.Error("expected owner notification email to be sent")
	}
	if provider.sendInput.To != "owner@org2.com" {
		t.Errorf("notification To = %q, want owner@org2.com", provider.sendInput.To)
	}
}

func TestBounceProcessor_Process_FindDeliveryError(t *testing.T) {
	deliveryLookup := &mockDeliveryLookup{
		findErr: errors.New("delivery not found"),
	}

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: deliveryLookup,
		HealthRepo:     newMockChannelHealthRepo(),
		UserLookup:     &mockUserEmailLookup{},
		EmailProvider:  &mockEmailProvider{},
		Templates:      &mockTemplateService{},
		Logger:         newTestLogger(),
	})

	err := bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "unknown",
		Type:              FeedbackBounce,
	})
	if err == nil {
		t.Fatal("expected error when delivery lookup fails")
	}
}

func TestBounceProcessor_Process_UpdateDeliveryError(t *testing.T) {
	deliveryLookup := &mockDeliveryLookup{
		findResult: &DeliveryInfo{
			DeliveryID:     "del-3",
			NotificationID: "notif-3",
			WatchPointID:   "wp-3",
			OrganizationID: "org-3",
			ChannelIndex:   0,
		},
		updateBouncedErr: errors.New("db error"),
	}

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: deliveryLookup,
		HealthRepo:     newMockChannelHealthRepo(),
		UserLookup:     &mockUserEmailLookup{},
		EmailProvider:  &mockEmailProvider{},
		Templates:      &mockTemplateService{},
		Logger:         newTestLogger(),
	})

	err := bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-3",
		Type:              FeedbackBounce,
		Reason:            "test",
	})
	if err == nil {
		t.Fatal("expected error when update delivery fails")
	}
}

func TestBounceProcessor_Process_IncrementFailureError(t *testing.T) {
	deliveryLookup := &mockDeliveryLookup{
		findResult: &DeliveryInfo{
			DeliveryID:     "del-4",
			NotificationID: "notif-4",
			WatchPointID:   "wp-4",
			OrganizationID: "org-4",
			ChannelIndex:   0,
		},
	}

	healthRepo := newMockChannelHealthRepo()
	healthRepo.incrementErr = errors.New("optimistic lock failed")

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: deliveryLookup,
		HealthRepo:     healthRepo,
		UserLookup:     &mockUserEmailLookup{},
		EmailProvider:  &mockEmailProvider{},
		Templates:      &mockTemplateService{},
		Logger:         newTestLogger(),
	})

	err := bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-4",
		Type:              FeedbackBounce,
		Reason:            "mailbox not found",
	})
	if err == nil {
		t.Fatal("expected error when increment failure fails")
	}
}

func TestBounceProcessor_Process_DisableChannelError_Bounce(t *testing.T) {
	deliveryLookup := &mockDeliveryLookup{
		findResult: &DeliveryInfo{
			DeliveryID:     "del-5",
			NotificationID: "notif-5",
			WatchPointID:   "wp-5",
			OrganizationID: "org-5",
			ChannelIndex:   0,
		},
	}

	healthRepo := newMockChannelHealthRepo()
	// Pre-set the failure count to 2 so the next increment reaches 3.
	healthRepo.failureCounts["wp-5:0"] = 2
	healthRepo.disableErr = errors.New("disable failed")

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: deliveryLookup,
		HealthRepo:     healthRepo,
		UserLookup:     &mockUserEmailLookup{},
		EmailProvider:  &mockEmailProvider{},
		Templates:      &mockTemplateService{},
		Logger:         newTestLogger(),
	})

	err := bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-5",
		Type:              FeedbackBounce,
		Reason:            "mailbox not found",
	})
	if err == nil {
		t.Fatal("expected error when disable channel fails")
	}
}

func TestBounceProcessor_Process_OwnerNotificationFailure_NonFatal(t *testing.T) {
	// Owner notification failure should be logged but NOT fail the overall process.
	deliveryLookup := &mockDeliveryLookup{
		findResult: &DeliveryInfo{
			DeliveryID:     "del-6",
			NotificationID: "notif-6",
			WatchPointID:   "wp-6",
			OrganizationID: "org-6",
			ChannelIndex:   0,
		},
	}

	healthRepo := newMockChannelHealthRepo()
	logger := newTestLogger()

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: deliveryLookup,
		HealthRepo:     healthRepo,
		// UserLookup fails - owner email cannot be found.
		UserLookup:    &mockUserEmailLookup{err: errors.New("no owner found")},
		EmailProvider: &mockEmailProvider{},
		Templates:     &mockTemplateService{resolveID: "d-system"},
		Logger:        logger,
	})

	event := BounceEvent{
		ProviderMessageID: "pmsg-6",
		EmailAddress:      "complainer@example.com",
		Reason:            "spam",
		Type:              FeedbackComplaint,
		Timestamp:         time.Now(),
	}

	err := bp.Process(context.Background(), event)
	if err != nil {
		t.Fatalf("Process() should not fail when owner notification fails, got: %v", err)
	}

	// Channel should still be disabled.
	if len(healthRepo.disableCalls) != 1 {
		t.Errorf("expected 1 DisableChannel call, got %d", len(healthRepo.disableCalls))
	}

	// Error should have been logged.
	if len(logger.errors) == 0 {
		t.Error("expected error log for failed owner notification")
	}
}

func TestBounceProcessor_Process_OwnerNotification_TemplateResolutionFailure(t *testing.T) {
	// Template resolution failure for owner notification should be non-fatal.
	deliveryLookup := &mockDeliveryLookup{
		findResult: &DeliveryInfo{
			DeliveryID:     "del-7",
			NotificationID: "notif-7",
			WatchPointID:   "wp-7",
			OrganizationID: "org-7",
			ChannelIndex:   0,
		},
	}

	healthRepo := newMockChannelHealthRepo()
	logger := newTestLogger()

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: deliveryLookup,
		HealthRepo:     healthRepo,
		UserLookup:     &mockUserEmailLookup{email: "owner@org7.com"},
		EmailProvider:  &mockEmailProvider{sendMsgID: "alert-msg"},
		Templates:      &mockTemplateService{resolveErr: errors.New("template not found")},
		Logger:         logger,
	})

	err := bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-7",
		EmailAddress:      "bad@example.com",
		Reason:            "spam",
		Type:              FeedbackComplaint,
	})
	if err != nil {
		t.Fatalf("Process() should not fail when owner notification template fails, got: %v", err)
	}

	// Error should have been logged.
	if len(logger.errors) == 0 {
		t.Error("expected error log for failed template resolution")
	}
}

func TestBounceProcessor_Process_OwnerNotification_SendFailure(t *testing.T) {
	// Email send failure for owner notification should be non-fatal.
	deliveryLookup := &mockDeliveryLookup{
		findResult: &DeliveryInfo{
			DeliveryID:     "del-8",
			NotificationID: "notif-8",
			WatchPointID:   "wp-8",
			OrganizationID: "org-8",
			ChannelIndex:   0,
		},
	}

	healthRepo := newMockChannelHealthRepo()
	logger := newTestLogger()

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: deliveryLookup,
		HealthRepo:     healthRepo,
		UserLookup:     &mockUserEmailLookup{email: "owner@org8.com"},
		EmailProvider:  &mockEmailProvider{sendErr: errors.New("send grid down")},
		Templates:      &mockTemplateService{resolveID: "d-system"},
		Logger:         logger,
	})

	err := bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-8",
		EmailAddress:      "bad@example.com",
		Reason:            "spam",
		Type:              FeedbackComplaint,
	})
	if err != nil {
		t.Fatalf("Process() should not fail when owner notification send fails, got: %v", err)
	}

	// Error should have been logged.
	if len(logger.errors) == 0 {
		t.Error("expected error log for failed email send")
	}
}

// --- Integration Test: 3 Bounces Disables Channel ---

// TestBounceProcessor_Integration_ThreeBouncesDisablesChannel simulates the
// full flow of 3 hard bounces to the same channel, verifying that:
//   - Each bounce increments the failure count
//   - The channel is disabled after the 3rd bounce
//   - The owner is notified only once (when disabled)
//
// This is the primary definition-of-done test for this task.
func TestBounceProcessor_Integration_ThreeBouncesDisablesChannel(t *testing.T) {
	const wpID = "wp-int-1"
	const orgID = "org-int-1"

	// Use a stateful health repo that tracks failure counts across calls.
	healthRepo := newMockChannelHealthRepo()

	// Track all provider.Send calls (for owner notification).
	provider := &mockEmailProvider{sendMsgID: "alert-msg-id"}

	logger := newTestLogger()

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: &mockDeliveryLookup{
			findResult: &DeliveryInfo{
				DeliveryID:     "del-int-1",
				NotificationID: "notif-int-1",
				WatchPointID:   wpID,
				OrganizationID: orgID,
				ChannelIndex:   0,
			},
		},
		HealthRepo:    healthRepo,
		UserLookup:    &mockUserEmailLookup{email: "owner@integration.com"},
		EmailProvider: provider,
		Templates:     &mockTemplateService{resolveID: "d-system-alert-template"},
		Logger:        logger,
	})

	// --- Bounce 1 ---
	err := bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-b1",
		EmailAddress:      "bad@example.com",
		Reason:            "mailbox not found",
		Type:              FeedbackBounce,
		Timestamp:         time.Now(),
	})
	if err != nil {
		t.Fatalf("Bounce 1: Process() error: %v", err)
	}

	// After 1 bounce: failure count = 1, no disable, no owner notification.
	if len(healthRepo.incrementCalls) != 1 {
		t.Errorf("Bounce 1: expected 1 increment call, got %d", len(healthRepo.incrementCalls))
	}
	if len(healthRepo.disableCalls) != 0 {
		t.Errorf("Bounce 1: expected 0 disable calls, got %d", len(healthRepo.disableCalls))
	}
	if provider.sendCalled {
		t.Error("Bounce 1: owner should NOT have been notified")
	}

	// Reset provider state for next bounce.
	provider.sendCalled = false

	// --- Bounce 2 ---
	err = bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-b2",
		EmailAddress:      "bad@example.com",
		Reason:            "mailbox not found",
		Type:              FeedbackBounce,
		Timestamp:         time.Now(),
	})
	if err != nil {
		t.Fatalf("Bounce 2: Process() error: %v", err)
	}

	// After 2 bounces: failure count = 2, no disable, no owner notification.
	if len(healthRepo.incrementCalls) != 2 {
		t.Errorf("Bounce 2: expected 2 increment calls, got %d", len(healthRepo.incrementCalls))
	}
	if len(healthRepo.disableCalls) != 0 {
		t.Errorf("Bounce 2: expected 0 disable calls, got %d", len(healthRepo.disableCalls))
	}
	if provider.sendCalled {
		t.Error("Bounce 2: owner should NOT have been notified")
	}

	// Reset provider state for next bounce.
	provider.sendCalled = false

	// --- Bounce 3 ---
	err = bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-b3",
		EmailAddress:      "bad@example.com",
		Reason:            "mailbox not found",
		Type:              FeedbackBounce,
		Timestamp:         time.Now(),
	})
	if err != nil {
		t.Fatalf("Bounce 3: Process() error: %v", err)
	}

	// After 3 bounces: failure count = 3, channel DISABLED, owner notified.
	if len(healthRepo.incrementCalls) != 3 {
		t.Errorf("Bounce 3: expected 3 increment calls, got %d", len(healthRepo.incrementCalls))
	}
	if len(healthRepo.disableCalls) != 1 {
		t.Fatalf("Bounce 3: expected 1 disable call after threshold, got %d", len(healthRepo.disableCalls))
	}

	// Verify the disable call details.
	disable := healthRepo.disableCalls[0]
	if disable.WpID != wpID {
		t.Errorf("DisableChannel wpID = %q, want %q", disable.WpID, wpID)
	}
	if disable.ChannelIdx != 0 {
		t.Errorf("DisableChannel channelIdx = %d, want 0", disable.ChannelIdx)
	}
	if disable.Reason != "hard_bounce" {
		t.Errorf("DisableChannel reason = %q, want hard_bounce", disable.Reason)
	}

	// Verify owner was notified.
	if !provider.sendCalled {
		t.Fatal("Bounce 3: owner should have been notified")
	}
	if provider.sendInput.To != "owner@integration.com" {
		t.Errorf("notification To = %q, want owner@integration.com", provider.sendInput.To)
	}
	if provider.sendInput.TemplateID != "d-system-alert-template" {
		t.Errorf("notification TemplateID = %q, want d-system-alert-template", provider.sendInput.TemplateID)
	}

	// Verify template data includes the bounced address.
	if addr, ok := provider.sendInput.TemplateData["bounced_address"].(string); !ok || addr != "bad@example.com" {
		t.Errorf("notification template data bounced_address = %v, want bad@example.com", provider.sendInput.TemplateData["bounced_address"])
	}

	// Verify template data includes the organization ID.
	if oid, ok := provider.sendInput.TemplateData["organization_id"].(string); !ok || oid != orgID {
		t.Errorf("notification template data organization_id = %v, want %s", provider.sendInput.TemplateData["organization_id"], orgID)
	}
}

// TestBounceProcessor_Integration_MixedBounceAndComplaint verifies that a
// complaint after bounces still triggers immediate disable even though the
// bounce count has not reached the threshold.
func TestBounceProcessor_Integration_MixedBounceAndComplaint(t *testing.T) {
	healthRepo := newMockChannelHealthRepo()
	provider := &mockEmailProvider{sendMsgID: "alert-msg"}

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: &mockDeliveryLookup{
			findResult: &DeliveryInfo{
				DeliveryID:     "del-mix-1",
				NotificationID: "notif-mix-1",
				WatchPointID:   "wp-mix-1",
				OrganizationID: "org-mix-1",
				ChannelIndex:   0,
			},
		},
		HealthRepo:    healthRepo,
		UserLookup:    &mockUserEmailLookup{email: "owner@mixed.com"},
		EmailProvider: provider,
		Templates:     &mockTemplateService{resolveID: "d-system"},
		Logger:        newTestLogger(),
	})

	// First: 1 bounce (below threshold).
	err := bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-mix-1",
		EmailAddress:      "user@example.com",
		Reason:            "mailbox full",
		Type:              FeedbackBounce,
		Timestamp:         time.Now(),
	})
	if err != nil {
		t.Fatalf("Bounce: Process() error: %v", err)
	}
	if len(healthRepo.disableCalls) != 0 {
		t.Errorf("after 1 bounce, expected 0 disable calls, got %d", len(healthRepo.disableCalls))
	}

	// Reset provider state.
	provider.sendCalled = false

	// Then: 1 complaint (should immediately disable).
	err = bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-mix-2",
		EmailAddress:      "user@example.com",
		Reason:            "marked as spam",
		Type:              FeedbackComplaint,
		Timestamp:         time.Now(),
	})
	if err != nil {
		t.Fatalf("Complaint: Process() error: %v", err)
	}

	if len(healthRepo.disableCalls) != 1 {
		t.Fatalf("after complaint, expected 1 disable call, got %d", len(healthRepo.disableCalls))
	}
	if healthRepo.disableCalls[0].Reason != "spam_complaint" {
		t.Errorf("disable reason = %q, want spam_complaint", healthRepo.disableCalls[0].Reason)
	}
	if !provider.sendCalled {
		t.Error("owner should have been notified after complaint")
	}
}

func TestBounceProcessor_Process_DisableChannelError_Complaint(t *testing.T) {
	// DisableChannel failure on complaint should propagate the error.
	deliveryLookup := &mockDeliveryLookup{
		findResult: &DeliveryInfo{
			DeliveryID:     "del-9",
			NotificationID: "notif-9",
			WatchPointID:   "wp-9",
			OrganizationID: "org-9",
			ChannelIndex:   0,
		},
	}

	healthRepo := newMockChannelHealthRepo()
	healthRepo.disableErr = errors.New("db connection lost")

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: deliveryLookup,
		HealthRepo:     healthRepo,
		UserLookup:     &mockUserEmailLookup{},
		EmailProvider:  &mockEmailProvider{},
		Templates:      &mockTemplateService{},
		Logger:         newTestLogger(),
	})

	err := bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-9",
		EmailAddress:      "complainer@example.com",
		Reason:            "spam",
		Type:              FeedbackComplaint,
	})
	if err == nil {
		t.Fatal("expected error when DisableChannel fails on complaint")
	}
}

// TestBounceProcessor_Process_OwnerNotificationDetails verifies the exact
// content of the system alert sent to the owner.
func TestBounceProcessor_Process_OwnerNotificationDetails(t *testing.T) {
	provider := &mockEmailProvider{sendMsgID: "alert-verification"}

	bp := NewBounceProcessor(BounceProcessorConfig{
		DeliveryLookup: &mockDeliveryLookup{
			findResult: &DeliveryInfo{
				DeliveryID:     "del-detail",
				NotificationID: "notif-detail",
				WatchPointID:   "wp-detail",
				OrganizationID: "org-detail",
				ChannelIndex:   2,
			},
		},
		HealthRepo:    newMockChannelHealthRepo(),
		UserLookup:    &mockUserEmailLookup{email: "admin@company.com"},
		EmailProvider: provider,
		Templates:     &mockTemplateService{resolveID: "d-sys-alert-v2"},
		Logger:        newTestLogger(),
	})

	err := bp.Process(context.Background(), BounceEvent{
		ProviderMessageID: "pmsg-detail",
		EmailAddress:      "bounced@company.com",
		Reason:            "spam report",
		Type:              FeedbackComplaint,
	})
	if err != nil {
		t.Fatalf("Process() error: %v", err)
	}

	if !provider.sendCalled {
		t.Fatal("expected Send to be called for owner notification")
	}

	// Verify sender identity.
	if provider.sendInput.From.Name != "WatchPoint System" {
		t.Errorf("From.Name = %q, want WatchPoint System", provider.sendInput.From.Name)
	}
	if provider.sendInput.From.Address != "system@watchpoint.io" {
		t.Errorf("From.Address = %q, want system@watchpoint.io", provider.sendInput.From.Address)
	}

	// Verify template data.
	data := provider.sendInput.TemplateData
	if data["bounced_address"] != "bounced@company.com" {
		t.Errorf("bounced_address = %v, want bounced@company.com", data["bounced_address"])
	}
	if data["organization_id"] != "org-detail" {
		t.Errorf("organization_id = %v, want org-detail", data["organization_id"])
	}
	if data["event_type"] != string(types.EventSystemAlert) {
		t.Errorf("event_type = %v, want %s", data["event_type"], types.EventSystemAlert)
	}
}
