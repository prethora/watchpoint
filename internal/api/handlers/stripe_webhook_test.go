package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"watchpoint/internal/external"
	"watchpoint/internal/types"
)

// ---------------------------------------------------------------------------
// Mock Implementations
// ---------------------------------------------------------------------------

// mockWebhookVerifier implements external.WebhookVerifier for testing.
type mockWebhookVerifier struct {
	shouldFail bool
	err        error
}

func (m *mockWebhookVerifier) Verify(payload []byte, header string, secret string) error {
	if m.shouldFail {
		if m.err != nil {
			return m.err
		}
		return errors.New("signature verification failed")
	}
	return nil
}

// mockSubStateUpdater implements SubscriptionStateUpdater for testing.
type mockSubStateUpdater struct {
	updateCalls  []updateSubCall
	failureCalls []updatePaymentFailureCall
	updateErr    error
	failureErr   error
}

type updateSubCall struct {
	OrgID          string
	Plan           types.PlanTier
	Status         types.SubscriptionStatus
	EventTimestamp time.Time
}

type updatePaymentFailureCall struct {
	OrgID    string
	FailedAt time.Time
}

func (m *mockSubStateUpdater) UpdateSubscriptionStatus(
	ctx context.Context,
	orgID string,
	newPlan types.PlanTier,
	status types.SubscriptionStatus,
	eventTimestamp time.Time,
) error {
	m.updateCalls = append(m.updateCalls, updateSubCall{
		OrgID:          orgID,
		Plan:           newPlan,
		Status:         status,
		EventTimestamp: eventTimestamp,
	})
	return m.updateErr
}

func (m *mockSubStateUpdater) UpdatePaymentFailure(ctx context.Context, orgID string, failedAt time.Time) error {
	m.failureCalls = append(m.failureCalls, updatePaymentFailureCall{
		OrgID:    orgID,
		FailedAt: failedAt,
	})
	return m.failureErr
}

// mockResumer implements WatchPointResumer for testing.
type mockResumer struct {
	calls []resumeCall
	err   error
}

type resumeCall struct {
	OrgID  string
	Reason types.PausedReason
}

func (m *mockResumer) ResumeAllByOrgID(ctx context.Context, orgID string, reason types.PausedReason) error {
	m.calls = append(m.calls, resumeCall{OrgID: orgID, Reason: reason})
	return m.err
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

// buildStripeEvent creates a JSON-encoded Stripe event for testing.
func buildStripeEvent(eventType string, eventID string, created int64, dataObject interface{}) []byte {
	objBytes, _ := json.Marshal(dataObject)
	event := map[string]interface{}{
		"id":      eventID,
		"type":    eventType,
		"created": created,
		"data": map[string]interface{}{
			"object": json.RawMessage(objBytes),
		},
	}
	b, _ := json.Marshal(event)
	return b
}

// buildCheckoutSessionEvent creates a checkout.session.completed webhook event.
func buildCheckoutSessionEvent(orgID string, plan string, created int64) []byte {
	obj := map[string]interface{}{
		"client_reference_id": orgID,
		"metadata": map[string]string{
			"org_id": orgID,
			"plan":   plan,
		},
		"subscription": "sub_test_123",
	}
	return buildStripeEvent(external.EventStripeCheckoutCompleted, "evt_checkout_1", created, obj)
}

// buildSubscriptionUpdatedEvent creates a customer.subscription.updated webhook event.
func buildSubscriptionUpdatedEvent(orgID string, priceID string, status string, created int64) []byte {
	obj := map[string]interface{}{
		"id":     "sub_test_123",
		"status": status,
		"metadata": map[string]string{
			"org_id": orgID,
		},
		"items": map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"price": map[string]interface{}{
						"id":       priceID,
						"metadata": map[string]string{},
					},
				},
			},
		},
	}
	return buildStripeEvent(external.EventStripeSubUpdated, "evt_sub_upd_1", created, obj)
}

// buildSubscriptionDeletedEvent creates a customer.subscription.deleted webhook event.
func buildSubscriptionDeletedEvent(orgID string, created int64) []byte {
	obj := map[string]interface{}{
		"id":     "sub_test_123",
		"status": "canceled",
		"metadata": map[string]string{
			"org_id": orgID,
		},
	}
	return buildStripeEvent(external.EventStripeSubDeleted, "evt_sub_del_1", created, obj)
}

// buildInvoicePaidEvent creates an invoice.paid webhook event.
func buildInvoicePaidEvent(orgID string, created int64) []byte {
	obj := map[string]interface{}{
		"subscription": "sub_test_123",
		"subscription_details": map[string]interface{}{
			"metadata": map[string]string{
				"org_id": orgID,
			},
		},
	}
	return buildStripeEvent(external.EventStripeInvoicePaid, "evt_inv_paid_1", created, obj)
}

// buildPaymentFailedEvent creates an invoice.payment_failed webhook event.
func buildPaymentFailedEvent(orgID string, created int64) []byte {
	obj := map[string]interface{}{
		"subscription": "sub_test_123",
		"subscription_details": map[string]interface{}{
			"metadata": map[string]string{
				"org_id": orgID,
			},
		},
	}
	return buildStripeEvent(external.EventStripePaymentFailed, "evt_pay_fail_1", created, obj)
}

// newTestWebhookHandler creates a StripeWebhookHandler with mock dependencies.
func newTestWebhookHandler(
	verifier *mockWebhookVerifier,
	subRepo *mockSubStateUpdater,
	resumer *mockResumer,
) *StripeWebhookHandler {
	return NewStripeWebhookHandler(
		verifier,
		subRepo,
		resumer,
		"whsec_test_secret",
		nil, // Use default logger
	)
}

// doWebhookRequest performs an HTTP request to the webhook handler.
func doWebhookRequest(handler *StripeWebhookHandler, body []byte, sigHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(body))
	if sigHeader != "" {
		req.Header.Set("Stripe-Signature", sigHeader)
	}
	rr := httptest.NewRecorder()
	handler.Handle(rr, req)
	return rr
}

// ---------------------------------------------------------------------------
// Tests: Signature Verification
// ---------------------------------------------------------------------------

func TestStripeWebhookHandler_Handle_MissingSignature(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	body := buildCheckoutSessionEvent("org_123", "pro", time.Now().Unix())
	rr := doWebhookRequest(handler, body, "")

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
	}

	// Verify the error response contains the right error code.
	var errResp map[string]map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if code, ok := errResp["error"]["code"].(string); !ok || code != string(types.ErrCodeAuthTokenMissing) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeAuthTokenMissing, code)
	}
}

func TestStripeWebhookHandler_Handle_InvalidSignature(t *testing.T) {
	verifier := &mockWebhookVerifier{shouldFail: true}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	body := buildCheckoutSessionEvent("org_123", "pro", time.Now().Unix())
	rr := doWebhookRequest(handler, body, "t=12345,v1=bad_signature")

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
	}

	var errResp map[string]map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if code, ok := errResp["error"]["code"].(string); !ok || code != string(types.ErrCodeAuthTokenInvalid) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeAuthTokenInvalid, code)
	}
}

func TestStripeWebhookHandler_Handle_ValidSignature(t *testing.T) {
	verifier := &mockWebhookVerifier{} // passes
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	body := buildCheckoutSessionEvent("org_123", "pro", time.Now().Unix())
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid_signature")

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: Event Routing
// ---------------------------------------------------------------------------

func TestStripeWebhookHandler_Handle_CheckoutCompleted(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	now := time.Now().Unix()
	body := buildCheckoutSessionEvent("org_abc", "pro", now)
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	if len(subRepo.updateCalls) != 1 {
		t.Fatalf("expected 1 UpdateSubscriptionStatus call, got %d", len(subRepo.updateCalls))
	}

	call := subRepo.updateCalls[0]
	if call.OrgID != "org_abc" {
		t.Errorf("expected orgID %q, got %q", "org_abc", call.OrgID)
	}
	if call.Plan != types.PlanPro {
		t.Errorf("expected plan %q, got %q", types.PlanPro, call.Plan)
	}
	if call.Status != types.SubStatusActive {
		t.Errorf("expected status %q, got %q", types.SubStatusActive, call.Status)
	}
	expectedTime := time.Unix(now, 0).UTC()
	if !call.EventTimestamp.Equal(expectedTime) {
		t.Errorf("expected timestamp %v, got %v", expectedTime, call.EventTimestamp)
	}
}

func TestStripeWebhookHandler_Handle_SubscriptionUpdated(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	now := time.Now().Unix()
	body := buildSubscriptionUpdatedEvent("org_xyz", "price_pro", "active", now)
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	if len(subRepo.updateCalls) != 1 {
		t.Fatalf("expected 1 UpdateSubscriptionStatus call, got %d", len(subRepo.updateCalls))
	}

	call := subRepo.updateCalls[0]
	if call.OrgID != "org_xyz" {
		t.Errorf("expected orgID %q, got %q", "org_xyz", call.OrgID)
	}
	if call.Plan != types.PlanPro {
		t.Errorf("expected plan %q, got %q", types.PlanPro, call.Plan)
	}
	if call.Status != types.SubStatusActive {
		t.Errorf("expected status %q, got %q", types.SubStatusActive, call.Status)
	}
}

func TestStripeWebhookHandler_Handle_SubscriptionUpdated_PastDue(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	body := buildSubscriptionUpdatedEvent("org_xyz", "price_starter", "past_due", time.Now().Unix())
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	if len(subRepo.updateCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(subRepo.updateCalls))
	}

	call := subRepo.updateCalls[0]
	if call.Plan != types.PlanStarter {
		t.Errorf("expected plan %q, got %q", types.PlanStarter, call.Plan)
	}
	if call.Status != types.SubStatusPastDue {
		t.Errorf("expected status %q, got %q", types.SubStatusPastDue, call.Status)
	}
}

func TestStripeWebhookHandler_Handle_SubscriptionDeleted(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	now := time.Now().Unix()
	body := buildSubscriptionDeletedEvent("org_cancel", now)
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	if len(subRepo.updateCalls) != 1 {
		t.Fatalf("expected 1 UpdateSubscriptionStatus call, got %d", len(subRepo.updateCalls))
	}

	call := subRepo.updateCalls[0]
	if call.OrgID != "org_cancel" {
		t.Errorf("expected orgID %q, got %q", "org_cancel", call.OrgID)
	}
	if call.Plan != types.PlanFree {
		t.Errorf("expected plan %q (free on delete), got %q", types.PlanFree, call.Plan)
	}
	if call.Status != types.SubStatusCanceled {
		t.Errorf("expected status %q, got %q", types.SubStatusCanceled, call.Status)
	}
}

func TestStripeWebhookHandler_Handle_InvoicePaid(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	body := buildInvoicePaidEvent("org_pay", time.Now().Unix())
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Verify ResumeAllByOrgID was called with the correct arguments.
	if len(resumer.calls) != 1 {
		t.Fatalf("expected 1 ResumeAllByOrgID call, got %d", len(resumer.calls))
	}

	call := resumer.calls[0]
	if call.OrgID != "org_pay" {
		t.Errorf("expected orgID %q, got %q", "org_pay", call.OrgID)
	}
	if call.Reason != types.PausedReasonBillingDelinquency {
		t.Errorf("expected reason %q, got %q", types.PausedReasonBillingDelinquency, call.Reason)
	}

	// No subscription status update should have been made for invoice.paid.
	if len(subRepo.updateCalls) != 0 {
		t.Errorf("expected 0 UpdateSubscriptionStatus calls for invoice.paid, got %d", len(subRepo.updateCalls))
	}
}

func TestStripeWebhookHandler_Handle_InvoicePaid_ResumeError(t *testing.T) {
	// Even if resume fails, the webhook should return 200 OK.
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{err: errors.New("db connection failed")}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	body := buildInvoicePaidEvent("org_pay", time.Now().Unix())
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d (even with resume error), got %d", http.StatusOK, rr.Code)
	}

	// Resume should have been attempted.
	if len(resumer.calls) != 1 {
		t.Fatalf("expected 1 ResumeAllByOrgID call, got %d", len(resumer.calls))
	}
}

func TestStripeWebhookHandler_Handle_InvoicePaid_NilResumer(t *testing.T) {
	// When resumer is nil, invoice.paid should still succeed.
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	handler := NewStripeWebhookHandler(verifier, subRepo, nil, "whsec_test", nil)

	body := buildInvoicePaidEvent("org_pay", time.Now().Unix())
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
}

func TestStripeWebhookHandler_Handle_PaymentFailed(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	now := time.Now().Unix()
	body := buildPaymentFailedEvent("org_fail", now)
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	if len(subRepo.failureCalls) != 1 {
		t.Fatalf("expected 1 UpdatePaymentFailure call, got %d", len(subRepo.failureCalls))
	}

	call := subRepo.failureCalls[0]
	if call.OrgID != "org_fail" {
		t.Errorf("expected orgID %q, got %q", "org_fail", call.OrgID)
	}
	expectedTime := time.Unix(now, 0).UTC()
	if !call.FailedAt.Equal(expectedTime) {
		t.Errorf("expected failedAt %v, got %v", expectedTime, call.FailedAt)
	}
}

func TestStripeWebhookHandler_Handle_PaymentFailed_DBError(t *testing.T) {
	// Even if UpdatePaymentFailure fails, the handler returns 200 to avoid Stripe retries.
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{failureErr: errors.New("db error")}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	body := buildPaymentFailedEvent("org_fail", time.Now().Unix())
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	// The handler returns 200 even on internal errors to avoid Stripe retries.
	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d (even with DB error), got %d", http.StatusOK, rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: Unknown Event Type
// ---------------------------------------------------------------------------

func TestStripeWebhookHandler_Handle_UnknownEventType(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	body := buildStripeEvent("some.unknown.event", "evt_unknown", time.Now().Unix(), map[string]interface{}{})
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d for unknown event, got %d", http.StatusOK, rr.Code)
	}

	// No calls should have been made to subRepo or resumer.
	if len(subRepo.updateCalls) != 0 {
		t.Errorf("expected 0 UpdateSubscriptionStatus calls, got %d", len(subRepo.updateCalls))
	}
	if len(subRepo.failureCalls) != 0 {
		t.Errorf("expected 0 UpdatePaymentFailure calls, got %d", len(subRepo.failureCalls))
	}
	if len(resumer.calls) != 0 {
		t.Errorf("expected 0 ResumeAllByOrgID calls, got %d", len(resumer.calls))
	}
}

// ---------------------------------------------------------------------------
// Tests: Invalid JSON
// ---------------------------------------------------------------------------

func TestStripeWebhookHandler_Handle_InvalidJSON(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	rr := doWebhookRequest(handler, []byte("not valid json"), "t=12345,v1=valid")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status %d for invalid JSON, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestStripeWebhookHandler_Handle_EmptyBody(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	rr := doWebhookRequest(handler, []byte{}, "t=12345,v1=valid")

	// Empty body should fail JSON parsing.
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status %d for empty body, got %d", http.StatusBadRequest, rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: Missing OrgID
// ---------------------------------------------------------------------------

func TestStripeWebhookHandler_Handle_CheckoutCompleted_MissingOrgID(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	// Build event with no client_reference_id and no org_id in metadata.
	obj := map[string]interface{}{
		"metadata":     map[string]string{},
		"subscription": "sub_123",
	}
	body := buildStripeEvent(external.EventStripeCheckoutCompleted, "evt_1", time.Now().Unix(), obj)
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	// Handler returns 200 even if processing fails internally.
	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// No subscription update should have been made.
	if len(subRepo.updateCalls) != 0 {
		t.Errorf("expected 0 UpdateSubscriptionStatus calls when orgID missing, got %d", len(subRepo.updateCalls))
	}
}

// ---------------------------------------------------------------------------
// Tests: RegisterRoutes
// ---------------------------------------------------------------------------

func TestStripeWebhookHandler_RegisterRoutes(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	// Use chi to verify route registration.
	r := chi.NewRouter()
	handler.RegisterRoutes(r)

	// Verify the POST /webhooks/stripe route exists by sending a request.
	body := buildCheckoutSessionEvent("org_route_test", "starter", time.Now().Unix())
	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", "t=12345,v1=valid")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d from registered route, got %d", http.StatusOK, rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: Body Size Limit
// ---------------------------------------------------------------------------

func TestStripeWebhookHandler_Handle_OversizedBody(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	// Create a body larger than maxWebhookBodySize (64 KB).
	oversizedBody := make([]byte, maxWebhookBodySize+1024)
	for i := range oversizedBody {
		oversizedBody[i] = 'a'
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(oversizedBody))
	req.Header.Set("Stripe-Signature", "t=12345,v1=valid")
	rr := httptest.NewRecorder()
	handler.Handle(rr, req)

	// The handler should either return a 400 (from body read failure) or
	// verification failure (since the truncated body won't verify).
	// The important thing is that it does NOT return 200.
	if rr.Code == http.StatusOK {
		t.Error("expected non-200 status for oversized body, got 200")
	}
}

// ---------------------------------------------------------------------------
// Tests: Event Parsing
// ---------------------------------------------------------------------------

func TestExtractOrgID_InvoiceViaSubscriptionDetails(t *testing.T) {
	// Test that invoice events extract orgID from subscription_details.metadata.
	obj := map[string]interface{}{
		"subscription": "sub_123",
		"subscription_details": map[string]interface{}{
			"metadata": map[string]string{
				"org_id": "org_from_sub_details",
			},
		},
		"metadata": map[string]string{
			"org_id": "org_from_invoice_meta",
		},
	}
	body := buildStripeEvent(external.EventStripeInvoicePaid, "evt_1", time.Now().Unix(), obj)

	var event stripeWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	orgID := event.extractOrgID()
	if orgID != "org_from_sub_details" {
		t.Errorf("expected orgID from subscription_details, got %q", orgID)
	}
}

func TestExtractOrgID_InvoiceViaMetadataFallback(t *testing.T) {
	// When subscription_details has no org_id, fall back to invoice metadata.
	obj := map[string]interface{}{
		"subscription":         "sub_123",
		"subscription_details": nil,
		"metadata": map[string]string{
			"org_id": "org_from_invoice_meta",
		},
	}
	body := buildStripeEvent(external.EventStripeInvoicePaid, "evt_1", time.Now().Unix(), obj)

	var event stripeWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	orgID := event.extractOrgID()
	if orgID != "org_from_invoice_meta" {
		t.Errorf("expected orgID from invoice metadata fallback, got %q", orgID)
	}
}

func TestExtractOrgID_CheckoutViaClientReferenceID(t *testing.T) {
	// Checkout events should prefer client_reference_id.
	obj := map[string]interface{}{
		"client_reference_id": "org_via_ref",
		"metadata": map[string]string{
			"org_id": "org_via_meta",
		},
	}
	body := buildStripeEvent(external.EventStripeCheckoutCompleted, "evt_1", time.Now().Unix(), obj)

	var event stripeWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	orgID := event.extractOrgID()
	if orgID != "org_via_ref" {
		t.Errorf("expected orgID from client_reference_id, got %q", orgID)
	}
}

func TestExtractPlanFromSubscription(t *testing.T) {
	tests := []struct {
		name     string
		priceID  string
		expected types.PlanTier
	}{
		{"starter", "price_starter", types.PlanStarter},
		{"pro", "price_pro", types.PlanPro},
		{"business", "price_business", types.PlanBusiness},
		{"enterprise", "price_enterprise", types.PlanEnterprise},
		{"unknown price", "price_unknown", types.PlanFree},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := buildSubscriptionUpdatedEvent("org_1", tt.priceID, "active", time.Now().Unix())
			var event stripeWebhookEvent
			if err := json.Unmarshal(body, &event); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			plan := event.extractPlanFromSubscription()
			if plan != tt.expected {
				t.Errorf("expected plan %q, got %q", tt.expected, plan)
			}
		})
	}
}

func TestExtractSubscriptionStatus(t *testing.T) {
	tests := []struct {
		status   string
		expected types.SubscriptionStatus
	}{
		{"active", types.SubStatusActive},
		{"past_due", types.SubStatusPastDue},
		{"canceled", types.SubStatusCanceled},
		{"incomplete", types.SubStatusIncomplete},
		{"incomplete_expired", types.SubStatusIncompleteExpired},
		{"trialing", types.SubStatusTrialing},
		{"unpaid", types.SubStatusUnpaid},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			body := buildSubscriptionUpdatedEvent("org_1", "price_pro", tt.status, time.Now().Unix())
			var event stripeWebhookEvent
			if err := json.Unmarshal(body, &event); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			status := event.extractSubscriptionStatus()
			if status != tt.expected {
				t.Errorf("expected status %q, got %q", tt.expected, status)
			}
		})
	}
}

func TestEventTimestamp(t *testing.T) {
	ts := int64(1706803200) // 2024-02-01T12:00:00Z
	body := buildStripeEvent("test.event", "evt_1", ts, map[string]interface{}{})

	var event stripeWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	result := event.eventTimestamp()
	expected := time.Unix(ts, 0).UTC()
	if !result.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, result)
	}
}

// ---------------------------------------------------------------------------
// Tests: Constructor
// ---------------------------------------------------------------------------

func TestNewStripeWebhookHandler_NilLogger(t *testing.T) {
	handler := NewStripeWebhookHandler(
		&mockWebhookVerifier{},
		&mockSubStateUpdater{},
		&mockResumer{},
		"secret",
		nil,
	)
	if handler.logger == nil {
		t.Error("expected non-nil logger when nil is passed")
	}
}

func TestNewStripeWebhookHandler_FieldsSet(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{}
	resumer := &mockResumer{}
	handler := NewStripeWebhookHandler(verifier, subRepo, resumer, "whsec_test", nil)

	if handler.secret != "whsec_test" {
		t.Errorf("expected secret %q, got %q", "whsec_test", handler.secret)
	}
	if handler.verifier != verifier {
		t.Error("verifier not set correctly")
	}
	if handler.subRepo != subRepo {
		t.Error("subRepo not set correctly")
	}
	if handler.resumer != resumer {
		t.Error("resumer not set correctly")
	}
}

// ---------------------------------------------------------------------------
// Tests: Subscription Update Error Propagation
// ---------------------------------------------------------------------------

func TestStripeWebhookHandler_Handle_CheckoutCompleted_DBError(t *testing.T) {
	verifier := &mockWebhookVerifier{}
	subRepo := &mockSubStateUpdater{
		updateErr: errors.New("connection refused"),
	}
	resumer := &mockResumer{}
	handler := newTestWebhookHandler(verifier, subRepo, resumer)

	body := buildCheckoutSessionEvent("org_123", "pro", time.Now().Unix())
	rr := doWebhookRequest(handler, body, "t=12345,v1=valid")

	// Returns 200 even on internal errors per Stripe best practices.
	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
}
