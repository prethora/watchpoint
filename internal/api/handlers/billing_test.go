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

	"watchpoint/internal/config"
	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// =============================================================================
// Mock Implementations
// =============================================================================

// mockBillingService implements BillingService for testing.
type mockBillingService struct {
	ensureCustomerFn         func(ctx context.Context, orgID, email string) (string, error)
	createCheckoutSessionFn  func(ctx context.Context, orgID string, plan types.PlanTier, urls types.RedirectURLs) (string, string, error)
	createPortalSessionFn    func(ctx context.Context, orgID, returnURL string) (string, error)
	getInvoicesFn            func(ctx context.Context, orgID string, params types.ListInvoicesParams) ([]*types.Invoice, types.PageInfo, error)
	getSubscriptionFn        func(ctx context.Context, orgID string) (*types.SubscriptionDetails, error)
}

func (m *mockBillingService) EnsureCustomer(ctx context.Context, orgID, email string) (string, error) {
	if m.ensureCustomerFn != nil {
		return m.ensureCustomerFn(ctx, orgID, email)
	}
	return "cus_test", nil
}

func (m *mockBillingService) CreateCheckoutSession(ctx context.Context, orgID string, plan types.PlanTier, urls types.RedirectURLs) (string, string, error) {
	if m.createCheckoutSessionFn != nil {
		return m.createCheckoutSessionFn(ctx, orgID, plan, urls)
	}
	return "https://checkout.stripe.com/test", "cs_test_123", nil
}

func (m *mockBillingService) CreatePortalSession(ctx context.Context, orgID, returnURL string) (string, error) {
	if m.createPortalSessionFn != nil {
		return m.createPortalSessionFn(ctx, orgID, returnURL)
	}
	return "https://billing.stripe.com/portal/test", nil
}

func (m *mockBillingService) GetInvoices(ctx context.Context, orgID string, params types.ListInvoicesParams) ([]*types.Invoice, types.PageInfo, error) {
	if m.getInvoicesFn != nil {
		return m.getInvoicesFn(ctx, orgID, params)
	}
	return nil, types.PageInfo{}, nil
}

func (m *mockBillingService) GetSubscription(ctx context.Context, orgID string) (*types.SubscriptionDetails, error) {
	if m.getSubscriptionFn != nil {
		return m.getSubscriptionFn(ctx, orgID)
	}
	return &types.SubscriptionDetails{
		Plan:   types.PlanPro,
		Status: types.SubStatusActive,
	}, nil
}

// mockUsageReporter implements UsageReporter for testing.
type mockUsageReporter struct {
	getCurrentUsageFn  func(ctx context.Context, orgID string) (*types.UsageSnapshot, error)
	getUsageHistoryFn  func(ctx context.Context, orgID string, start, end time.Time, granularity types.TimeGranularity) ([]*types.UsageDataPoint, error)
}

func (m *mockUsageReporter) GetCurrentUsage(ctx context.Context, orgID string) (*types.UsageSnapshot, error) {
	if m.getCurrentUsageFn != nil {
		return m.getCurrentUsageFn(ctx, orgID)
	}
	return &types.UsageSnapshot{
		ResourceUsage: map[types.ResourceType]int{
			types.ResourceWatchPoints: 5,
			types.ResourceAPICalls:    42,
		},
		LimitDetails: map[types.ResourceType]types.LimitDetail{
			types.ResourceWatchPoints: {Limit: 100, Used: 5, ResetType: types.ResetNever},
			types.ResourceAPICalls:    {Limit: 1000, Used: 42, ResetType: types.ResetDaily},
		},
	}, nil
}

func (m *mockUsageReporter) GetUsageHistory(ctx context.Context, orgID string, start, end time.Time, granularity types.TimeGranularity) ([]*types.UsageDataPoint, error) {
	if m.getUsageHistoryFn != nil {
		return m.getUsageHistoryFn(ctx, orgID, start, end, granularity)
	}
	return []*types.UsageDataPoint{
		{Timestamp: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Value: 10},
		{Timestamp: time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC), Value: 20},
	}, nil
}

// mockAuditLogger implements AuditLogger for testing.
type mockAuditLogger struct {
	logFn func(ctx context.Context, event types.AuditEvent) error
	calls []types.AuditEvent
}

func (m *mockAuditLogger) Log(ctx context.Context, event types.AuditEvent) error {
	m.calls = append(m.calls, event)
	if m.logFn != nil {
		return m.logFn(ctx, event)
	}
	return nil
}

// mockOrgBillingReader implements OrgBillingReader for testing.
type mockOrgBillingReader struct {
	getByIDFn func(ctx context.Context, orgID string) (*types.Organization, error)
}

func (m *mockOrgBillingReader) GetByID(ctx context.Context, orgID string) (*types.Organization, error) {
	if m.getByIDFn != nil {
		return m.getByIDFn(ctx, orgID)
	}
	return &types.Organization{
		ID:           orgID,
		Name:         "Test Org",
		BillingEmail: "billing@test.com",
		Plan:         types.PlanPro,
	}, nil
}

// Compile-time interface assertions for mocks.
var (
	_ BillingService  = (*mockBillingService)(nil)
	_ UsageReporter   = (*mockUsageReporter)(nil)
	_ AuditLogger     = (*mockAuditLogger)(nil)
	_ OrgBillingReader = (*mockOrgBillingReader)(nil)
)

// =============================================================================
// Test Helpers
// =============================================================================

func newTestBillingHandler(
	svc BillingService,
	orgRepo OrgBillingReader,
	reporter UsageReporter,
	audit AuditLogger,
) *BillingHandler {
	logger := slog.Default()
	validator := core.NewValidator(logger)

	cfg := &config.Config{}
	cfg.Server.DashboardURL = "https://app.watchpoint.io"

	usageHandler := NewUsageHandler(reporter, orgRepo, validator)

	return NewBillingHandler(svc, orgRepo, usageHandler, audit, cfg, validator, logger)
}

func newDefaultTestBillingHandler() *BillingHandler {
	return newTestBillingHandler(
		&mockBillingService{},
		&mockOrgBillingReader{},
		&mockUsageReporter{},
		&mockAuditLogger{},
	)
}

// contextWithActor creates a context with an authenticated user Actor.
func contextWithActor(orgID string, role types.UserRole) context.Context {
	ctx := context.Background()
	ctx = types.WithRequestID(ctx, "req_test_123")
	actor := types.Actor{
		ID:             "user_test_123",
		Type:           types.ActorTypeUser,
		OrganizationID: orgID,
		Role:           role,
	}
	return types.WithActor(ctx, actor)
}

// makeRequest creates an HTTP request with the given method, path, body, and context.
func makeRequest(method, path string, body interface{}, ctx context.Context) *http.Request {
	var reqBody *bytes.Buffer
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewBuffer(data)
	} else {
		reqBody = bytes.NewBuffer(nil)
	}

	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	return req
}

// parseJSONResponse decodes the response body into the given target.
func parseJSONResponse(t *testing.T, rr *httptest.ResponseRecorder, target interface{}) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), target); err != nil {
		t.Fatalf("failed to parse response body: %v\nbody: %s", err, rr.Body.String())
	}
}

// =============================================================================
// CreateCheckoutSession Tests
// =============================================================================

func TestCreateCheckoutSession_Success(t *testing.T) {
	var capturedURLs types.RedirectURLs
	svc := &mockBillingService{
		createCheckoutSessionFn: func(ctx context.Context, orgID string, plan types.PlanTier, urls types.RedirectURLs) (string, string, error) {
			capturedURLs = urls
			return "https://checkout.stripe.com/test_session", "cs_test_abc", nil
		},
	}

	h := newTestBillingHandler(svc, &mockOrgBillingReader{}, &mockUsageReporter{}, &mockAuditLogger{})

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	body := CreateCheckoutRequest{Plan: types.PlanPro}
	req := makeRequest("POST", "/v1/billing/checkout-session", body, ctx)
	rr := httptest.NewRecorder()

	h.CreateCheckoutSession(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify response structure.
	var resp struct {
		Data CheckoutResponse `json:"data"`
	}
	parseJSONResponse(t, rr, &resp)

	if resp.Data.CheckoutURL != "https://checkout.stripe.com/test_session" {
		t.Errorf("expected checkout URL 'https://checkout.stripe.com/test_session', got %q", resp.Data.CheckoutURL)
	}
	if resp.Data.SessionID != "cs_test_abc" {
		t.Errorf("expected session ID 'cs_test_abc', got %q", resp.Data.SessionID)
	}
	if resp.Data.ExpiresAt.IsZero() {
		t.Error("expected non-zero ExpiresAt")
	}

	// Verify server-controlled URLs use DashboardURL.
	if capturedURLs.Success != "https://app.watchpoint.io/billing?success=true" {
		t.Errorf("expected success URL with DashboardURL prefix, got %q", capturedURLs.Success)
	}
	if capturedURLs.Cancel != "https://app.watchpoint.io/billing?canceled=true" {
		t.Errorf("expected cancel URL with DashboardURL prefix, got %q", capturedURLs.Cancel)
	}
}

func TestCreateCheckoutSession_RejectsFreePlan(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	body := CreateCheckoutRequest{Plan: types.PlanFree}
	req := makeRequest("POST", "/v1/billing/checkout-session", body, ctx)
	rr := httptest.NewRecorder()

	h.CreateCheckoutSession(rr, req)

	// The validator rejects free plan because the validate tag is "oneof=starter pro business".
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for free plan, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateCheckoutSession_NoOrgContext(t *testing.T) {
	h := newDefaultTestBillingHandler()

	// Context without actor (no org ID).
	ctx := types.WithRequestID(context.Background(), "req_test_no_org")
	body := CreateCheckoutRequest{Plan: types.PlanPro}
	req := makeRequest("POST", "/v1/billing/checkout-session", body, ctx)
	rr := httptest.NewRecorder()

	h.CreateCheckoutSession(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for missing org context, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateCheckoutSession_EnsureCustomerFailure(t *testing.T) {
	svc := &mockBillingService{
		ensureCustomerFn: func(ctx context.Context, orgID, email string) (string, error) {
			return "", types.NewAppError(types.ErrCodeUpstreamStripe, "Stripe unavailable", nil)
		},
	}

	h := newTestBillingHandler(svc, &mockOrgBillingReader{}, &mockUsageReporter{}, &mockAuditLogger{})

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	body := CreateCheckoutRequest{Plan: types.PlanPro}
	req := makeRequest("POST", "/v1/billing/checkout-session", body, ctx)
	rr := httptest.NewRecorder()

	h.CreateCheckoutSession(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("expected status 502 for Stripe error, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateCheckoutSession_AuditEventLogged(t *testing.T) {
	audit := &mockAuditLogger{}
	h := newTestBillingHandler(&mockBillingService{}, &mockOrgBillingReader{}, &mockUsageReporter{}, audit)

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	body := CreateCheckoutRequest{Plan: types.PlanStarter}
	req := makeRequest("POST", "/v1/billing/checkout-session", body, ctx)
	rr := httptest.NewRecorder()

	h.CreateCheckoutSession(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if len(audit.calls) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(audit.calls))
	}
	if audit.calls[0].Action != "billing.checkout.created" {
		t.Errorf("expected audit action 'billing.checkout.created', got %q", audit.calls[0].Action)
	}
	if audit.calls[0].ResourceID != "org_test_1" {
		t.Errorf("expected audit resource ID 'org_test_1', got %q", audit.calls[0].ResourceID)
	}
}

func TestCreateCheckoutSession_InvalidJSON(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	req := httptest.NewRequest("POST", "/v1/billing/checkout-session", bytes.NewBufferString("{invalid}"))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()

	h.CreateCheckoutSession(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for invalid JSON, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateCheckoutSession_EmptyBody(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	req := httptest.NewRequest("POST", "/v1/billing/checkout-session", bytes.NewBufferString(""))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()

	h.CreateCheckoutSession(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for empty body, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateCheckoutSession_OrgNotFound(t *testing.T) {
	orgRepo := &mockOrgBillingReader{
		getByIDFn: func(ctx context.Context, orgID string) (*types.Organization, error) {
			return nil, types.NewAppError(types.ErrCodeNotFoundOrg, "organization not found", nil)
		},
	}

	h := newTestBillingHandler(&mockBillingService{}, orgRepo, &mockUsageReporter{}, &mockAuditLogger{})

	ctx := contextWithActor("org_nonexistent", types.RoleAdmin)
	body := CreateCheckoutRequest{Plan: types.PlanPro}
	req := makeRequest("POST", "/v1/billing/checkout-session", body, ctx)
	rr := httptest.NewRecorder()

	h.CreateCheckoutSession(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected status 404 for org not found, got %d: %s", rr.Code, rr.Body.String())
	}
}

// =============================================================================
// CreatePortalSession Tests
// =============================================================================

func TestCreatePortalSession_Success(t *testing.T) {
	var capturedReturnURL string
	svc := &mockBillingService{
		createPortalSessionFn: func(ctx context.Context, orgID, returnURL string) (string, error) {
			capturedReturnURL = returnURL
			return "https://billing.stripe.com/portal/test_session", nil
		},
	}

	h := newTestBillingHandler(svc, &mockOrgBillingReader{}, &mockUsageReporter{}, &mockAuditLogger{})

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	// Portal session request is an empty POST.
	req := makeRequest("POST", "/v1/billing/portal-session", nil, ctx)
	rr := httptest.NewRecorder()

	h.CreatePortalSession(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Data PortalResponse `json:"data"`
	}
	parseJSONResponse(t, rr, &resp)

	if resp.Data.PortalURL != "https://billing.stripe.com/portal/test_session" {
		t.Errorf("expected portal URL, got %q", resp.Data.PortalURL)
	}

	// Verify server-controlled return URL.
	if capturedReturnURL != "https://app.watchpoint.io/billing" {
		t.Errorf("expected return URL with DashboardURL prefix, got %q", capturedReturnURL)
	}
}

func TestCreatePortalSession_NoOrgContext(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := types.WithRequestID(context.Background(), "req_test")
	req := makeRequest("POST", "/v1/billing/portal-session", nil, ctx)
	rr := httptest.NewRecorder()

	h.CreatePortalSession(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreatePortalSession_EnsureCustomerFailure(t *testing.T) {
	svc := &mockBillingService{
		ensureCustomerFn: func(ctx context.Context, orgID, email string) (string, error) {
			return "", types.NewAppError(types.ErrCodeUpstreamStripe, "Stripe error", nil)
		},
	}

	h := newTestBillingHandler(svc, &mockOrgBillingReader{}, &mockUsageReporter{}, &mockAuditLogger{})

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	req := makeRequest("POST", "/v1/billing/portal-session", nil, ctx)
	rr := httptest.NewRecorder()

	h.CreatePortalSession(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("expected status 502 for Stripe error, got %d: %s", rr.Code, rr.Body.String())
	}
}

// =============================================================================
// GetInvoices Tests
// =============================================================================

func TestGetInvoices_Success(t *testing.T) {
	now := time.Now().UTC()
	svc := &mockBillingService{
		getInvoicesFn: func(ctx context.Context, orgID string, params types.ListInvoicesParams) ([]*types.Invoice, types.PageInfo, error) {
			return []*types.Invoice{
				{
					ID:          "inv_test_1",
					AmountCents: 2999,
					Status:      "paid",
					PeriodStart: now.AddDate(0, -1, 0),
					PeriodEnd:   now,
					PDFURL:      "https://stripe.com/invoice.pdf",
				},
			}, types.PageInfo{HasMore: false}, nil
		},
	}

	h := newTestBillingHandler(svc, &mockOrgBillingReader{}, &mockUsageReporter{}, &mockAuditLogger{})

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	req := makeRequest("GET", "/v1/billing/invoices", nil, ctx)
	rr := httptest.NewRecorder()

	h.GetInvoices(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Data []*types.Invoice   `json:"data"`
		Meta *types.ResponseMeta `json:"meta"`
	}
	parseJSONResponse(t, rr, &resp)

	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 invoice, got %d", len(resp.Data))
	}
	if resp.Data[0].ID != "inv_test_1" {
		t.Errorf("expected invoice ID 'inv_test_1', got %q", resp.Data[0].ID)
	}
}

func TestGetInvoices_WithPagination(t *testing.T) {
	svc := &mockBillingService{
		getInvoicesFn: func(ctx context.Context, orgID string, params types.ListInvoicesParams) ([]*types.Invoice, types.PageInfo, error) {
			if params.Limit != 10 {
				t.Errorf("expected limit 10, got %d", params.Limit)
			}
			if params.Cursor != "inv_prev" {
				t.Errorf("expected cursor 'inv_prev', got %q", params.Cursor)
			}
			return nil, types.PageInfo{HasMore: true, NextCursor: "inv_next"}, nil
		},
	}

	h := newTestBillingHandler(svc, &mockOrgBillingReader{}, &mockUsageReporter{}, &mockAuditLogger{})

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	req := makeRequest("GET", "/v1/billing/invoices?limit=10&cursor=inv_prev", nil, ctx)
	rr := httptest.NewRecorder()

	h.GetInvoices(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetInvoices_InvalidLimit(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	req := makeRequest("GET", "/v1/billing/invoices?limit=abc", nil, ctx)
	rr := httptest.NewRecorder()

	h.GetInvoices(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for invalid limit, got %d: %s", rr.Code, rr.Body.String())
	}
}

// =============================================================================
// GetSubscription Tests
// =============================================================================

func TestGetSubscription_Success(t *testing.T) {
	svc := &mockBillingService{
		getSubscriptionFn: func(ctx context.Context, orgID string) (*types.SubscriptionDetails, error) {
			return &types.SubscriptionDetails{
				Plan:               types.PlanPro,
				Status:             types.SubStatusActive,
				CurrentPeriodStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				CurrentPeriodEnd:   time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			}, nil
		},
	}

	h := newTestBillingHandler(svc, &mockOrgBillingReader{}, &mockUsageReporter{}, &mockAuditLogger{})

	ctx := contextWithActor("org_test_1", types.RoleMember)
	req := makeRequest("GET", "/v1/billing/subscription", nil, ctx)
	rr := httptest.NewRecorder()

	h.GetSubscription(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Data types.SubscriptionDetails `json:"data"`
	}
	parseJSONResponse(t, rr, &resp)

	if resp.Data.Plan != types.PlanPro {
		t.Errorf("expected plan 'pro', got %q", resp.Data.Plan)
	}
	if resp.Data.Status != types.SubStatusActive {
		t.Errorf("expected status 'active', got %q", resp.Data.Status)
	}
}

func TestGetSubscription_ServiceError(t *testing.T) {
	svc := &mockBillingService{
		getSubscriptionFn: func(ctx context.Context, orgID string) (*types.SubscriptionDetails, error) {
			return nil, types.NewAppError(types.ErrCodeUpstreamStripe, "Stripe error", nil)
		},
	}

	h := newTestBillingHandler(svc, &mockOrgBillingReader{}, &mockUsageReporter{}, &mockAuditLogger{})

	ctx := contextWithActor("org_test_1", types.RoleMember)
	req := makeRequest("GET", "/v1/billing/subscription", nil, ctx)
	rr := httptest.NewRecorder()

	h.GetSubscription(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d: %s", rr.Code, rr.Body.String())
	}
}

// =============================================================================
// GetCurrent (Usage) Tests
// =============================================================================

func TestGetCurrentUsage_Success(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := contextWithActor("org_test_1", types.RoleMember)
	req := makeRequest("GET", "/v1/usage", nil, ctx)
	rr := httptest.NewRecorder()

	h.usageHandler.GetCurrent(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Data UsageResponse `json:"data"`
	}
	parseJSONResponse(t, rr, &resp)

	if resp.Data.Plan != types.PlanPro {
		t.Errorf("expected plan 'pro', got %q", resp.Data.Plan)
	}
	if resp.Data.Snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if resp.Data.Snapshot.ResourceUsage[types.ResourceWatchPoints] != 5 {
		t.Errorf("expected 5 watchpoints, got %d", resp.Data.Snapshot.ResourceUsage[types.ResourceWatchPoints])
	}
}

func TestGetCurrentUsage_NoOrgContext(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := types.WithRequestID(context.Background(), "req_test")
	req := makeRequest("GET", "/v1/usage", nil, ctx)
	rr := httptest.NewRecorder()

	h.usageHandler.GetCurrent(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetCurrentUsage_ReporterError(t *testing.T) {
	reporter := &mockUsageReporter{
		getCurrentUsageFn: func(ctx context.Context, orgID string) (*types.UsageSnapshot, error) {
			return nil, types.NewAppError(types.ErrCodeInternalUnexpected, "DB error", nil)
		},
	}

	h := newTestBillingHandler(&mockBillingService{}, &mockOrgBillingReader{}, reporter, &mockAuditLogger{})

	ctx := contextWithActor("org_test_1", types.RoleMember)
	req := makeRequest("GET", "/v1/usage", nil, ctx)
	rr := httptest.NewRecorder()

	h.usageHandler.GetCurrent(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", rr.Code, rr.Body.String())
	}
}

// =============================================================================
// GetHistory (Usage) Tests
// =============================================================================

func TestGetUsageHistory_Success(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := contextWithActor("org_test_1", types.RoleMember)
	req := makeRequest("GET", "/v1/usage/history", nil, ctx)
	rr := httptest.NewRecorder()

	h.usageHandler.GetHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Data []*types.UsageDataPoint `json:"data"`
	}
	parseJSONResponse(t, rr, &resp)

	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 data points, got %d", len(resp.Data))
	}
}

func TestGetUsageHistory_WithCustomRange(t *testing.T) {
	var capturedStart, capturedEnd time.Time
	var capturedGranularity types.TimeGranularity

	reporter := &mockUsageReporter{
		getUsageHistoryFn: func(ctx context.Context, orgID string, start, end time.Time, granularity types.TimeGranularity) ([]*types.UsageDataPoint, error) {
			capturedStart = start
			capturedEnd = end
			capturedGranularity = granularity
			return nil, nil
		},
	}

	h := newTestBillingHandler(&mockBillingService{}, &mockOrgBillingReader{}, reporter, &mockAuditLogger{})

	ctx := contextWithActor("org_test_1", types.RoleMember)
	req := makeRequest("GET", "/v1/usage/history?start=2026-01-01T00:00:00Z&end=2026-01-31T23:59:59Z&granularity=monthly", nil, ctx)
	rr := httptest.NewRecorder()

	h.usageHandler.GetHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	expectedStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expectedEnd := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)
	if !capturedStart.Equal(expectedStart) {
		t.Errorf("expected start %v, got %v", expectedStart, capturedStart)
	}
	if !capturedEnd.Equal(expectedEnd) {
		t.Errorf("expected end %v, got %v", expectedEnd, capturedEnd)
	}
	if capturedGranularity != types.GranularityMonthly {
		t.Errorf("expected monthly granularity, got %q", capturedGranularity)
	}
}

func TestGetUsageHistory_InvalidStartTime(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := contextWithActor("org_test_1", types.RoleMember)
	req := makeRequest("GET", "/v1/usage/history?start=not-a-date", nil, ctx)
	rr := httptest.NewRecorder()

	h.usageHandler.GetHistory(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetUsageHistory_InvalidGranularity(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := contextWithActor("org_test_1", types.RoleMember)
	req := makeRequest("GET", "/v1/usage/history?granularity=hourly", nil, ctx)
	rr := httptest.NewRecorder()

	h.usageHandler.GetHistory(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetUsageHistory_EndBeforeStart(t *testing.T) {
	h := newDefaultTestBillingHandler()

	ctx := contextWithActor("org_test_1", types.RoleMember)
	req := makeRequest("GET", "/v1/usage/history?start=2026-02-01T00:00:00Z&end=2026-01-01T00:00:00Z", nil, ctx)
	rr := httptest.NewRecorder()

	h.usageHandler.GetHistory(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// =============================================================================
// Route Registration Tests
// =============================================================================

func TestRegisterRoutes_MountsExpectedPaths(t *testing.T) {
	h := newDefaultTestBillingHandler()

	r := chi.NewRouter()
	h.RegisterRoutes(r)

	// Walk the routes and collect the patterns.
	routes := map[string]bool{}
	walkFn := func(method, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		routes[method+" "+route] = true
		return nil
	}
	if err := chi.Walk(r, walkFn); err != nil {
		t.Fatalf("failed to walk routes: %v", err)
	}

	expectedRoutes := []string{
		"POST /billing/checkout-session",
		"POST /billing/portal-session",
		"GET /billing/invoices",
		"GET /billing/subscription",
		"GET /usage",
		"GET /usage/history",
	}

	for _, expected := range expectedRoutes {
		if !routes[expected] {
			t.Errorf("expected route %q not found; registered routes: %v", expected, routes)
		}
	}
}

// =============================================================================
// requireMinRole Middleware Tests
// =============================================================================

func TestRequireMinRole_AdminAllowed(t *testing.T) {
	called := false
	handler := requireMinRole(types.RoleAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	ctx := contextWithActor("org_test_1", types.RoleAdmin)
	req := httptest.NewRequest("GET", "/test", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("expected handler to be called for admin role")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestRequireMinRole_OwnerAllowed(t *testing.T) {
	called := false
	handler := requireMinRole(types.RoleAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	ctx := contextWithActor("org_test_1", types.RoleOwner)
	req := httptest.NewRequest("GET", "/test", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("expected handler to be called for owner role")
	}
}

func TestRequireMinRole_MemberDenied(t *testing.T) {
	called := false
	handler := requireMinRole(types.RoleAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	ctx := contextWithActor("org_test_1", types.RoleMember)
	req := httptest.NewRequest("GET", "/test", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if called {
		t.Error("expected handler NOT to be called for member role")
	}
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rr.Code)
	}
}

func TestRequireMinRole_NoActor(t *testing.T) {
	handler := requireMinRole(types.RoleAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	ctx := types.WithRequestID(context.Background(), "req_test")
	req := httptest.NewRequest("GET", "/test", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestRequireMinRole_SystemActorBypasses(t *testing.T) {
	called := false
	handler := requireMinRole(types.RoleOwner)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	ctx := types.WithActor(context.Background(), types.Actor{
		ID:             "system_test",
		Type:           types.ActorTypeSystem,
		OrganizationID: "org_test_1",
	})
	req := httptest.NewRequest("GET", "/test", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("expected handler to be called for system actor")
	}
}
