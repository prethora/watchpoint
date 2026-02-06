package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// =============================================================================
// Mock Implementations
// =============================================================================

// --- StripeSyncer Mocks ---

type mockStripeSyncerDB struct {
	orgs              []BillingSyncOrg
	listErr           error
	syncUpdated       map[string]time.Time // orgID -> syncedAt
	billingUpdates    []billingUpdate
	customerUpdates   map[string]string // orgID -> customerID
	updateSyncErr     error
	updateBillingErr  error
	updateCustomerErr error
}

type billingUpdate struct {
	OrgID          string
	Plan           types.PlanTier
	Status         types.SubscriptionStatus
	EventTimestamp time.Time
}

func (m *mockStripeSyncerDB) ListOrgsForBillingSync(_ context.Context, _ time.Time, _ time.Duration, _ int) ([]BillingSyncOrg, error) {
	return m.orgs, m.listErr
}

func (m *mockStripeSyncerDB) UpdateLastBillingSync(_ context.Context, orgID string, syncedAt time.Time) error {
	if m.updateSyncErr != nil {
		return m.updateSyncErr
	}
	if m.syncUpdated == nil {
		m.syncUpdated = make(map[string]time.Time)
	}
	m.syncUpdated[orgID] = syncedAt
	return nil
}

func (m *mockStripeSyncerDB) UpdateOrgBillingState(_ context.Context, orgID string, plan types.PlanTier, status types.SubscriptionStatus, eventTimestamp time.Time) error {
	if m.updateBillingErr != nil {
		return m.updateBillingErr
	}
	m.billingUpdates = append(m.billingUpdates, billingUpdate{
		OrgID:          orgID,
		Plan:           plan,
		Status:         status,
		EventTimestamp: eventTimestamp,
	})
	return nil
}

func (m *mockStripeSyncerDB) UpdateStripeCustomerID(_ context.Context, orgID string, customerID string) error {
	if m.updateCustomerErr != nil {
		return m.updateCustomerErr
	}
	if m.customerUpdates == nil {
		m.customerUpdates = make(map[string]string)
	}
	m.customerUpdates[orgID] = customerID
	return nil
}

type mockBillingClient struct {
	customerIDMap   map[string]string // orgID -> customerID
	subscriptionMap map[string]*types.SubscriptionDetails
	ensureErr       error
	getSubErr       error
}

func (m *mockBillingClient) EnsureCustomer(_ context.Context, orgID string) (string, error) {
	if m.ensureErr != nil {
		return "", m.ensureErr
	}
	if id, ok := m.customerIDMap[orgID]; ok {
		return id, nil
	}
	return "cus_new_" + orgID, nil
}

func (m *mockBillingClient) GetSubscription(_ context.Context, orgID string) (*types.SubscriptionDetails, error) {
	if m.getSubErr != nil {
		return nil, m.getSubErr
	}
	if m.subscriptionMap != nil {
		if sub, ok := m.subscriptionMap[orgID]; ok {
			return sub, nil
		}
	}
	return nil, nil
}

type mockSyncerMetrics struct {
	driftRecorded []string // orgIDs
}

func (m *mockSyncerMetrics) RecordBillingDrift(_ context.Context, orgID string) {
	m.driftRecorded = append(m.driftRecorded, orgID)
}

// --- SubscriptionEnforcer Mocks ---

type mockEnforcerDB struct {
	delinquentOrgs      []string
	listDelinquentErr   error
	overageOrgs         []OverageOrg
	listOverageErr      error
	pauseAllCalls       []pauseAllCall
	pauseAllErr         error
	activeCountMap      map[string]int
	countActiveErr      error
	pauseExcessCalls    []pauseExcessCall
	pauseExcessErr      error
}

type pauseAllCall struct {
	OrgID  string
	Reason types.PausedReason
}

type pauseExcessCall struct {
	OrgID string
	Limit int
}

func (m *mockEnforcerDB) ListDelinquentOrgs(_ context.Context, _ time.Time) ([]string, error) {
	return m.delinquentOrgs, m.listDelinquentErr
}

func (m *mockEnforcerDB) PauseAllByOrgID(_ context.Context, orgID string, reason types.PausedReason) error {
	if m.pauseAllErr != nil {
		return m.pauseAllErr
	}
	m.pauseAllCalls = append(m.pauseAllCalls, pauseAllCall{OrgID: orgID, Reason: reason})
	return nil
}

func (m *mockEnforcerDB) ListOverageOrgs(_ context.Context, _ time.Time) ([]OverageOrg, error) {
	return m.overageOrgs, m.listOverageErr
}

func (m *mockEnforcerDB) CountActiveWatchPoints(_ context.Context, orgID string) (int, error) {
	if m.countActiveErr != nil {
		return 0, m.countActiveErr
	}
	if m.activeCountMap != nil {
		if count, ok := m.activeCountMap[orgID]; ok {
			return count, nil
		}
	}
	return 0, nil
}

func (m *mockEnforcerDB) PauseExcessByOrgID(_ context.Context, orgID string, limit int) error {
	if m.pauseExcessErr != nil {
		return m.pauseExcessErr
	}
	m.pauseExcessCalls = append(m.pauseExcessCalls, pauseExcessCall{OrgID: orgID, Limit: limit})
	return nil
}

type mockEnforcerMetrics struct {
	enforcements []enforcementRecord
}

type enforcementRecord struct {
	OrgID  string
	Reason string
	Count  int
}

func (m *mockEnforcerMetrics) RecordBillingEnforcement(_ context.Context, orgID string, reason string, count int) {
	m.enforcements = append(m.enforcements, enforcementRecord{
		OrgID:  orgID,
		Reason: reason,
		Count:  count,
	})
}

// --- CanaryVerifier Mocks ---

type mockCanaryVerifierDB struct {
	canaries []CanaryWatchPoint
	listErr  error
}

func (m *mockCanaryVerifierDB) ListCanaryWatchPoints(_ context.Context) ([]CanaryWatchPoint, error) {
	return m.canaries, m.listErr
}

type mockCanaryMetrics struct {
	successes []string // watchpointIDs
	failures  []string // watchpointIDs
}

func (m *mockCanaryMetrics) RecordCanarySuccess(_ context.Context, watchpointID string) {
	m.successes = append(m.successes, watchpointID)
}

func (m *mockCanaryMetrics) RecordCanaryFailure(_ context.Context, watchpointID string) {
	m.failures = append(m.failures, watchpointID)
}

// =============================================================================
// StripeSyncer Tests
// =============================================================================

func TestStripeSyncer_SyncAtRisk_NoOrgs(t *testing.T) {
	db := &mockStripeSyncerDB{orgs: nil}
	billing := &mockBillingClient{}
	syncer := NewStripeSyncer(db, billing, nil, slog.Default())

	count, err := syncer.SyncAtRisk(context.Background(), time.Now().UTC(), 24*time.Hour, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 synced, got %d", count)
	}
}

func TestStripeSyncer_SyncAtRisk_ListError(t *testing.T) {
	db := &mockStripeSyncerDB{listErr: errors.New("db error")}
	syncer := NewStripeSyncer(db, &mockBillingClient{}, nil, slog.Default())

	_, err := syncer.SyncAtRisk(context.Background(), time.Now().UTC(), 24*time.Hour, 50)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestStripeSyncer_SyncAtRisk_HeadlessRepair(t *testing.T) {
	now := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	db := &mockStripeSyncerDB{
		orgs: []BillingSyncOrg{
			{ID: "org_1", StripeCustomerID: "", Plan: types.PlanFree, SubscriptionStatus: ""},
		},
	}
	billing := &mockBillingClient{
		customerIDMap: map[string]string{"org_1": "cus_repaired_1"},
	}
	metrics := &mockSyncerMetrics{}
	syncer := NewStripeSyncer(db, billing, metrics, slog.Default())

	count, err := syncer.SyncAtRisk(context.Background(), now, 24*time.Hour, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 synced, got %d", count)
	}

	// Verify customer ID was backfilled.
	if db.customerUpdates["org_1"] != "cus_repaired_1" {
		t.Errorf("expected customer update for org_1, got %v", db.customerUpdates)
	}

	// Verify last_billing_sync_at was updated.
	if _, ok := db.syncUpdated["org_1"]; !ok {
		t.Error("expected sync timestamp to be updated for org_1")
	}
}

func TestStripeSyncer_SyncAtRisk_StateDrift(t *testing.T) {
	now := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	db := &mockStripeSyncerDB{
		orgs: []BillingSyncOrg{
			{ID: "org_2", StripeCustomerID: "cus_existing", Plan: types.PlanFree, SubscriptionStatus: types.SubStatusActive},
		},
	}
	billing := &mockBillingClient{
		subscriptionMap: map[string]*types.SubscriptionDetails{
			"org_2": {
				Plan:   types.PlanPro,
				Status: types.SubStatusActive,
			},
		},
	}
	metrics := &mockSyncerMetrics{}
	syncer := NewStripeSyncer(db, billing, metrics, slog.Default())

	count, err := syncer.SyncAtRisk(context.Background(), now, 24*time.Hour, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 synced, got %d", count)
	}

	// Verify billing state was updated due to plan mismatch.
	if len(db.billingUpdates) != 1 {
		t.Fatalf("expected 1 billing update, got %d", len(db.billingUpdates))
	}
	if db.billingUpdates[0].Plan != types.PlanPro {
		t.Errorf("expected plan update to Pro, got %s", db.billingUpdates[0].Plan)
	}

	// Verify drift metric was emitted.
	if len(metrics.driftRecorded) != 1 || metrics.driftRecorded[0] != "org_2" {
		t.Errorf("expected drift metric for org_2, got %v", metrics.driftRecorded)
	}
}

func TestStripeSyncer_SyncAtRisk_NoMismatch(t *testing.T) {
	now := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	db := &mockStripeSyncerDB{
		orgs: []BillingSyncOrg{
			{ID: "org_3", StripeCustomerID: "cus_ok", Plan: types.PlanPro, SubscriptionStatus: types.SubStatusActive},
		},
	}
	billing := &mockBillingClient{
		subscriptionMap: map[string]*types.SubscriptionDetails{
			"org_3": {
				Plan:   types.PlanPro,
				Status: types.SubStatusActive,
			},
		},
	}
	metrics := &mockSyncerMetrics{}
	syncer := NewStripeSyncer(db, billing, metrics, slog.Default())

	count, err := syncer.SyncAtRisk(context.Background(), now, 24*time.Hour, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 synced, got %d", count)
	}

	// No billing updates since state matches.
	if len(db.billingUpdates) != 0 {
		t.Errorf("expected 0 billing updates, got %d", len(db.billingUpdates))
	}

	// No drift metric since state matches.
	if len(metrics.driftRecorded) != 0 {
		t.Errorf("expected 0 drift metrics, got %d", len(metrics.driftRecorded))
	}

	// Sync timestamp still updated.
	if _, ok := db.syncUpdated["org_3"]; !ok {
		t.Error("expected sync timestamp to be updated for org_3")
	}
}

func TestStripeSyncer_SyncAtRisk_HeadlessRepairFailure(t *testing.T) {
	now := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	db := &mockStripeSyncerDB{
		orgs: []BillingSyncOrg{
			{ID: "org_fail", StripeCustomerID: ""},
		},
	}
	billing := &mockBillingClient{
		ensureErr: errors.New("stripe unavailable"),
	}
	syncer := NewStripeSyncer(db, billing, nil, slog.Default())

	count, err := syncer.SyncAtRisk(context.Background(), now, 24*time.Hour, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v (should continue)", err)
	}
	// Org should be skipped (error logged), count = 0.
	if count != 0 {
		t.Errorf("expected 0 synced, got %d", count)
	}
}

func TestStripeSyncer_SyncAtRisk_NoSubscription(t *testing.T) {
	now := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	db := &mockStripeSyncerDB{
		orgs: []BillingSyncOrg{
			{ID: "org_free", StripeCustomerID: "cus_free", Plan: types.PlanFree},
		},
	}
	billing := &mockBillingClient{
		// Returns nil subscription for this org.
		subscriptionMap: map[string]*types.SubscriptionDetails{},
	}
	syncer := NewStripeSyncer(db, billing, nil, slog.Default())

	count, err := syncer.SyncAtRisk(context.Background(), now, 24*time.Hour, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 synced, got %d", count)
	}
	// No billing updates for orgs with no Stripe subscription.
	if len(db.billingUpdates) != 0 {
		t.Errorf("expected 0 billing updates, got %d", len(db.billingUpdates))
	}
}

// =============================================================================
// SubscriptionEnforcer Tests
// =============================================================================

func TestEnforcer_PaymentFailure_NoDelinquent(t *testing.T) {
	db := &mockEnforcerDB{delinquentOrgs: nil}
	enforcer := NewSubscriptionEnforcer(db, nil, slog.Default())

	count, err := enforcer.EnforcePaymentFailure(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 enforced, got %d", count)
	}
}

func TestEnforcer_PaymentFailure_ListError(t *testing.T) {
	db := &mockEnforcerDB{listDelinquentErr: errors.New("db error")}
	enforcer := NewSubscriptionEnforcer(db, nil, slog.Default())

	_, err := enforcer.EnforcePaymentFailure(context.Background(), time.Now().UTC())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestEnforcer_PaymentFailure_PausesAllActive(t *testing.T) {
	db := &mockEnforcerDB{
		delinquentOrgs: []string{"org_delinquent_1", "org_delinquent_2"},
	}
	metrics := &mockEnforcerMetrics{}
	enforcer := NewSubscriptionEnforcer(db, metrics, slog.Default())

	count, err := enforcer.EnforcePaymentFailure(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 enforced, got %d", count)
	}

	// Verify PauseAllByOrgID was called with correct reason.
	if len(db.pauseAllCalls) != 2 {
		t.Fatalf("expected 2 pause calls, got %d", len(db.pauseAllCalls))
	}
	for _, call := range db.pauseAllCalls {
		if call.Reason != types.PausedReasonBillingDelinquency {
			t.Errorf("expected reason %q, got %q", types.PausedReasonBillingDelinquency, call.Reason)
		}
	}

	// Verify metrics were emitted.
	if len(metrics.enforcements) != 2 {
		t.Errorf("expected 2 enforcement metrics, got %d", len(metrics.enforcements))
	}
}

func TestEnforcer_PaymentFailure_PauseError(t *testing.T) {
	db := &mockEnforcerDB{
		delinquentOrgs: []string{"org_1", "org_2"},
		pauseAllErr:    errors.New("pause failed"),
	}
	enforcer := NewSubscriptionEnforcer(db, nil, slog.Default())

	count, err := enforcer.EnforcePaymentFailure(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v (should continue)", err)
	}
	// Both should fail but processing should continue.
	if count != 0 {
		t.Errorf("expected 0 enforced, got %d", count)
	}
}

func TestEnforcer_Overage_NoOverage(t *testing.T) {
	db := &mockEnforcerDB{overageOrgs: nil}
	enforcer := NewSubscriptionEnforcer(db, nil, slog.Default())

	count, err := enforcer.EnforceOverage(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 enforced, got %d", count)
	}
}

func TestEnforcer_Overage_ListError(t *testing.T) {
	db := &mockEnforcerDB{listOverageErr: errors.New("db error")}
	enforcer := NewSubscriptionEnforcer(db, nil, slog.Default())

	_, err := enforcer.EnforceOverage(context.Background(), time.Now().UTC())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestEnforcer_Overage_LIFOPausing(t *testing.T) {
	db := &mockEnforcerDB{
		overageOrgs: []OverageOrg{
			{ID: "org_over", PlanLimits: types.PlanLimits{MaxWatchPoints: 25}},
		},
		activeCountMap: map[string]int{
			"org_over": 30, // 5 over the limit of 25
		},
	}
	metrics := &mockEnforcerMetrics{}
	enforcer := NewSubscriptionEnforcer(db, metrics, slog.Default())

	count, err := enforcer.EnforceOverage(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 enforced, got %d", count)
	}

	// Verify PauseExcessByOrgID was called with the plan limit (not the excess count).
	if len(db.pauseExcessCalls) != 1 {
		t.Fatalf("expected 1 pause excess call, got %d", len(db.pauseExcessCalls))
	}
	if db.pauseExcessCalls[0].OrgID != "org_over" {
		t.Errorf("expected org_over, got %s", db.pauseExcessCalls[0].OrgID)
	}
	if db.pauseExcessCalls[0].Limit != 25 {
		t.Errorf("expected limit 25, got %d", db.pauseExcessCalls[0].Limit)
	}

	// Verify enforcement metric.
	if len(metrics.enforcements) != 1 {
		t.Fatalf("expected 1 enforcement metric, got %d", len(metrics.enforcements))
	}
	if metrics.enforcements[0].Reason != "overage" {
		t.Errorf("expected reason 'overage', got %q", metrics.enforcements[0].Reason)
	}
	if metrics.enforcements[0].Count != 5 { // 30 - 25 = 5 excess
		t.Errorf("expected excess count 5, got %d", metrics.enforcements[0].Count)
	}
}

func TestEnforcer_Overage_SkipsUnlimitedPlan(t *testing.T) {
	db := &mockEnforcerDB{
		overageOrgs: []OverageOrg{
			{ID: "org_enterprise", PlanLimits: types.PlanLimits{MaxWatchPoints: 0}}, // Enterprise: unlimited
		},
	}
	enforcer := NewSubscriptionEnforcer(db, nil, slog.Default())

	count, err := enforcer.EnforceOverage(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 enforced, got %d", count)
	}
	if len(db.pauseExcessCalls) != 0 {
		t.Errorf("expected 0 pause excess calls for unlimited plan, got %d", len(db.pauseExcessCalls))
	}
}

func TestEnforcer_Overage_SkipsResolved(t *testing.T) {
	db := &mockEnforcerDB{
		overageOrgs: []OverageOrg{
			{ID: "org_resolved", PlanLimits: types.PlanLimits{MaxWatchPoints: 25}},
		},
		activeCountMap: map[string]int{
			"org_resolved": 20, // Under limit, overage resolved
		},
	}
	enforcer := NewSubscriptionEnforcer(db, nil, slog.Default())

	count, err := enforcer.EnforceOverage(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 enforced (overage resolved), got %d", count)
	}
	if len(db.pauseExcessCalls) != 0 {
		t.Errorf("expected 0 pause excess calls, got %d", len(db.pauseExcessCalls))
	}
}

func TestEnforcer_Overage_PauseExcessError(t *testing.T) {
	db := &mockEnforcerDB{
		overageOrgs: []OverageOrg{
			{ID: "org_err", PlanLimits: types.PlanLimits{MaxWatchPoints: 10}},
		},
		activeCountMap: map[string]int{
			"org_err": 20,
		},
		pauseExcessErr: errors.New("pause excess failed"),
	}
	enforcer := NewSubscriptionEnforcer(db, nil, slog.Default())

	count, err := enforcer.EnforceOverage(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v (should continue)", err)
	}
	if count != 0 {
		t.Errorf("expected 0 enforced due to error, got %d", count)
	}
}

func TestEnforcer_Overage_CountActiveError(t *testing.T) {
	db := &mockEnforcerDB{
		overageOrgs: []OverageOrg{
			{ID: "org_count_err", PlanLimits: types.PlanLimits{MaxWatchPoints: 10}},
		},
		countActiveErr: errors.New("count failed"),
	}
	enforcer := NewSubscriptionEnforcer(db, nil, slog.Default())

	count, err := enforcer.EnforceOverage(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v (should continue)", err)
	}
	if count != 0 {
		t.Errorf("expected 0 enforced due to count error, got %d", count)
	}
}

func TestEnforcer_Overage_ExactlyAtLimit(t *testing.T) {
	db := &mockEnforcerDB{
		overageOrgs: []OverageOrg{
			{ID: "org_exact", PlanLimits: types.PlanLimits{MaxWatchPoints: 25}},
		},
		activeCountMap: map[string]int{
			"org_exact": 25, // Exactly at limit, no excess
		},
	}
	enforcer := NewSubscriptionEnforcer(db, nil, slog.Default())

	count, err := enforcer.EnforceOverage(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 enforced (exactly at limit), got %d", count)
	}
	if len(db.pauseExcessCalls) != 0 {
		t.Errorf("expected 0 pause excess calls, got %d", len(db.pauseExcessCalls))
	}
}

func TestEnforcer_Overage_MultipleOrgs(t *testing.T) {
	db := &mockEnforcerDB{
		overageOrgs: []OverageOrg{
			{ID: "org_a", PlanLimits: types.PlanLimits{MaxWatchPoints: 5}},
			{ID: "org_b", PlanLimits: types.PlanLimits{MaxWatchPoints: 10}},
			{ID: "org_c", PlanLimits: types.PlanLimits{MaxWatchPoints: 3}},
		},
		activeCountMap: map[string]int{
			"org_a": 10,  // 5 over
			"org_b": 8,   // Under limit, resolved
			"org_c": 100, // 97 over
		},
	}
	metrics := &mockEnforcerMetrics{}
	enforcer := NewSubscriptionEnforcer(db, metrics, slog.Default())

	count, err := enforcer.EnforceOverage(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// org_a and org_c should be enforced. org_b resolved.
	if count != 2 {
		t.Errorf("expected 2 enforced, got %d", count)
	}
	if len(db.pauseExcessCalls) != 2 {
		t.Fatalf("expected 2 pause excess calls, got %d", len(db.pauseExcessCalls))
	}

	// Verify the limits passed to PauseExcessByOrgID.
	for _, call := range db.pauseExcessCalls {
		switch call.OrgID {
		case "org_a":
			if call.Limit != 5 {
				t.Errorf("expected limit 5 for org_a, got %d", call.Limit)
			}
		case "org_c":
			if call.Limit != 3 {
				t.Errorf("expected limit 3 for org_c, got %d", call.Limit)
			}
		default:
			t.Errorf("unexpected org in pause excess calls: %s", call.OrgID)
		}
	}
}

// =============================================================================
// CanaryVerifier Tests
// =============================================================================

func TestCanary_Verify_NoCanaries(t *testing.T) {
	db := &mockCanaryVerifierDB{canaries: nil}
	verifier := NewCanaryVerifier(db, nil, slog.Default())

	count, err := verifier.Verify(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 verified, got %d", count)
	}
}

func TestCanary_Verify_ListError(t *testing.T) {
	db := &mockCanaryVerifierDB{listErr: errors.New("db error")}
	verifier := NewCanaryVerifier(db, nil, slog.Default())

	_, err := verifier.Verify(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCanary_Verify_AllMatch(t *testing.T) {
	triggerTrue := true
	db := &mockCanaryVerifierDB{
		canaries: []CanaryWatchPoint{
			{
				ID:                   "canary_1",
				Metadata:             json.RawMessage(`{"system_role":"canary","canary_expect_trigger":true}`),
				PreviousTriggerState: &triggerTrue,
			},
			{
				ID:                   "canary_2",
				Metadata:             json.RawMessage(`{"system_role":"canary","canary_expect_trigger":false}`),
				PreviousTriggerState: nil, // NULL defaults to false
			},
		},
	}
	metrics := &mockCanaryMetrics{}
	verifier := NewCanaryVerifier(db, metrics, slog.Default())

	count, err := verifier.Verify(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 verified, got %d", count)
	}

	// All should be successes.
	if len(metrics.successes) != 2 {
		t.Errorf("expected 2 successes, got %d", len(metrics.successes))
	}
	if len(metrics.failures) != 0 {
		t.Errorf("expected 0 failures, got %d", len(metrics.failures))
	}
}

func TestCanary_Verify_Mismatch(t *testing.T) {
	triggerFalse := false
	db := &mockCanaryVerifierDB{
		canaries: []CanaryWatchPoint{
			{
				ID:                   "canary_bad",
				Metadata:             json.RawMessage(`{"system_role":"canary","canary_expect_trigger":true}`),
				PreviousTriggerState: &triggerFalse, // Expected true, got false
			},
		},
	}
	metrics := &mockCanaryMetrics{}
	verifier := NewCanaryVerifier(db, metrics, slog.Default())

	count, err := verifier.Verify(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 verified, got %d", count)
	}

	// Should be a failure.
	if len(metrics.failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(metrics.failures))
	}
	if metrics.failures[0] != "canary_bad" {
		t.Errorf("expected failure for canary_bad, got %s", metrics.failures[0])
	}
	if len(metrics.successes) != 0 {
		t.Errorf("expected 0 successes, got %d", len(metrics.successes))
	}
}

func TestCanary_Verify_NullTriggerStateDefaultsFalse(t *testing.T) {
	// When PreviousTriggerState is nil and expected is false, it should match.
	db := &mockCanaryVerifierDB{
		canaries: []CanaryWatchPoint{
			{
				ID:                   "canary_null",
				Metadata:             json.RawMessage(`{"system_role":"canary","canary_expect_trigger":false}`),
				PreviousTriggerState: nil,
			},
		},
	}
	metrics := &mockCanaryMetrics{}
	verifier := NewCanaryVerifier(db, metrics, slog.Default())

	count, err := verifier.Verify(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 verified, got %d", count)
	}
	if len(metrics.successes) != 1 {
		t.Errorf("expected 1 success, got %d", len(metrics.successes))
	}
}

func TestCanary_Verify_NullTriggerStateWithExpectedTrue(t *testing.T) {
	// When PreviousTriggerState is nil but expected is true, should be a mismatch.
	db := &mockCanaryVerifierDB{
		canaries: []CanaryWatchPoint{
			{
				ID:                   "canary_null_fail",
				Metadata:             json.RawMessage(`{"system_role":"canary","canary_expect_trigger":true}`),
				PreviousTriggerState: nil,
			},
		},
	}
	metrics := &mockCanaryMetrics{}
	verifier := NewCanaryVerifier(db, metrics, slog.Default())

	count, err := verifier.Verify(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 verified, got %d", count)
	}
	if len(metrics.failures) != 1 {
		t.Errorf("expected 1 failure, got %d", len(metrics.failures))
	}
}

func TestCanary_Verify_InvalidMetadata(t *testing.T) {
	db := &mockCanaryVerifierDB{
		canaries: []CanaryWatchPoint{
			{
				ID:       "canary_malformed",
				Metadata: json.RawMessage(`not valid json`),
			},
		},
	}
	metrics := &mockCanaryMetrics{}
	verifier := NewCanaryVerifier(db, metrics, slog.Default())

	count, err := verifier.Verify(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Malformed metadata should be skipped, not counted as verified.
	if count != 0 {
		t.Errorf("expected 0 verified (malformed skipped), got %d", count)
	}
	if len(metrics.successes) != 0 || len(metrics.failures) != 0 {
		t.Error("no metrics should be emitted for malformed canaries")
	}
}

func TestCanary_Verify_MixedResults(t *testing.T) {
	triggerTrue := true
	triggerFalse := false
	db := &mockCanaryVerifierDB{
		canaries: []CanaryWatchPoint{
			{
				ID:                   "canary_ok_1",
				Metadata:             json.RawMessage(`{"system_role":"canary","canary_expect_trigger":true}`),
				PreviousTriggerState: &triggerTrue,
			},
			{
				ID:                   "canary_fail_1",
				Metadata:             json.RawMessage(`{"system_role":"canary","canary_expect_trigger":true}`),
				PreviousTriggerState: &triggerFalse,
			},
			{
				ID:                   "canary_ok_2",
				Metadata:             json.RawMessage(`{"system_role":"canary","canary_expect_trigger":false}`),
				PreviousTriggerState: &triggerFalse,
			},
		},
	}
	metrics := &mockCanaryMetrics{}
	verifier := NewCanaryVerifier(db, metrics, slog.Default())

	count, err := verifier.Verify(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 verified, got %d", count)
	}
	if len(metrics.successes) != 2 {
		t.Errorf("expected 2 successes, got %d", len(metrics.successes))
	}
	if len(metrics.failures) != 1 {
		t.Errorf("expected 1 failure, got %d", len(metrics.failures))
	}
}

func TestCanary_Verify_NilMetrics(t *testing.T) {
	triggerTrue := true
	db := &mockCanaryVerifierDB{
		canaries: []CanaryWatchPoint{
			{
				ID:                   "canary_nometrics",
				Metadata:             json.RawMessage(`{"system_role":"canary","canary_expect_trigger":true}`),
				PreviousTriggerState: &triggerTrue,
			},
		},
	}
	// Pass nil metrics -- should not panic.
	verifier := NewCanaryVerifier(db, nil, slog.Default())

	count, err := verifier.Verify(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 verified, got %d", count)
	}
}

// =============================================================================
// Grace Period Constant Tests
// =============================================================================

func TestPaymentGracePeriod(t *testing.T) {
	if PaymentGracePeriod != 7*24*time.Hour {
		t.Errorf("expected PaymentGracePeriod to be 7 days, got %v", PaymentGracePeriod)
	}
}

func TestOverageGracePeriod(t *testing.T) {
	if OverageGracePeriod != 14*24*time.Hour {
		t.Errorf("expected OverageGracePeriod to be 14 days, got %v", OverageGracePeriod)
	}
}
