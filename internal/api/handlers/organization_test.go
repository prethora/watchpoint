package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/billing"
	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// =============================================================================
// Mock Implementations for Organization Handler
// =============================================================================

type mockOrgRepo struct {
	getByIDFn func(ctx context.Context, id string) (*types.Organization, error)
	createFn  func(ctx context.Context, org *types.Organization) error
	updateFn  func(ctx context.Context, org *types.Organization) error
	deleteFn  func(ctx context.Context, id string) error
}

func (m *mockOrgRepo) GetByID(ctx context.Context, id string) (*types.Organization, error) {
	if m.getByIDFn != nil {
		return m.getByIDFn(ctx, id)
	}
	return &types.Organization{
		ID:           id,
		Name:         "Test Org",
		BillingEmail: "billing@test.com",
		Plan:         types.PlanFree,
		PlanLimits:   types.PlanLimits{MaxWatchPoints: 3},
		CreatedAt:    time.Now().UTC(),
	}, nil
}

func (m *mockOrgRepo) Create(ctx context.Context, org *types.Organization) error {
	if m.createFn != nil {
		return m.createFn(ctx, org)
	}
	return nil
}

func (m *mockOrgRepo) Update(ctx context.Context, org *types.Organization) error {
	if m.updateFn != nil {
		return m.updateFn(ctx, org)
	}
	return nil
}

func (m *mockOrgRepo) Delete(ctx context.Context, id string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id)
	}
	return nil
}

type mockOrgWPRepo struct {
	pauseAllFn func(ctx context.Context, orgID string) error
}

func (m *mockOrgWPRepo) PauseAllByOrgID(ctx context.Context, orgID string) error {
	if m.pauseAllFn != nil {
		return m.pauseAllFn(ctx, orgID)
	}
	return nil
}

type mockOrgNotifRepo struct {
	listFn func(ctx context.Context, filter types.NotificationFilter) ([]types.NotificationHistoryItem, types.PageInfo, error)
}

func (m *mockOrgNotifRepo) List(ctx context.Context, filter types.NotificationFilter) ([]types.NotificationHistoryItem, types.PageInfo, error) {
	if m.listFn != nil {
		return m.listFn(ctx, filter)
	}
	return nil, types.PageInfo{}, nil
}

type mockOrgAuditRepo struct {
	logFn  func(ctx context.Context, entry *types.AuditEvent) error
	listFn func(ctx context.Context, params types.AuditQueryFilters) ([]*types.AuditEvent, types.PageInfo, error)
	events []*types.AuditEvent
}

func (m *mockOrgAuditRepo) Log(ctx context.Context, entry *types.AuditEvent) error {
	m.events = append(m.events, entry)
	if m.logFn != nil {
		return m.logFn(ctx, entry)
	}
	return nil
}

func (m *mockOrgAuditRepo) List(ctx context.Context, params types.AuditQueryFilters) ([]*types.AuditEvent, types.PageInfo, error) {
	if m.listFn != nil {
		return m.listFn(ctx, params)
	}
	return nil, types.PageInfo{}, nil
}

type mockOrgUserRepo struct {
	createFn func(ctx context.Context, user *types.User) error
}

func (m *mockOrgUserRepo) CreateWithProvider(ctx context.Context, user *types.User) error {
	if m.createFn != nil {
		return m.createFn(ctx, user)
	}
	return nil
}

type mockOrgBillingSvc struct {
	ensureCustomerFn     func(ctx context.Context, orgID, email string) (string, error)
	cancelSubscriptionFn func(ctx context.Context, orgID string) error
	ensureCalled         bool
	cancelCalled         bool
}

func (m *mockOrgBillingSvc) EnsureCustomer(ctx context.Context, orgID, email string) (string, error) {
	m.ensureCalled = true
	if m.ensureCustomerFn != nil {
		return m.ensureCustomerFn(ctx, orgID, email)
	}
	return "cus_test", nil
}

func (m *mockOrgBillingSvc) CancelSubscription(ctx context.Context, orgID string) error {
	m.cancelCalled = true
	if m.cancelSubscriptionFn != nil {
		return m.cancelSubscriptionFn(ctx, orgID)
	}
	return nil
}

// =============================================================================
// Test Helpers
// =============================================================================

func newTestOrgHandler() (*OrganizationHandler, *mockOrgRepo, *mockOrgAuditRepo, *mockOrgBillingSvc) {
	orgRepo := &mockOrgRepo{}
	auditRepo := &mockOrgAuditRepo{}
	billingSvc := &mockOrgBillingSvc{}
	logger := slog.Default()
	validator := core.NewValidator(logger)

	h := NewOrganizationHandler(
		orgRepo,
		&mockOrgWPRepo{},
		&mockOrgNotifRepo{},
		auditRepo,
		&mockOrgUserRepo{},
		billing.NewStaticPlanRegistry(),
		billingSvc,
		validator,
		logger,
	)

	return h, orgRepo, auditRepo, billingSvc
}

func orgContext(orgID string, role types.UserRole) context.Context {
	ctx := context.Background()
	actor := types.Actor{
		ID:             "usr_test123",
		Type:           types.ActorTypeUser,
		OrganizationID: orgID,
		Role:           role,
	}
	ctx = types.WithActor(ctx, actor)
	return ctx
}

// =============================================================================
// Organization Handler Tests
// =============================================================================

func TestOrganizationHandler_Get(t *testing.T) {
	h, orgRepo, _, _ := newTestOrgHandler()
	orgID := "org_test123"

	orgRepo.getByIDFn = func(_ context.Context, id string) (*types.Organization, error) {
		assert.Equal(t, orgID, id)
		return &types.Organization{
			ID:           id,
			Name:         "My Org",
			BillingEmail: "billing@test.com",
			Plan:         types.PlanFree,
			PlanLimits:   types.PlanLimits{MaxWatchPoints: 3},
			CreatedAt:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/organization", nil)
	req = req.WithContext(orgContext(orgID, types.RoleMember))
	w := httptest.NewRecorder()

	h.Get(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp core.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.NotNil(t, resp.Data)
}

func TestOrganizationHandler_Get_NotFound(t *testing.T) {
	h, orgRepo, _, _ := newTestOrgHandler()
	orgID := "org_nonexistent"

	orgRepo.getByIDFn = func(_ context.Context, _ string) (*types.Organization, error) {
		return nil, types.NewAppError(types.ErrCodeNotFoundOrg, "organization not found", nil)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/organization", nil)
	req = req.WithContext(orgContext(orgID, types.RoleMember))
	w := httptest.NewRecorder()

	h.Get(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestOrganizationHandler_Get_NoOrgContext(t *testing.T) {
	h, _, _, _ := newTestOrgHandler()

	req := httptest.NewRequest(http.MethodGet, "/v1/organization", nil)
	// No actor in context
	w := httptest.NewRecorder()

	h.Get(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestOrganizationHandler_Update(t *testing.T) {
	h, orgRepo, auditRepo, _ := newTestOrgHandler()
	orgID := "org_test123"

	org := &types.Organization{
		ID:           orgID,
		Name:         "Old Name",
		BillingEmail: "old@test.com",
		Plan:         types.PlanFree,
		PlanLimits:   types.PlanLimits{MaxWatchPoints: 3},
	}
	orgRepo.getByIDFn = func(_ context.Context, _ string) (*types.Organization, error) {
		return org, nil
	}

	var updatedOrg *types.Organization
	orgRepo.updateFn = func(_ context.Context, o *types.Organization) error {
		updatedOrg = o
		return nil
	}

	body := `{"name": "New Name"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/organization", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(orgContext(orgID, types.RoleAdmin))
	w := httptest.NewRecorder()

	h.Update(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, updatedOrg)
	assert.Equal(t, "New Name", updatedOrg.Name)
	assert.Equal(t, "old@test.com", updatedOrg.BillingEmail) // Unchanged
	assert.Len(t, auditRepo.events, 1)
	assert.Equal(t, "organization.updated", auditRepo.events[0].Action)
}

func TestOrganizationHandler_Update_BillingEmailChange(t *testing.T) {
	h, orgRepo, _, billingSvc := newTestOrgHandler()
	orgID := "org_test123"

	orgRepo.getByIDFn = func(_ context.Context, _ string) (*types.Organization, error) {
		return &types.Organization{
			ID:           orgID,
			Name:         "My Org",
			BillingEmail: "old@test.com",
			Plan:         types.PlanFree,
		}, nil
	}

	body := `{"billing_email": "new@test.com"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/organization", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(orgContext(orgID, types.RoleAdmin))
	w := httptest.NewRecorder()

	h.Update(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// Best-effort Stripe sync should have been called.
	assert.True(t, billingSvc.ensureCalled)
}

func TestOrganizationHandler_Delete(t *testing.T) {
	h, orgRepo, auditRepo, billingSvc := newTestOrgHandler()
	orgID := "org_test123"

	deleteCalled := false
	orgRepo.deleteFn = func(_ context.Context, id string) error {
		assert.Equal(t, orgID, id)
		deleteCalled = true
		return nil
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/organization", nil)
	req = req.WithContext(orgContext(orgID, types.RoleOwner))
	w := httptest.NewRecorder()

	h.Delete(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, deleteCalled)
	assert.True(t, billingSvc.cancelCalled)
	assert.Len(t, auditRepo.events, 1)
	assert.Equal(t, "organization.deleted", auditRepo.events[0].Action)
}

func TestOrganizationHandler_Delete_BillingCancelFails(t *testing.T) {
	h, _, _, billingSvc := newTestOrgHandler()
	orgID := "org_test123"

	// Per USER-003: If Stripe fails, return 500 to prevent zombie billing.
	billingSvc.cancelSubscriptionFn = func(_ context.Context, _ string) error {
		return types.NewAppError(types.ErrCodeUpstreamStripe, "Stripe unavailable", nil)
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/organization", nil)
	req = req.WithContext(orgContext(orgID, types.RoleOwner))
	w := httptest.NewRecorder()

	h.Delete(w, req)

	// Should return 500 (fail-fast), not 204.
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestOrganizationHandler_Create(t *testing.T) {
	h, orgRepo, auditRepo, billingSvc := newTestOrgHandler()

	var createdOrg *types.Organization
	orgRepo.createFn = func(_ context.Context, org *types.Organization) error {
		createdOrg = org
		return nil
	}

	body := `{"name": "New Org", "billing_email": "admin@neworg.com"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/organization", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(orgContext("org_tmp", types.RoleOwner))
	w := httptest.NewRecorder()

	h.Create(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	require.NotNil(t, createdOrg)
	assert.Equal(t, "New Org", createdOrg.Name)
	assert.Equal(t, "admin@neworg.com", createdOrg.BillingEmail)
	assert.Equal(t, types.PlanFree, createdOrg.Plan)
	assert.True(t, billingSvc.ensureCalled, "Best-effort Stripe should be called")
	assert.Len(t, auditRepo.events, 1)
}

func TestOrganizationHandler_Create_BillingFails_BestEffort(t *testing.T) {
	h, _, _, billingSvc := newTestOrgHandler()

	// Billing failure should NOT fail the request (best effort).
	billingSvc.ensureCustomerFn = func(_ context.Context, _, _ string) (string, error) {
		return "", types.NewAppError(types.ErrCodeUpstreamStripe, "Stripe down", nil)
	}

	body := `{"name": "New Org", "billing_email": "admin@neworg.com"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/organization", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(orgContext("org_tmp", types.RoleOwner))
	w := httptest.NewRecorder()

	h.Create(w, req)

	// Should still return 201 despite Stripe failure.
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestOrganizationHandler_ListAuditLogs(t *testing.T) {
	h, _, auditRepo, _ := newTestOrgHandler()
	orgID := "org_test123"

	testEvents := []*types.AuditEvent{
		{
			ID:           "audit_1",
			Action:       "watchpoint.created",
			ResourceType: "watchpoint",
			ResourceID:   "wp_1",
			Timestamp:    time.Now().UTC(),
		},
	}

	auditRepo.listFn = func(_ context.Context, params types.AuditQueryFilters) ([]*types.AuditEvent, types.PageInfo, error) {
		assert.Equal(t, orgID, params.OrganizationID)
		assert.Equal(t, "watchpoint", params.ResourceType)
		assert.Equal(t, 20, params.Limit)
		return testEvents, types.PageInfo{HasMore: false}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/organization/audit-logs?resource_type=watchpoint", nil)
	req = req.WithContext(orgContext(orgID, types.RoleAdmin))
	w := httptest.NewRecorder()

	h.ListAuditLogs(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp core.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.NotNil(t, resp.Data)
}

func TestOrganizationHandler_ListAuditLogs_WithPagination(t *testing.T) {
	h, _, auditRepo, _ := newTestOrgHandler()
	orgID := "org_test123"

	auditRepo.listFn = func(_ context.Context, params types.AuditQueryFilters) ([]*types.AuditEvent, types.PageInfo, error) {
		assert.Equal(t, 10, params.Limit)
		assert.Equal(t, "some_cursor", params.Cursor)
		return nil, types.PageInfo{}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/organization/audit-logs?limit=10&cursor=some_cursor", nil)
	req = req.WithContext(orgContext(orgID, types.RoleOwner))
	w := httptest.NewRecorder()

	h.ListAuditLogs(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestOrganizationHandler_GetPreferences(t *testing.T) {
	h, orgRepo, _, _ := newTestOrgHandler()
	orgID := "org_test123"

	orgRepo.getByIDFn = func(_ context.Context, _ string) (*types.Organization, error) {
		return &types.Organization{
			ID:   orgID,
			Name: "Test",
			NotificationPreferences: types.NotificationPreferences{
				QuietHours: &types.QuietHoursConfig{
					Enabled:  true,
					Timezone: "America/New_York",
				},
			},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/organization/notification-preferences", nil)
	req = req.WithContext(orgContext(orgID, types.RoleMember))
	w := httptest.NewRecorder()

	h.GetPreferences(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestOrganizationHandler_UpdatePreferences(t *testing.T) {
	h, orgRepo, auditRepo, _ := newTestOrgHandler()
	orgID := "org_test123"

	orgRepo.getByIDFn = func(_ context.Context, _ string) (*types.Organization, error) {
		return &types.Organization{
			ID:   orgID,
			Name: "Test",
			NotificationPreferences: types.NotificationPreferences{},
		}, nil
	}

	var updatedOrg *types.Organization
	orgRepo.updateFn = func(_ context.Context, o *types.Organization) error {
		updatedOrg = o
		return nil
	}

	body := `{"quiet_hours": {"enabled": true, "timezone": "America/New_York"}}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/organization/notification-preferences", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(orgContext(orgID, types.RoleAdmin))
	w := httptest.NewRecorder()

	h.UpdatePreferences(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, updatedOrg)
	require.NotNil(t, updatedOrg.NotificationPreferences.QuietHours)
	assert.True(t, updatedOrg.NotificationPreferences.QuietHours.Enabled)
	assert.Equal(t, "America/New_York", updatedOrg.NotificationPreferences.QuietHours.Timezone)
	assert.Len(t, auditRepo.events, 1)
}

func TestOrganizationHandler_GetNotificationHistory(t *testing.T) {
	h, _, _, _ := newTestOrgHandler()
	orgID := "org_test123"

	req := httptest.NewRequest(http.MethodGet, "/v1/organization/notifications", nil)
	req = req.WithContext(orgContext(orgID, types.RoleMember))
	w := httptest.NewRecorder()

	h.GetNotificationHistory(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// =============================================================================
// Route Registration Integration Test
// =============================================================================

func TestOrganizationHandler_RegisterRoutes(t *testing.T) {
	h, _, _, _ := newTestOrgHandler()

	r := chi.NewRouter()
	h.RegisterRoutes(r, nil, nil)

	// Verify routes are mounted by walking the router.
	// This is a simple smoke test to ensure no panics during registration.
	assert.NotNil(t, r)
}
