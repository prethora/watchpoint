package core

import (
	"context"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// mockClock implements types.Clock for deterministic testing.
type mockClock struct {
	now time.Time
}

func (c *mockClock) Now() time.Time { return c.now }

// mockLogger implements types.Logger as a no-op for tests.
type mockLogger struct{}

func (l *mockLogger) Info(msg string, args ...any)  {}
func (l *mockLogger) Error(msg string, args ...any) {}
func (l *mockLogger) Warn(msg string, args ...any)  {}
func (l *mockLogger) With(args ...any) types.Logger { return l }

func newTestPolicyEngine(now time.Time) *PolicyEngineImpl {
	return NewPolicyEngine(&mockClock{now: now}, &mockLogger{})
}

func TestPolicyEngine_CriticalUrgencyBypassesAll(t *testing.T) {
	// Even during quiet hours, critical urgency must deliver immediately.
	// Set current time to 3 AM Tokyo time (deep in quiet hours).
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 2, 3, 3, 0, 0, 0, tokyo).UTC()

	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_123",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyCritical,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "Asia/Tokyo",
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "22:00", End: "08:00"}, // every day
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver immediately, got %s", result.Decision)
	}
	if result.Reason != "critical urgency bypasses all suppression" {
		t.Errorf("unexpected reason: %s", result.Reason)
	}
}

func TestPolicyEngine_ThresholdClearedBypassesQuietHours(t *testing.T) {
	// threshold_cleared events are time-sensitive and must bypass quiet hours.
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 2, 3, 3, 0, 0, 0, tokyo).UTC()

	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_456",
		EventType: types.EventThresholdCleared,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "Asia/Tokyo",
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "22:00", End: "08:00"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver immediately, got %s", result.Decision)
	}
}

func TestPolicyEngine_QuietHours_DuringQuietPeriod_Defers(t *testing.T) {
	// Simulate NOTIF-002: user in Asia/Tokyo, current time 02:00.
	// Quiet hours 22:00-08:00. Should defer until 08:00 same day.
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 2, 3, 2, 0, 0, 0, tokyo).UTC()

	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_789",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "Asia/Tokyo",
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "22:00", End: "08:00"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDefer {
		t.Fatalf("expected defer, got %s", result.Decision)
	}
	if result.ResumeAt == nil {
		t.Fatal("expected ResumeAt to be set")
	}

	// ResumeAt should be 08:00 on Feb 3 Tokyo time.
	expectedResume := time.Date(2026, 2, 3, 8, 0, 0, 0, tokyo)
	if !result.ResumeAt.Equal(expectedResume) {
		t.Errorf("expected resume at %s, got %s", expectedResume, result.ResumeAt)
	}
}

func TestPolicyEngine_QuietHours_BeforeMidnight_DefersToNextDay(t *testing.T) {
	// Current time 23:00 Tokyo. Quiet hours 22:00-08:00.
	// Should defer until 08:00 next day.
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 2, 3, 23, 0, 0, 0, tokyo).UTC()

	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_abc",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyWatch,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "Asia/Tokyo",
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "22:00", End: "08:00"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDefer {
		t.Fatalf("expected defer, got %s", result.Decision)
	}
	if result.ResumeAt == nil {
		t.Fatal("expected ResumeAt to be set")
	}

	// ResumeAt should be 08:00 on Feb 4 (next day).
	expectedResume := time.Date(2026, 2, 4, 8, 0, 0, 0, tokyo)
	if !result.ResumeAt.Equal(expectedResume) {
		t.Errorf("expected resume at %s, got %s", expectedResume, result.ResumeAt)
	}
}

func TestPolicyEngine_QuietHours_OutsideQuietPeriod_Delivers(t *testing.T) {
	// Current time 12:00 Tokyo. Quiet hours 22:00-08:00. Should deliver.
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Date(2026, 2, 3, 12, 0, 0, 0, tokyo).UTC()

	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_def",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "Asia/Tokyo",
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "22:00", End: "08:00"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver immediately, got %s", result.Decision)
	}
}

func TestPolicyEngine_QuietHours_DaySpecific(t *testing.T) {
	// Monday quiet hours only. Current time is 23:00 Monday Tokyo.
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	// 2026-02-02 is a Monday.
	now := time.Date(2026, 2, 2, 23, 0, 0, 0, tokyo).UTC()

	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_day",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "Asia/Tokyo",
			Schedule: []types.QuietPeriod{
				{Days: []string{"monday"}, Start: "22:00", End: "08:00"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDefer {
		t.Errorf("expected defer on Monday, got %s", result.Decision)
	}

	// Now try the same time on a Tuesday - should not be in quiet hours.
	tuesdayNow := time.Date(2026, 2, 3, 23, 0, 0, 0, tokyo).UTC() // Tuesday
	engine2 := newTestPolicyEngine(tuesdayNow)

	result2, err := engine2.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result2.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver on Tuesday, got %s", result2.Decision)
	}
}

func TestPolicyEngine_QuietHours_Disabled(t *testing.T) {
	now := time.Date(2026, 2, 3, 3, 0, 0, 0, time.UTC)
	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_disabled",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  false, // disabled
			Timezone: "UTC",
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "00:00", End: "23:59"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver when quiet hours disabled, got %s", result.Decision)
	}
}

func TestPolicyEngine_QuietHours_NilPrefs(t *testing.T) {
	now := time.Date(2026, 2, 3, 3, 0, 0, 0, time.UTC)
	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_nilprefs",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{} // no quiet hours configured

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver with nil prefs, got %s", result.Decision)
	}
}

func TestPolicyEngine_QuietHours_NewYorkTimezone(t *testing.T) {
	// IMPORTANT: Tests the key spec requirement that Quiet Hours use the
	// organization's timezone, NOT the WatchPoint's location.
	// User in New York monitors a location in Los Angeles.
	// At 23:00 UTC, it is 18:00 EST. Quiet hours 22:00-07:00 EST.
	// So it should NOT be in quiet hours.
	now := time.Date(2026, 2, 3, 23, 0, 0, 0, time.UTC)
	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_tz",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "America/New_York",
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "22:00", End: "07:00"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver at 18:00 EST, got %s (reason: %s)", result.Decision, result.Reason)
	}

	// Now at 04:00 UTC = 23:00 EST. Should be in quiet hours.
	now2 := time.Date(2026, 2, 4, 4, 0, 0, 0, time.UTC)
	engine2 := newTestPolicyEngine(now2)

	result2, err := engine2.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result2.Decision != PolicyDefer {
		t.Errorf("expected defer at 23:00 EST, got %s", result2.Decision)
	}
}

func TestPolicyEngine_QuietHours_SameDayPeriod(t *testing.T) {
	// Same-day quiet hours: 09:00-17:00 (work hours suppression).
	now := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC) // noon UTC
	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_sameday",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "UTC",
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "09:00", End: "17:00"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDefer {
		t.Fatalf("expected defer during 09:00-17:00, got %s", result.Decision)
	}

	expectedResume := time.Date(2026, 2, 3, 17, 0, 0, 0, time.UTC)
	if !result.ResumeAt.Equal(expectedResume) {
		t.Errorf("expected resume at %s, got %s", expectedResume, result.ResumeAt)
	}

	// Just before the period starts - should deliver.
	now2 := time.Date(2026, 2, 3, 8, 59, 0, 0, time.UTC)
	engine2 := newTestPolicyEngine(now2)

	result2, err := engine2.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result2.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver at 08:59, got %s", result2.Decision)
	}
}

func TestPolicyEngine_QuietHours_InvalidTimezone_FailOpen(t *testing.T) {
	// Invalid timezone should fail open (deliver) rather than suppress.
	now := time.Date(2026, 2, 3, 3, 0, 0, 0, time.UTC)
	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_badtz",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "Invalid/Timezone",
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "00:00", End: "23:59"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should deliver despite the error (fail-open).
	if result.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver on invalid timezone (fail-open), got %s", result.Decision)
	}
}

func TestPolicyEngine_QuietHours_EmptySchedule(t *testing.T) {
	now := time.Date(2026, 2, 3, 3, 0, 0, 0, time.UTC)
	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_empty_sched",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "UTC",
			Schedule: []types.QuietPeriod{}, // empty schedule
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver with empty schedule, got %s", result.Decision)
	}
}

func TestPolicyEngine_QuietHours_ExactBoundary(t *testing.T) {
	// At exactly the end of quiet hours (08:00), should NOT be in quiet period.
	// Quiet hours 22:00-08:00, current time exactly 08:00.
	now := time.Date(2026, 2, 3, 8, 0, 0, 0, time.UTC)
	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_boundary",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "UTC",
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "22:00", End: "08:00"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDeliverImmediately {
		t.Errorf("expected deliver at exact end boundary (08:00), got %s", result.Decision)
	}

	// At exactly 22:00, should BE in quiet hours (start is inclusive).
	now2 := time.Date(2026, 2, 3, 22, 0, 0, 0, time.UTC)
	engine2 := newTestPolicyEngine(now2)

	result2, err := engine2.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result2.Decision != PolicyDefer {
		t.Errorf("expected defer at exact start boundary (22:00), got %s", result2.Decision)
	}
}

func TestPolicyEngine_QuietHours_DefaultTimezoneUTC(t *testing.T) {
	// When timezone is empty, should default to UTC.
	now := time.Date(2026, 2, 3, 3, 0, 0, 0, time.UTC)
	engine := newTestPolicyEngine(now)

	n := &types.Notification{
		ID:        "notif_default_tz",
		EventType: types.EventThresholdCrossed,
		Urgency:   types.UrgencyRoutine,
	}
	org := &types.Organization{}
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Enabled:  true,
			Timezone: "", // empty - should default to UTC
			Schedule: []types.QuietPeriod{
				{Days: []string{}, Start: "02:00", End: "06:00"},
			},
		},
	}

	result, err := engine.Evaluate(context.Background(), n, org, prefs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != PolicyDefer {
		t.Errorf("expected defer at 03:00 UTC with empty timezone, got %s", result.Decision)
	}
}
