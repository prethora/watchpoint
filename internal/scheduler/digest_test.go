package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// --- Mocks ---

// mockDigestDB records calls and returns configured results for testing.
type mockDigestDB struct {
	// listDueOrgs is called by ListDueDigestOrgs. Each call pops the first
	// element from this slice to simulate paginated results.
	listDueOrgs [][]DigestOrgRow
	listDueErr  error

	// updateCalls records all UpdateNextDigestAt invocations.
	updateCalls []updateNextDigestCall
	updateErr   error
}

type updateNextDigestCall struct {
	OrgID       string
	NextDigestAt time.Time
}

func (m *mockDigestDB) ListDueDigestOrgs(_ context.Context, _ time.Time, _ int) ([]DigestOrgRow, error) {
	if m.listDueErr != nil {
		return nil, m.listDueErr
	}
	if len(m.listDueOrgs) == 0 {
		return nil, nil
	}
	result := m.listDueOrgs[0]
	m.listDueOrgs = m.listDueOrgs[1:]
	return result, nil
}

func (m *mockDigestDB) UpdateNextDigestAt(_ context.Context, orgID string, nextDigestAt time.Time) error {
	m.updateCalls = append(m.updateCalls, updateNextDigestCall{
		OrgID:       orgID,
		NextDigestAt: nextDigestAt,
	})
	return m.updateErr
}

// mockDigestPublisher records all published digest messages.
type mockDigestPublisher struct {
	published []DigestMessage
	err       error
}

func (m *mockDigestPublisher) PublishDigest(_ context.Context, msg DigestMessage) error {
	if m.err != nil {
		return m.err
	}
	m.published = append(m.published, msg)
	return nil
}

// --- CalculateNextDigestRun Tests ---

func TestCalculateNextDigestRun_DailyUTC(t *testing.T) {
	// Current time: 2026-02-06 10:00 UTC
	// Delivery time: 08:00 (default)
	// Timezone: UTC
	// Expected: 2026-02-07 08:00 UTC (already past 08:00 today)
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{}

	next, err := CalculateNextDigestRun(now, prefs, "UTC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Date(2026, 2, 7, 8, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

func TestCalculateNextDigestRun_DailyBeforeDeliveryTime(t *testing.T) {
	// Current time: 2026-02-06 06:00 UTC
	// Delivery time: 08:00
	// Timezone: UTC
	// Expected: 2026-02-06 08:00 UTC (still before delivery time today)
	now := time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{}

	next, err := CalculateNextDigestRun(now, prefs, "UTC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

func TestCalculateNextDigestRun_CustomDeliveryTime(t *testing.T) {
	// Current time: 2026-02-06 15:00 UTC
	// Delivery time: 09:30 (custom)
	// Timezone: UTC
	// Expected: 2026-02-07 09:30 UTC
	now := time.Date(2026, 2, 6, 15, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{
		Digest: &types.DigestConfig{
			Enabled:      true,
			Frequency:    "daily",
			DeliveryTime: "09:30",
		},
	}

	next, err := CalculateNextDigestRun(now, prefs, "UTC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Date(2026, 2, 7, 9, 30, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

func TestCalculateNextDigestRun_TimezoneEast(t *testing.T) {
	// Current time: 2026-02-06 02:00 UTC
	// Delivery time: 08:00
	// Timezone: Asia/Tokyo (UTC+9)
	// Local time is 2026-02-06 11:00 JST (past 08:00 local)
	// Expected next local: 2026-02-07 08:00 JST = 2026-02-06 23:00 UTC
	now := time.Date(2026, 2, 6, 2, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{
		Digest: &types.DigestConfig{
			Enabled:      true,
			Frequency:    "daily",
			DeliveryTime: "08:00",
		},
	}

	next, err := CalculateNextDigestRun(now, prefs, "Asia/Tokyo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	expected := time.Date(2026, 2, 7, 8, 0, 0, 0, tokyo).UTC()
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

func TestCalculateNextDigestRun_TimezoneWest(t *testing.T) {
	// Current time: 2026-02-06 12:00 UTC
	// Delivery time: 08:00
	// Timezone: America/New_York (UTC-5 in February, no DST)
	// Local time is 2026-02-06 07:00 EST (before 08:00 local)
	// Expected next local: 2026-02-06 08:00 EST = 2026-02-06 13:00 UTC
	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{
		Digest: &types.DigestConfig{
			Enabled:      true,
			Frequency:    "daily",
			DeliveryTime: "08:00",
		},
	}

	next, err := CalculateNextDigestRun(now, prefs, "America/New_York")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ny, _ := time.LoadLocation("America/New_York")
	expected := time.Date(2026, 2, 6, 8, 0, 0, 0, ny).UTC()
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

func TestCalculateNextDigestRun_DSTSpringForward(t *testing.T) {
	// In 2026, US DST starts on March 8 (clocks spring forward at 2:00 AM).
	// Current time: 2026-03-07 20:00 UTC
	// Delivery time: 02:30 (which is affected by DST transition)
	// Timezone: America/New_York
	// Local time is 2026-03-07 15:00 EST (before midnight, 02:30 is tomorrow)
	// On March 8, 2:30 AM doesn't exist (clocks jump from 2:00 to 3:00).
	// Go's time.Date normalizes this: 2:30 AM on March 8 in America/New_York
	// becomes 3:30 AM EDT.
	// Expected: 2026-03-08 03:30 EDT = 2026-03-08 07:30 UTC
	now := time.Date(2026, 3, 7, 20, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{
		Digest: &types.DigestConfig{
			Enabled:      true,
			Frequency:    "daily",
			DeliveryTime: "02:30",
		},
	}

	next, err := CalculateNextDigestRun(now, prefs, "America/New_York")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ny, _ := time.LoadLocation("America/New_York")
	// During DST spring-forward, 02:30 does not exist. Go normalizes it.
	// time.Date(2026, 3, 8, 2, 30, 0, 0, ny) will produce 03:30 EDT.
	expected := time.Date(2026, 3, 8, 2, 30, 0, 0, ny).UTC()
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v (in UTC)", next, expected)
	}
}

func TestCalculateNextDigestRun_DSTFallBack(t *testing.T) {
	// In 2026, US DST ends on November 1 (clocks fall back at 2:00 AM).
	// Current time: 2026-10-31 10:00 UTC
	// Delivery time: 01:30
	// Timezone: America/New_York (EDT = UTC-4 currently)
	// Local time is 2026-10-31 06:00 EDT (before 01:30, so next is today)
	// Wait - 01:30 < 06:00, so today's 01:30 already passed.
	// Next: 2026-11-01 01:30 in NY.
	// On Nov 1, 01:30 AM exists twice (EDT and EST). Go picks the first (EDT).
	// 01:30 EDT = 05:30 UTC.
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{
		Digest: &types.DigestConfig{
			Enabled:      true,
			Frequency:    "daily",
			DeliveryTime: "01:30",
		},
	}

	next, err := CalculateNextDigestRun(now, prefs, "America/New_York")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ny, _ := time.LoadLocation("America/New_York")
	expected := time.Date(2026, 11, 1, 1, 30, 0, 0, ny).UTC()
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v (in UTC)", next, expected)
	}
}

func TestCalculateNextDigestRun_EmptyTimezoneDefaultsUTC(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{}

	next, err := CalculateNextDigestRun(now, prefs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Date(2026, 2, 7, 8, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

func TestCalculateNextDigestRun_InvalidTimezone(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{}

	_, err := CalculateNextDigestRun(now, prefs, "Invalid/Zone")
	if err == nil {
		t.Fatal("expected error for invalid timezone, got nil")
	}
}

func TestCalculateNextDigestRun_InvalidDeliveryTime(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{
		Digest: &types.DigestConfig{
			DeliveryTime: "not-a-time",
		},
	}

	_, err := CalculateNextDigestRun(now, prefs, "UTC")
	if err == nil {
		t.Fatal("expected error for invalid delivery_time, got nil")
	}
}

func TestCalculateNextDigestRun_WeeklyFrequency(t *testing.T) {
	// Current time: 2026-02-06 10:00 UTC (Friday)
	// Delivery time: 08:00
	// Frequency: weekly
	// Expected: 7 days from the next daily occurrence
	// Next daily would be 2026-02-07 08:00 UTC, so weekly = 2026-02-13 08:00 UTC
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{
		Digest: &types.DigestConfig{
			Enabled:      true,
			Frequency:    "weekly",
			DeliveryTime: "08:00",
		},
	}

	next, err := CalculateNextDigestRun(now, prefs, "UTC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Date(2026, 2, 13, 8, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

func TestCalculateNextDigestRun_ExactDeliveryTime(t *testing.T) {
	// Current time is exactly at the delivery time.
	// Expected: next day at the same time.
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)
	prefs := types.NotificationPreferences{}

	next, err := CalculateNextDigestRun(now, prefs, "UTC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := time.Date(2026, 2, 7, 8, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

// --- parseTimeOfDay Tests ---

func TestParseTimeOfDay_Valid(t *testing.T) {
	tests := []struct {
		input string
		hour  int
		min   int
	}{
		{"08:00", 8, 0},
		{"00:00", 0, 0},
		{"23:59", 23, 59},
		{"12:30", 12, 30},
		{"09:05", 9, 5},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			h, m, err := parseTimeOfDay(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h != tt.hour || m != tt.min {
				t.Errorf("got %d:%d, want %d:%d", h, m, tt.hour, tt.min)
			}
		})
	}
}

func TestParseTimeOfDay_Invalid(t *testing.T) {
	tests := []string{
		"",
		"abc",
		"25:00",
		"08:60",
		"-1:00",
		"8",
		"08:00:00",
	}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			_, _, err := parseTimeOfDay(input)
			if err == nil {
				t.Errorf("expected error for input %q, got nil", input)
			}
		})
	}
}

// --- ResolveOrgTimezone Tests ---

func TestResolveOrgTimezone_FromQuietHours(t *testing.T) {
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Timezone: "America/Chicago",
		},
	}
	tz := ResolveOrgTimezone(prefs)
	if tz != "America/Chicago" {
		t.Errorf("got %q, want %q", tz, "America/Chicago")
	}
}

func TestResolveOrgTimezone_NoQuietHours(t *testing.T) {
	prefs := types.NotificationPreferences{}
	tz := ResolveOrgTimezone(prefs)
	if tz != DefaultDigestTimezone {
		t.Errorf("got %q, want %q", tz, DefaultDigestTimezone)
	}
}

func TestResolveOrgTimezone_EmptyTimezone(t *testing.T) {
	prefs := types.NotificationPreferences{
		QuietHours: &types.QuietHoursConfig{
			Timezone: "",
		},
	}
	tz := ResolveOrgTimezone(prefs)
	if tz != DefaultDigestTimezone {
		t.Errorf("got %q, want %q", tz, DefaultDigestTimezone)
	}
}

// --- computeNextDayAtTime Tests ---

func TestComputeNextDayAtTime_BeforeTarget(t *testing.T) {
	now := time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC)
	next := computeNextDayAtTime(now, 8, 0, time.UTC)
	expected := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

func TestComputeNextDayAtTime_AfterTarget(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	next := computeNextDayAtTime(now, 8, 0, time.UTC)
	expected := time.Date(2026, 2, 7, 8, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

func TestComputeNextDayAtTime_ExactlyAtTarget(t *testing.T) {
	now := time.Date(2026, 2, 6, 8, 0, 0, 0, time.UTC)
	next := computeNextDayAtTime(now, 8, 0, time.UTC)
	expected := time.Date(2026, 2, 7, 8, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

func TestComputeNextDayAtTime_Midnight(t *testing.T) {
	now := time.Date(2026, 2, 6, 23, 59, 0, 0, time.UTC)
	next := computeNextDayAtTime(now, 0, 0, time.UTC)
	expected := time.Date(2026, 2, 7, 0, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("got %v, want %v", next, expected)
	}
}

// --- TriggerDigests Integration Tests ---

func TestTriggerDigests_Success(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	db := &mockDigestDB{
		listDueOrgs: [][]DigestOrgRow{
			{
				{
					ID: "org_001",
					NotificationPreferences: types.NotificationPreferences{
						Digest: &types.DigestConfig{
							Enabled:      true,
							Frequency:    "daily",
							DeliveryTime: "08:00",
						},
					},
					Timezone: "America/New_York",
				},
				{
					ID: "org_002",
					NotificationPreferences: types.NotificationPreferences{
						Digest: &types.DigestConfig{
							Enabled:      true,
							Frequency:    "daily",
							DeliveryTime: "09:00",
						},
					},
					Timezone: "Europe/London",
				},
			},
		},
	}
	pub := &mockDigestPublisher{}

	sched := NewDigestScheduler(db, pub, nil)
	count, err := sched.TriggerDigests(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if count != 2 {
		t.Errorf("got count %d, want 2", count)
	}

	if len(pub.published) != 2 {
		t.Fatalf("got %d published messages, want 2", len(pub.published))
	}

	// Verify first message.
	if pub.published[0].OrganizationID != "org_001" {
		t.Errorf("got org_id %q, want %q", pub.published[0].OrganizationID, "org_001")
	}
	if pub.published[0].EventType != types.EventDigest {
		t.Errorf("got event_type %q, want %q", pub.published[0].EventType, types.EventDigest)
	}

	// Verify update calls.
	if len(db.updateCalls) != 2 {
		t.Fatalf("got %d update calls, want 2", len(db.updateCalls))
	}

	// Verify first org's next_digest_at is correct for America/New_York.
	ny, _ := time.LoadLocation("America/New_York")
	// At 10:00 UTC on Feb 6, it's 05:00 EST. So next 08:00 EST is today.
	expectedNY := time.Date(2026, 2, 6, 8, 0, 0, 0, ny).UTC()
	if !db.updateCalls[0].NextDigestAt.Equal(expectedNY) {
		t.Errorf("org_001 next_digest_at: got %v, want %v", db.updateCalls[0].NextDigestAt, expectedNY)
	}

	// Verify second org's next_digest_at is correct for Europe/London.
	london, _ := time.LoadLocation("Europe/London")
	// At 10:00 UTC on Feb 6, it's 10:00 GMT. So next 09:00 GMT is tomorrow.
	expectedLondon := time.Date(2026, 2, 7, 9, 0, 0, 0, london).UTC()
	if !db.updateCalls[1].NextDigestAt.Equal(expectedLondon) {
		t.Errorf("org_002 next_digest_at: got %v, want %v", db.updateCalls[1].NextDigestAt, expectedLondon)
	}
}

func TestTriggerDigests_NoOrgs(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	db := &mockDigestDB{
		listDueOrgs: [][]DigestOrgRow{},
	}
	pub := &mockDigestPublisher{}

	sched := NewDigestScheduler(db, pub, nil)
	count, err := sched.TriggerDigests(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if count != 0 {
		t.Errorf("got count %d, want 0", count)
	}

	if len(pub.published) != 0 {
		t.Errorf("got %d published messages, want 0", len(pub.published))
	}
}

func TestTriggerDigests_DBListError(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	db := &mockDigestDB{
		listDueErr: fmt.Errorf("connection refused"),
	}
	pub := &mockDigestPublisher{}

	sched := NewDigestScheduler(db, pub, nil)
	_, err := sched.TriggerDigests(context.Background(), now)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestTriggerDigests_PublishErrorSkipsOrg(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	db := &mockDigestDB{
		listDueOrgs: [][]DigestOrgRow{
			{
				{ID: "org_001", Timezone: "UTC"},
				{ID: "org_002", Timezone: "UTC"},
			},
		},
	}

	pub := &failNthPublisher{failOnCall: 1} // Fail on first call (org_001)

	sched := NewDigestScheduler(db, pub, nil)
	count, err := sched.TriggerDigests(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only org_002 should succeed.
	if count != 1 {
		t.Errorf("got count %d, want 1", count)
	}

	// Only org_002 should have its next_digest_at updated.
	if len(db.updateCalls) != 1 {
		t.Fatalf("got %d update calls, want 1", len(db.updateCalls))
	}
	if db.updateCalls[0].OrgID != "org_002" {
		t.Errorf("got org_id %q, want %q", db.updateCalls[0].OrgID, "org_002")
	}
}

func TestTriggerDigests_UpdateErrorSkipsOrg(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	db := &mockDigestDB{
		listDueOrgs: [][]DigestOrgRow{
			{
				{ID: "org_001", Timezone: "UTC"},
			},
		},
		updateErr: fmt.Errorf("db write error"),
	}
	pub := &mockDigestPublisher{}

	sched := NewDigestScheduler(db, pub, nil)
	count, err := sched.TriggerDigests(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The org was published to SQS but the update failed, so it counts as failed.
	if count != 0 {
		t.Errorf("got count %d, want 0", count)
	}

	// SQS message was still published (the error is on DB update).
	if len(pub.published) != 1 {
		t.Errorf("got %d published messages, want 1", len(pub.published))
	}
}

func TestTriggerDigests_MultipleBatches(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)

	// Simulate two batches: first returns DigestBatchLimit orgs, second returns fewer.
	batch1 := make([]DigestOrgRow, DigestBatchLimit)
	for i := range batch1 {
		batch1[i] = DigestOrgRow{
			ID:       fmt.Sprintf("org_%03d", i),
			Timezone: "UTC",
		}
	}
	batch2 := []DigestOrgRow{
		{ID: "org_last", Timezone: "UTC"},
	}

	db := &mockDigestDB{
		listDueOrgs: [][]DigestOrgRow{batch1, batch2},
	}
	pub := &mockDigestPublisher{}

	sched := NewDigestScheduler(db, pub, nil)
	count, err := sched.TriggerDigests(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedCount := DigestBatchLimit + 1
	if count != expectedCount {
		t.Errorf("got count %d, want %d", count, expectedCount)
	}
}

func TestTriggerDigests_InvalidTimezoneUsesFallback(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)
	db := &mockDigestDB{
		listDueOrgs: [][]DigestOrgRow{
			{
				{
					ID:       "org_bad_tz",
					Timezone: "Invalid/BadZone",
				},
			},
		},
	}
	pub := &mockDigestPublisher{}

	sched := NewDigestScheduler(db, pub, nil)
	count, err := sched.TriggerDigests(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if count != 1 {
		t.Errorf("got count %d, want 1", count)
	}

	// Should have used fallback: next day at 08:00 UTC.
	if len(db.updateCalls) != 1 {
		t.Fatalf("got %d update calls, want 1", len(db.updateCalls))
	}
	expected := time.Date(2026, 2, 7, 8, 0, 0, 0, time.UTC)
	if !db.updateCalls[0].NextDigestAt.Equal(expected) {
		t.Errorf("fallback next_digest_at: got %v, want %v", db.updateCalls[0].NextDigestAt, expected)
	}
}

func TestTriggerDigests_AllFailBreaksToPreventInfiniteLoop(t *testing.T) {
	now := time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC)

	// Create a batch of exactly DigestBatchLimit orgs where ALL fail to publish.
	// Without the safety break, this would loop forever.
	batch := make([]DigestOrgRow, DigestBatchLimit)
	for i := range batch {
		batch[i] = DigestOrgRow{
			ID:       fmt.Sprintf("org_%03d", i),
			Timezone: "UTC",
		}
	}

	// alwaysFailDB returns the same full batch every time, simulating no progress.
	db := &alwaysFailDB{batch: batch}
	pub := &mockDigestPublisher{err: fmt.Errorf("SQS unavailable")}

	sched := NewDigestScheduler(db, pub, nil)
	count, err := sched.TriggerDigests(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if count != 0 {
		t.Errorf("got count %d, want 0", count)
	}

	// Should have called ListDueDigestOrgs exactly once (broke after first batch).
	if db.callCount != 1 {
		t.Errorf("ListDueDigestOrgs called %d times, want 1", db.callCount)
	}
}

// alwaysFailDB returns the same batch every time, simulating a scenario
// where no orgs are successfully processed (next_digest_at never advances).
type alwaysFailDB struct {
	batch     []DigestOrgRow
	callCount int
}

func (m *alwaysFailDB) ListDueDigestOrgs(_ context.Context, _ time.Time, _ int) ([]DigestOrgRow, error) {
	m.callCount++
	return m.batch, nil
}

func (m *alwaysFailDB) UpdateNextDigestAt(_ context.Context, _ string, _ time.Time) error {
	return nil
}

// --- MarshalDigestMessage Tests ---

func TestMarshalDigestMessage(t *testing.T) {
	msg := DigestMessage{
		OrganizationID: "org_123",
		EventType:      types.EventDigest,
		TriggeredAt:    time.Date(2026, 2, 6, 10, 0, 0, 0, time.UTC),
	}

	data, err := MarshalDigestMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("expected non-empty JSON output")
	}

	// Verify it contains the expected fields.
	jsonStr := string(data)
	for _, expected := range []string{`"org_123"`, `"monitor_digest"`, `"2026-02-06T10:00:00Z"`} {
		if !strings.Contains(jsonStr, expected) {
			t.Errorf("JSON %q does not contain %q", jsonStr, expected)
		}
	}
}

// --- Helper types ---

// failNthPublisher fails on the Nth call (1-indexed).
type failNthPublisher struct {
	callCount  int
	failOnCall int
	published  []DigestMessage
}

func (p *failNthPublisher) PublishDigest(_ context.Context, msg DigestMessage) error {
	p.callCount++
	if p.callCount == p.failOnCall {
		return fmt.Errorf("SQS publish error")
	}
	p.published = append(p.published, msg)
	return nil
}

