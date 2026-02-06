package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// ============================================================
// Mock Implementations
// ============================================================

// mockUsageAggregatorDB is an in-memory mock of UsageAggregatorDB.
type mockUsageAggregatorDB struct {
	mu sync.Mutex

	// staleOrgIDs is returned by ListStaleRateLimits, one batch at a time.
	// Each call pops the first batch. When empty, returns nil.
	staleOrgBatches [][]string
	listErr         error

	// transactions tracks all created transactions for verification.
	transactions []*mockUsageAggregatorTx

	// txFactory allows tests to control per-org transaction creation.
	// If nil, a default mock tx is created.
	txFactory func(orgID string) *mockUsageAggregatorTx
	beginErr  error
}

func (m *mockUsageAggregatorDB) ListStaleRateLimits(_ context.Context, _ time.Time, _ int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.listErr != nil {
		return nil, m.listErr
	}
	if len(m.staleOrgBatches) == 0 {
		return nil, nil
	}
	batch := m.staleOrgBatches[0]
	m.staleOrgBatches = m.staleOrgBatches[1:]
	return batch, nil
}

func (m *mockUsageAggregatorDB) BeginTx(_ context.Context) (UsageAggregatorTx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.beginErr != nil {
		return nil, m.beginErr
	}

	// If there's a factory, use it to get the correct transaction for the next org.
	var tx *mockUsageAggregatorTx
	if m.txFactory != nil && len(m.transactions) < len(m.staleOrgBatches)+100 {
		// txFactory will be called with empty string; tests should configure
		// transactions via the txByOrg map instead.
		tx = &mockUsageAggregatorTx{}
	}
	if tx == nil {
		tx = &mockUsageAggregatorTx{}
	}
	m.transactions = append(m.transactions, tx)
	return tx, nil
}

// mockUsageAggregatorTx is an in-memory mock of UsageAggregatorTx.
type mockUsageAggregatorTx struct {
	mu sync.Mutex

	// LockRateLimitRows behavior
	lockedRows    []StaleRateLimitRow
	lockErr       error
	lockCallCount int

	// InsertUsageHistory behavior
	insertedHistory []insertHistoryCall
	insertErr       error

	// GetOrgOverageInfo behavior
	planLimits     types.PlanLimits
	overageStarted *time.Time
	overageInfoErr error

	// UpdateOrgOverage behavior
	overageUpdates []overageUpdateCall
	overageUpdErr  error

	// ResetRateLimitRow behavior
	resetCalls []resetCall
	resetErr   error

	// Commit/Rollback tracking
	committed  bool
	commitErr  error
	rolledBack bool
}

type insertHistoryCall struct {
	OrgID       string
	Date        time.Time
	Source      string
	APICalls    int
	WatchPoints int
}

type overageUpdateCall struct {
	OrgID            string
	OverageStartedAt *time.Time
}

type resetCall struct {
	OrgID       string
	Source      string
	PeriodStart time.Time
	PeriodEnd   time.Time
}

func (tx *mockUsageAggregatorTx) LockRateLimitRows(_ context.Context, _ string) ([]StaleRateLimitRow, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.lockCallCount++
	if tx.lockErr != nil {
		return nil, tx.lockErr
	}
	return tx.lockedRows, nil
}

func (tx *mockUsageAggregatorTx) InsertUsageHistory(_ context.Context, orgID string, date time.Time, source string, apiCalls, watchpoints int) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.insertedHistory = append(tx.insertedHistory, insertHistoryCall{
		OrgID:       orgID,
		Date:        date,
		Source:      source,
		APICalls:    apiCalls,
		WatchPoints: watchpoints,
	})
	return tx.insertErr
}

func (tx *mockUsageAggregatorTx) GetOrgOverageInfo(_ context.Context, _ string) (types.PlanLimits, *time.Time, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.overageInfoErr != nil {
		return types.PlanLimits{}, nil, tx.overageInfoErr
	}
	return tx.planLimits, tx.overageStarted, nil
}

func (tx *mockUsageAggregatorTx) UpdateOrgOverage(_ context.Context, orgID string, overageStartedAt *time.Time) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.overageUpdates = append(tx.overageUpdates, overageUpdateCall{
		OrgID:            orgID,
		OverageStartedAt: overageStartedAt,
	})
	return tx.overageUpdErr
}

func (tx *mockUsageAggregatorTx) ResetRateLimitRow(_ context.Context, orgID, source string, periodStart, periodEnd time.Time) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.resetCalls = append(tx.resetCalls, resetCall{
		OrgID:       orgID,
		Source:      source,
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
	})
	return tx.resetErr
}

func (tx *mockUsageAggregatorTx) Commit(_ context.Context) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.committed = true
	return tx.commitErr
}

func (tx *mockUsageAggregatorTx) Rollback(_ context.Context) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.rolledBack = true
	return nil
}

// testLogger creates a logger for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ============================================================
// Helper: mockDBWithOrgs creates a mock DB that returns a single batch of org IDs
// and creates per-org transactions via the provided map.
// ============================================================

type perOrgMockDB struct {
	mu         sync.Mutex
	orgBatches [][]string
	listErr    error
	beginErr   error

	// txByOrg maps org ID -> the mock tx that should be used.
	// processOrg calls BeginTx then LockRateLimitRows(orgID).
	// We track which tx index maps to which org by order.
	txByOrg map[string]*mockUsageAggregatorTx
	txOrder []string // tracks the order of BeginTx calls for org mapping
}

func (m *perOrgMockDB) ListStaleRateLimits(_ context.Context, _ time.Time, _ int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	if len(m.orgBatches) == 0 {
		return nil, nil
	}
	batch := m.orgBatches[0]
	m.orgBatches = m.orgBatches[1:]
	return batch, nil
}

func (m *perOrgMockDB) BeginTx(_ context.Context) (UsageAggregatorTx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.beginErr != nil {
		return nil, m.beginErr
	}
	// Return a tx that will be used for the next org in the processing order.
	// Since processOrg is called sequentially per org, we track via txOrder.
	if len(m.txOrder) < len(m.txByOrg) {
		// Look for the next unprocessed org
	}
	return &delegatingTx{parent: m}, nil
}

// delegatingTx is a wrapper that delegates to the correct per-org mock tx
// once LockRateLimitRows reveals the org ID.
type delegatingTx struct {
	parent   *perOrgMockDB
	delegate *mockUsageAggregatorTx
}

func (d *delegatingTx) LockRateLimitRows(ctx context.Context, orgID string) ([]StaleRateLimitRow, error) {
	d.parent.mu.Lock()
	d.delegate = d.parent.txByOrg[orgID]
	d.parent.txOrder = append(d.parent.txOrder, orgID)
	d.parent.mu.Unlock()

	if d.delegate == nil {
		return nil, fmt.Errorf("no mock tx configured for org %s", orgID)
	}
	return d.delegate.LockRateLimitRows(ctx, orgID)
}

func (d *delegatingTx) InsertUsageHistory(ctx context.Context, orgID string, date time.Time, source string, apiCalls, watchpoints int) error {
	return d.delegate.InsertUsageHistory(ctx, orgID, date, source, apiCalls, watchpoints)
}

func (d *delegatingTx) GetOrgOverageInfo(ctx context.Context, orgID string) (types.PlanLimits, *time.Time, error) {
	return d.delegate.GetOrgOverageInfo(ctx, orgID)
}

func (d *delegatingTx) UpdateOrgOverage(ctx context.Context, orgID string, overageStartedAt *time.Time) error {
	return d.delegate.UpdateOrgOverage(ctx, orgID, overageStartedAt)
}

func (d *delegatingTx) ResetRateLimitRow(ctx context.Context, orgID, source string, periodStart, periodEnd time.Time) error {
	return d.delegate.ResetRateLimitRow(ctx, orgID, source, periodStart, periodEnd)
}

func (d *delegatingTx) Commit(ctx context.Context) error {
	return d.delegate.Commit(ctx)
}

func (d *delegatingTx) Rollback(ctx context.Context) error {
	return d.delegate.Rollback(ctx)
}

// ============================================================
// Tests
// ============================================================

func TestSnapshotDailyUsage_NoStaleRecords(t *testing.T) {
	db := &perOrgMockDB{
		orgBatches: [][]string{}, // No stale orgs
		txByOrg:    map[string]*mockUsageAggregatorTx{},
	}

	agg := NewUsageAggregator(db, testLogger())
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)

	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 processed, got %d", count)
	}
}

func TestSnapshotDailyUsage_SingleOrg_SingleSource(t *testing.T) {
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{
				OrganizationID:   "org_1",
				Source:           "default",
				APICallsCount:    42,
				WatchPointsCount: 3,
				PeriodStart:      yesterday,
				PeriodEnd:        yesterdayEnd,
			},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
		// No overage currently set
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_1"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_1": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed, got %d", count)
	}

	// Verify usage history was inserted
	if len(tx.insertedHistory) != 1 {
		t.Fatalf("expected 1 history insert, got %d", len(tx.insertedHistory))
	}
	h := tx.insertedHistory[0]
	if h.OrgID != "org_1" || h.Source != "default" || h.APICalls != 42 || h.WatchPoints != 3 {
		t.Errorf("unexpected history insert: %+v", h)
	}
	if h.Date != yesterday {
		t.Errorf("expected history date %v, got %v", yesterday, h.Date)
	}

	// Verify no overage update (usage within limits)
	if len(tx.overageUpdates) != 0 {
		t.Errorf("expected no overage updates, got %d", len(tx.overageUpdates))
	}

	// Verify rate limits were reset
	if len(tx.resetCalls) != 1 {
		t.Fatalf("expected 1 reset call, got %d", len(tx.resetCalls))
	}
	r := tx.resetCalls[0]
	if r.OrgID != "org_1" || r.Source != "default" {
		t.Errorf("unexpected reset call: %+v", r)
	}
	if r.PeriodStart != now {
		t.Errorf("expected period_start=%v, got %v", now, r.PeriodStart)
	}
	expectedEnd := time.Date(2026, 2, 7, 0, 0, 0, 0, time.UTC)
	if r.PeriodEnd != expectedEnd {
		t.Errorf("expected period_end=%v, got %v", expectedEnd, r.PeriodEnd)
	}

	// Verify transaction was committed
	if !tx.committed {
		t.Error("expected transaction to be committed")
	}
}

func TestSnapshotDailyUsage_SingleOrg_MultipleSources_VERT001(t *testing.T) {
	// Tests composite key processing (VERT-001): multiple (org, source) rows
	// are snapshotted individually and usage is aggregated for overage check.
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{
				OrganizationID:   "org_1",
				Source:           "default",
				APICallsCount:    30,
				WatchPointsCount: 5,
				PeriodStart:      yesterday,
				PeriodEnd:        yesterdayEnd,
			},
			{
				OrganizationID:   "org_1",
				Source:           "wedding_app",
				APICallsCount:    20,
				WatchPointsCount: 3,
				PeriodStart:      yesterday,
				PeriodEnd:        yesterdayEnd,
			},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_1"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_1": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed, got %d", count)
	}

	// Verify two separate history inserts (one per source)
	if len(tx.insertedHistory) != 2 {
		t.Fatalf("expected 2 history inserts, got %d", len(tx.insertedHistory))
	}
	if tx.insertedHistory[0].Source != "default" || tx.insertedHistory[0].APICalls != 30 {
		t.Errorf("unexpected first history insert: %+v", tx.insertedHistory[0])
	}
	if tx.insertedHistory[1].Source != "wedding_app" || tx.insertedHistory[1].APICalls != 20 {
		t.Errorf("unexpected second history insert: %+v", tx.insertedHistory[1])
	}

	// Verify two separate reset calls (one per source)
	if len(tx.resetCalls) != 2 {
		t.Fatalf("expected 2 reset calls, got %d", len(tx.resetCalls))
	}
	if tx.resetCalls[0].Source != "default" {
		t.Errorf("expected first reset for 'default', got %s", tx.resetCalls[0].Source)
	}
	if tx.resetCalls[1].Source != "wedding_app" {
		t.Errorf("expected second reset for 'wedding_app', got %s", tx.resetCalls[1].Source)
	}

	// No overage (30+20=50 API calls < 100 limit, 5+3=8 WPs < 25 limit)
	if len(tx.overageUpdates) != 0 {
		t.Errorf("expected no overage updates, got %d", len(tx.overageUpdates))
	}

	if !tx.committed {
		t.Error("expected transaction to be committed")
	}
}

func TestSnapshotDailyUsage_OverageDetection_APICallsExceeded(t *testing.T) {
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{
				OrganizationID:   "org_over",
				Source:           "default",
				APICallsCount:    150, // Exceeds 100 limit
				WatchPointsCount: 5,
				PeriodStart:      yesterday,
				PeriodEnd:        yesterdayEnd,
			},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
		// No current overage
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_over"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_over": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed, got %d", count)
	}

	// Verify overage was set
	if len(tx.overageUpdates) != 1 {
		t.Fatalf("expected 1 overage update, got %d", len(tx.overageUpdates))
	}
	ou := tx.overageUpdates[0]
	if ou.OrgID != "org_over" {
		t.Errorf("expected overage update for org_over, got %s", ou.OrgID)
	}
	if ou.OverageStartedAt == nil {
		t.Fatal("expected overage_started_at to be set, got nil")
	}
	if !ou.OverageStartedAt.Equal(now) {
		t.Errorf("expected overage_started_at=%v, got %v", now, *ou.OverageStartedAt)
	}
}

func TestSnapshotDailyUsage_OverageDetection_WatchPointsExceeded(t *testing.T) {
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{
				OrganizationID:   "org_over",
				Source:           "default",
				APICallsCount:    10,
				WatchPointsCount: 30, // Exceeds 25 limit
				PeriodStart:      yesterday,
				PeriodEnd:        yesterdayEnd,
			},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_over"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_over": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed, got %d", count)
	}

	// Verify overage was set
	if len(tx.overageUpdates) != 1 {
		t.Fatalf("expected 1 overage update, got %d", len(tx.overageUpdates))
	}
	if tx.overageUpdates[0].OverageStartedAt == nil {
		t.Fatal("expected overage_started_at to be set")
	}
}

func TestSnapshotDailyUsage_OverageCure(t *testing.T) {
	// Tests compliance check: org was in overage, usage is now within limits.
	// Overage should be cleared (set to NULL).
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	overageTime := time.Date(2026, 1, 20, 1, 0, 0, 0, time.UTC) // Org has been in overage since Jan 20

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{
				OrganizationID:   "org_cured",
				Source:           "default",
				APICallsCount:    50, // Within 100 limit
				WatchPointsCount: 10, // Within 25 limit
				PeriodStart:      yesterday,
				PeriodEnd:        yesterdayEnd,
			},
		},
		planLimits:     types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
		overageStarted: &overageTime,
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_cured"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_cured": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed, got %d", count)
	}

	// Verify overage was cleared
	if len(tx.overageUpdates) != 1 {
		t.Fatalf("expected 1 overage update, got %d", len(tx.overageUpdates))
	}
	if tx.overageUpdates[0].OverageStartedAt != nil {
		t.Errorf("expected overage_started_at to be nil (cleared), got %v", *tx.overageUpdates[0].OverageStartedAt)
	}
}

func TestSnapshotDailyUsage_OverageAlreadySet_NoDoubleSet(t *testing.T) {
	// Tests that if an org is already in overage and still over the limit,
	// no overage update is issued (idempotent behavior).
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	overageTime := time.Date(2026, 1, 25, 1, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{
				OrganizationID:   "org_still_over",
				Source:           "default",
				APICallsCount:    200, // Still over 100 limit
				WatchPointsCount: 5,
				PeriodStart:      yesterday,
				PeriodEnd:        yesterdayEnd,
			},
		},
		planLimits:     types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
		overageStarted: &overageTime,
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_still_over"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_still_over": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed, got %d", count)
	}

	// Verify NO overage update (already in overage, stays in overage)
	if len(tx.overageUpdates) != 0 {
		t.Errorf("expected no overage updates (already in overage), got %d", len(tx.overageUpdates))
	}
}

func TestSnapshotDailyUsage_EnterprisePlan_UnlimitedNeverOverage(t *testing.T) {
	// Enterprise plan has 0 for limits which means unlimited.
	// Even with very high usage, should never trigger overage.
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{
				OrganizationID:   "org_ent",
				Source:           "default",
				APICallsCount:    999999,
				WatchPointsCount: 99999,
				PeriodStart:      yesterday,
				PeriodEnd:        yesterdayEnd,
			},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 0, MaxWatchPoints: 0}, // Enterprise: unlimited
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_ent"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_ent": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed, got %d", count)
	}

	// No overage update for Enterprise tier
	if len(tx.overageUpdates) != 0 {
		t.Errorf("expected no overage updates for Enterprise tier, got %d", len(tx.overageUpdates))
	}
}

func TestSnapshotDailyUsage_MultipleOrgs(t *testing.T) {
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx1 := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_1", Source: "default", APICallsCount: 10, WatchPointsCount: 2, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
	}
	tx2 := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_2", Source: "default", APICallsCount: 20, WatchPointsCount: 5, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_1", "org_2"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_1": tx1,
			"org_2": tx2,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 processed, got %d", count)
	}

	// Both should be committed
	if !tx1.committed {
		t.Error("expected tx1 to be committed")
	}
	if !tx2.committed {
		t.Error("expected tx2 to be committed")
	}
}

func TestSnapshotDailyUsage_OrgFailure_ContinuesWithOthers(t *testing.T) {
	// If one org fails, the aggregator should continue processing others.
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	txFailing := &mockUsageAggregatorTx{
		lockErr: errors.New("db timeout"), // This org will fail
	}
	txSucceeding := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_ok", Source: "default", APICallsCount: 10, WatchPointsCount: 2, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_fail", "org_ok"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_fail": txFailing,
			"org_ok":   txSucceeding,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only the successful org should be counted
	if count != 1 {
		t.Errorf("expected 1 processed (1 failed), got %d", count)
	}

	if txSucceeding.committed != true {
		t.Error("expected successful org's tx to be committed")
	}
}

func TestSnapshotDailyUsage_ListStaleError(t *testing.T) {
	db := &perOrgMockDB{
		listErr: errors.New("connection refused"),
		txByOrg: map[string]*mockUsageAggregatorTx{},
	}

	agg := NewUsageAggregator(db, testLogger())
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)

	_, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err == nil {
		t.Fatal("expected error from ListStaleRateLimits, got nil")
	}
}

func TestSnapshotDailyUsage_InsertHistoryError_RollsBack(t *testing.T) {
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_1", Source: "default", APICallsCount: 10, WatchPointsCount: 2, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
		},
		insertErr: errors.New("disk full"),
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_1"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_1": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	// The overall function doesn't return an error for individual org failures.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 processed, got %d", count)
	}

	// Verify transaction was rolled back (via deferred rollback), not committed
	if tx.committed {
		t.Error("expected transaction NOT to be committed on error")
	}
	if !tx.rolledBack {
		t.Error("expected transaction to be rolled back on error")
	}
}

func TestSnapshotDailyUsage_EmptyLockedRows(t *testing.T) {
	// Race condition: org appeared in ListStaleRateLimits but by the time
	// we lock, another process already reset it. Should be a no-op.
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{}, // Empty: already processed
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_1"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_1": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The org was "processed" (no-op because no rows), but processOrg returns nil
	// without incrementing totalProcessed since it returns nil with no rows locked.
	// Actually, processOrg returns nil for empty rows, and the caller counts it as success.
	// Let me verify the actual behavior by re-reading the implementation:
	// processOrg returns nil for empty rows, and the caller increments totalProcessed.
	if count != 1 {
		t.Errorf("expected 1 processed (no-op is still success), got %d", count)
	}

	// No history inserts, no overage updates, no resets
	if len(tx.insertedHistory) != 0 {
		t.Errorf("expected 0 history inserts, got %d", len(tx.insertedHistory))
	}
	if len(tx.overageUpdates) != 0 {
		t.Errorf("expected 0 overage updates, got %d", len(tx.overageUpdates))
	}
	if len(tx.resetCalls) != 0 {
		t.Errorf("expected 0 reset calls, got %d", len(tx.resetCalls))
	}
}

func TestSnapshotDailyUsage_MultipleBatches(t *testing.T) {
	// Tests that the aggregator continues fetching batches until no more stale orgs.
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	makeTx := func(orgID string) *mockUsageAggregatorTx {
		return &mockUsageAggregatorTx{
			lockedRows: []StaleRateLimitRow{
				{OrganizationID: orgID, Source: "default", APICallsCount: 5, WatchPointsCount: 1, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
			},
			planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
		}
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{
			{"org_1", "org_2"}, // First batch
			{"org_3"},          // Second batch
			// Third call returns empty -> loop ends
		},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_1": makeTx("org_1"),
			"org_2": makeTx("org_2"),
			"org_3": makeTx("org_3"),
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 processed across 2 batches, got %d", count)
	}
}

func TestSnapshotDailyUsage_OverageCheck_AggregatesAcrossSources(t *testing.T) {
	// Tests that overage check uses SUM across all sources.
	// Each source alone is under the limit, but combined they exceed it.
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_multi", Source: "default", APICallsCount: 60, WatchPointsCount: 3, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
			{OrganizationID: "org_multi", Source: "app2", APICallsCount: 50, WatchPointsCount: 2, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
		},
		// Total: 110 API calls, 5 WPs. Limit is 100 API calls.
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_multi"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_multi": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 processed, got %d", count)
	}

	// Overage should be triggered because SUM(60+50) = 110 > 100
	if len(tx.overageUpdates) != 1 {
		t.Fatalf("expected 1 overage update (aggregated across sources), got %d", len(tx.overageUpdates))
	}
	if tx.overageUpdates[0].OverageStartedAt == nil {
		t.Fatal("expected overage_started_at to be set")
	}
}

func TestSnapshotDailyUsage_CommitError_CountsAsFailure(t *testing.T) {
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_1", Source: "default", APICallsCount: 10, WatchPointsCount: 2, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
		commitErr:  errors.New("serialization failure"),
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_1"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_1": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Commit failure means this org was NOT successfully processed
	if count != 0 {
		t.Errorf("expected 0 processed (commit failed), got %d", count)
	}
}

func TestSnapshotDailyUsage_ResetError_RollsBack(t *testing.T) {
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_1", Source: "default", APICallsCount: 10, WatchPointsCount: 2, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
		resetErr:   errors.New("constraint violation"),
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_1"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_1": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 processed (reset failed), got %d", count)
	}

	if tx.committed {
		t.Error("expected transaction NOT to be committed after reset error")
	}
	if !tx.rolledBack {
		t.Error("expected transaction to be rolled back after reset error")
	}
}

// ============================================================
// Unit tests for helper functions
// ============================================================

func TestIsUsageOverLimit(t *testing.T) {
	tests := []struct {
		name       string
		apiCalls   int
		watchpoints int
		limits     types.PlanLimits
		want       bool
	}{
		{
			name:       "within limits",
			apiCalls:   50,
			watchpoints: 10,
			limits:     types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
			want:       false,
		},
		{
			name:       "api calls exceeded",
			apiCalls:   101,
			watchpoints: 10,
			limits:     types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
			want:       true,
		},
		{
			name:       "watchpoints exceeded",
			apiCalls:   50,
			watchpoints: 26,
			limits:     types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
			want:       true,
		},
		{
			name:       "both exceeded",
			apiCalls:   101,
			watchpoints: 26,
			limits:     types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
			want:       true,
		},
		{
			name:       "exactly at limit is not overage",
			apiCalls:   100,
			watchpoints: 25,
			limits:     types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
			want:       false,
		},
		{
			name:       "enterprise unlimited (0 means no limit)",
			apiCalls:   999999,
			watchpoints: 999999,
			limits:     types.PlanLimits{MaxAPICallsDaily: 0, MaxWatchPoints: 0},
			want:       false,
		},
		{
			name:       "free tier no API calls (0 limit, 0 usage)",
			apiCalls:   0,
			watchpoints: 2,
			limits:     types.PlanLimits{MaxAPICallsDaily: 0, MaxWatchPoints: 3},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isUsageOverLimit(tt.apiCalls, tt.watchpoints, tt.limits)
			if got != tt.want {
				t.Errorf("isUsageOverLimit(%d, %d, %+v) = %v, want %v",
					tt.apiCalls, tt.watchpoints, tt.limits, got, tt.want)
			}
		})
	}
}

func TestComputeNextMidnight(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "1 AM UTC",
			now:  time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC),
			want: time.Date(2026, 2, 7, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "23:59 UTC",
			now:  time.Date(2026, 2, 6, 23, 59, 59, 0, time.UTC),
			want: time.Date(2026, 2, 7, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "midnight exactly",
			now:  time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, 2, 7, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "end of month",
			now:  time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "end of year",
			now:  time.Date(2026, 12, 31, 18, 0, 0, 0, time.UTC),
			want: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeNextMidnight(tt.now)
			if !got.Equal(tt.want) {
				t.Errorf("computeNextMidnight(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestSnapshotDailyUsage_BeginTxError(t *testing.T) {
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_1"}},
		beginErr:   errors.New("connection pool exhausted"),
		txByOrg:    map[string]*mockUsageAggregatorTx{},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	// BeginTx failure for one org doesn't fail the batch; it's logged and skipped.
	if count != 0 {
		t.Errorf("expected 0 processed, got %d", count)
	}
}

func TestSnapshotDailyUsage_GetOrgOverageInfoError(t *testing.T) {
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_1", Source: "default", APICallsCount: 10, WatchPointsCount: 2, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
		},
		overageInfoErr: errors.New("org not found"),
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_1"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_1": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 processed, got %d", count)
	}
	if tx.committed {
		t.Error("expected transaction NOT to be committed")
	}
}

func TestSnapshotDailyUsage_UpdateOrgOverageError(t *testing.T) {
	now := time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_over", Source: "default", APICallsCount: 200, WatchPointsCount: 5, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
		},
		planLimits:  types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
		overageUpdErr: errors.New("deadlock detected"),
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_over"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_over": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	count, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 processed, got %d", count)
	}
	if tx.committed {
		t.Error("expected transaction NOT to be committed after overage update error")
	}
}

func TestSnapshotDailyUsage_HistoryDate_DerivedFromPeriodStart(t *testing.T) {
	// Verifies that the usage_history date is derived from the rate_limits period_start,
	// not from the targetDate parameter. This is important for correctness when
	// processing rate_limits rows from different periods.
	now := time.Date(2026, 2, 8, 1, 0, 0, 0, time.UTC) // Running on Feb 8
	// But the stale row is from Feb 5 (3 days old, was never reset)
	periodStart := time.Date(2026, 2, 5, 3, 15, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_late", Source: "default", APICallsCount: 42, WatchPointsCount: 3, PeriodStart: periodStart, PeriodEnd: periodEnd},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_late"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_late": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	_, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The history date should be Feb 5 (from period_start), not Feb 8 (targetDate)
	expectedDate := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	if len(tx.insertedHistory) != 1 {
		t.Fatalf("expected 1 history insert, got %d", len(tx.insertedHistory))
	}
	if !tx.insertedHistory[0].Date.Equal(expectedDate) {
		t.Errorf("expected history date=%v (from period_start), got %v", expectedDate, tx.insertedHistory[0].Date)
	}
}

func TestSnapshotDailyUsage_ResetPeriodEnd_IsNextMidnight(t *testing.T) {
	// Verifies the new period_end after reset is the next UTC midnight.
	now := time.Date(2026, 2, 6, 1, 30, 0, 0, time.UTC)
	yesterday := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	yesterdayEnd := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)

	tx := &mockUsageAggregatorTx{
		lockedRows: []StaleRateLimitRow{
			{OrganizationID: "org_1", Source: "default", APICallsCount: 10, WatchPointsCount: 2, PeriodStart: yesterday, PeriodEnd: yesterdayEnd},
		},
		planLimits: types.PlanLimits{MaxAPICallsDaily: 100, MaxWatchPoints: 25},
	}

	db := &perOrgMockDB{
		orgBatches: [][]string{{"org_1"}},
		txByOrg: map[string]*mockUsageAggregatorTx{
			"org_1": tx,
		},
	}

	agg := NewUsageAggregator(db, testLogger())
	_, err := agg.SnapshotDailyUsage(context.Background(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// period_start should be `now`, period_end should be next midnight
	if len(tx.resetCalls) != 1 {
		t.Fatalf("expected 1 reset call, got %d", len(tx.resetCalls))
	}
	r := tx.resetCalls[0]
	expectedStart := now
	expectedEnd := time.Date(2026, 2, 7, 0, 0, 0, 0, time.UTC)
	if !r.PeriodStart.Equal(expectedStart) {
		t.Errorf("expected period_start=%v, got %v", expectedStart, r.PeriodStart)
	}
	if !r.PeriodEnd.Equal(expectedEnd) {
		t.Errorf("expected period_end=%v, got %v", expectedEnd, r.PeriodEnd)
	}
}
