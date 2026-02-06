package external

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"watchpoint/internal/types"

	stripe "github.com/stripe/stripe-go/v82"
)

// ---------------------------------------------------------------------------
// Mock OrgBillingLookup
// ---------------------------------------------------------------------------

type mockOrgBillingLookup struct {
	getBillingInfoFn        func(ctx context.Context, orgID string) (string, string, error)
	updateStripeCustomerFn  func(ctx context.Context, orgID string, customerID string) error
}

func (m *mockOrgBillingLookup) GetBillingInfo(ctx context.Context, orgID string) (string, string, error) {
	if m.getBillingInfoFn != nil {
		return m.getBillingInfoFn(ctx, orgID)
	}
	return "cus_test123", "billing@example.com", nil
}

func (m *mockOrgBillingLookup) UpdateStripeCustomerID(ctx context.Context, orgID string, customerID string) error {
	if m.updateStripeCustomerFn != nil {
		return m.updateStripeCustomerFn(ctx, orgID, customerID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helper: Create test stripe client pointed at httptest server
// ---------------------------------------------------------------------------

func newTestStripeClient(t *testing.T, serverURL string, lookup OrgBillingLookup) *StripeClient {
	t.Helper()
	base := NewBaseClient(
		&http.Client{Timeout: 5 * time.Second},
		"test-stripe",
		RetryPolicy{
			MaxRetries: 0, // No retries in tests for deterministic behavior
			MinWait:    1 * time.Millisecond,
			MaxWait:    10 * time.Millisecond,
		},
		"WatchPoint-Test/1.0",
		WithSleepFunc(noopSleep),
	)

	return NewStripeClientWithBase(base, lookup, StripeClientConfig{
		SecretKey: "sk_test_secret",
		BaseURL:   serverURL,
	})
}

// ---------------------------------------------------------------------------
// EnsureCustomer Tests
// ---------------------------------------------------------------------------

func TestEnsureCustomer_ExistingCustomer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify it's a search request
		if r.URL.Path != "/v1/customers/search" {
			t.Errorf("expected path /v1/customers/search, got %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}

		// Verify authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer sk_test_secret" {
			t.Errorf("expected Bearer sk_test_secret, got %s", auth)
		}

		// Verify search query
		query := r.URL.Query().Get("query")
		if !strings.Contains(query, "org_123") {
			t.Errorf("expected query to contain org_123, got %s", query)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"id":       "cus_existing",
					"email":    "billing@example.com",
					"metadata": map[string]string{"org_id": "org_123"},
				},
			},
			"has_more": false,
		})
	}))
	defer server.Close()

	var updatedOrgID, updatedCustomerID string
	lookup := &mockOrgBillingLookup{
		updateStripeCustomerFn: func(ctx context.Context, orgID string, customerID string) error {
			updatedOrgID = orgID
			updatedCustomerID = customerID
			return nil
		},
	}

	client := newTestStripeClient(t, server.URL, lookup)

	customerID, err := client.EnsureCustomer(context.Background(), "org_123", "billing@example.com")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if customerID != "cus_existing" {
		t.Errorf("expected customer ID cus_existing, got %s", customerID)
	}

	// Verify DB was updated
	if updatedOrgID != "org_123" {
		t.Errorf("expected orgID org_123, got %s", updatedOrgID)
	}
	if updatedCustomerID != "cus_existing" {
		t.Errorf("expected customerID cus_existing, got %s", updatedCustomerID)
	}
}

func TestEnsureCustomer_CreatesNewCustomer(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/v1/customers/search" && r.Method == http.MethodGet:
			// Return empty search result
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data":     []interface{}{},
				"has_more": false,
			})

		case r.URL.Path == "/v1/customers" && r.Method == http.MethodPost:
			// Verify form data
			r.ParseForm()
			if email := r.FormValue("email"); email != "new@example.com" {
				t.Errorf("expected email new@example.com, got %s", email)
			}
			if orgID := r.FormValue("metadata[org_id]"); orgID != "org_new" {
				t.Errorf("expected metadata[org_id] org_new, got %s", orgID)
			}

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":       "cus_created",
				"email":    "new@example.com",
				"metadata": map[string]string{"org_id": "org_new"},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	customerID, err := client.EnsureCustomer(context.Background(), "org_new", "new@example.com")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if customerID != "cus_created" {
		t.Errorf("expected customer ID cus_created, got %s", customerID)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls (search + create), got %d", callCount)
	}
}

func TestEnsureCustomer_StripeSearchError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":    "api_error",
				"message": "internal server error",
			},
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	_, err := client.EnsureCustomer(context.Background(), "org_123", "test@example.com")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// BaseClient will convert 5xx to an AppError with ErrCodeUpstreamUnavailable
	// since retries are exhausted (MaxRetries: 0).
	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// CreateCheckoutSession Tests
// ---------------------------------------------------------------------------

func TestCreateCheckoutSession_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkout/sessions" {
			t.Errorf("expected path /v1/checkout/sessions, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}

		r.ParseForm()

		// Verify customer
		if cust := r.FormValue("customer"); cust != "cus_test123" {
			t.Errorf("expected customer cus_test123, got %s", cust)
		}
		// Verify mode
		if mode := r.FormValue("mode"); mode != "subscription" {
			t.Errorf("expected mode subscription, got %s", mode)
		}
		// Verify client_reference_id
		if ref := r.FormValue("client_reference_id"); ref != "org_123" {
			t.Errorf("expected client_reference_id org_123, got %s", ref)
		}
		// Verify URLs
		if url := r.FormValue("success_url"); url != "https://app.example.com/billing?success=true" {
			t.Errorf("expected success_url, got %s", url)
		}
		if url := r.FormValue("cancel_url"); url != "https://app.example.com/billing?canceled=true" {
			t.Errorf("expected cancel_url, got %s", url)
		}
		// Verify metadata
		if plan := r.FormValue("metadata[plan]"); plan != "pro" {
			t.Errorf("expected metadata[plan] pro, got %s", plan)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":  "cs_test_session",
			"url": "https://checkout.stripe.com/session/cs_test_session",
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	checkoutURL, sessionID, err := client.CreateCheckoutSession(
		context.Background(),
		"org_123",
		types.PlanPro,
		types.RedirectURLs{
			Success: "https://app.example.com/billing?success=true",
			Cancel:  "https://app.example.com/billing?canceled=true",
		},
	)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if sessionID != "cs_test_session" {
		t.Errorf("expected session ID cs_test_session, got %s", sessionID)
	}
	if checkoutURL != "https://checkout.stripe.com/session/cs_test_session" {
		t.Errorf("expected checkout URL, got %s", checkoutURL)
	}
}

func TestCreateCheckoutSession_NoCustomerID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not have made a Stripe API call")
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{
		getBillingInfoFn: func(ctx context.Context, orgID string) (string, string, error) {
			return "", "billing@example.com", nil // No customer ID
		},
	}
	client := newTestStripeClient(t, server.URL, lookup)

	_, _, err := client.CreateCheckoutSession(
		context.Background(),
		"org_no_cust",
		types.PlanPro,
		types.RedirectURLs{Success: "https://example.com/ok", Cancel: "https://example.com/cancel"},
	)
	if err == nil {
		t.Fatal("expected error when no customer ID, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeNotFoundOrg {
		t.Errorf("expected error code %s, got %s", types.ErrCodeNotFoundOrg, appErr.Code)
	}
}

// ---------------------------------------------------------------------------
// CreatePortalSession Tests
// ---------------------------------------------------------------------------

func TestCreatePortalSession_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/billing_portal/sessions" {
			t.Errorf("expected path /v1/billing_portal/sessions, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}

		r.ParseForm()
		if cust := r.FormValue("customer"); cust != "cus_test123" {
			t.Errorf("expected customer cus_test123, got %s", cust)
		}
		if ret := r.FormValue("return_url"); ret != "https://app.example.com/billing" {
			t.Errorf("expected return_url, got %s", ret)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":  "bps_test",
			"url": "https://billing.stripe.com/session/bps_test",
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	portalURL, err := client.CreatePortalSession(
		context.Background(),
		"org_123",
		"https://app.example.com/billing",
	)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if portalURL != "https://billing.stripe.com/session/bps_test" {
		t.Errorf("expected portal URL, got %s", portalURL)
	}
}

// ---------------------------------------------------------------------------
// GetInvoices Tests
// ---------------------------------------------------------------------------

func TestGetInvoices_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/invoices" {
			t.Errorf("expected path /v1/invoices, got %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}

		// Verify query params
		if cust := r.URL.Query().Get("customer"); cust != "cus_test123" {
			t.Errorf("expected customer cus_test123, got %s", cust)
		}
		if limit := r.URL.Query().Get("limit"); limit != "20" {
			t.Errorf("expected limit 20, got %s", limit)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"id":           "inv_001",
					"amount_due":   2999,
					"status":       "paid",
					"period_start": 1706745600,
					"period_end":   1709424000,
					"invoice_pdf":  "https://pay.stripe.com/invoice/inv_001.pdf",
					"status_transitions": map[string]interface{}{
						"paid_at": 1706746000,
					},
				},
				{
					"id":           "inv_002",
					"amount_due":   4999,
					"status":       "open",
					"period_start": 1709424000,
					"period_end":   1712016000,
					"invoice_pdf":  "https://pay.stripe.com/invoice/inv_002.pdf",
					"status_transitions": map[string]interface{}{
						"paid_at": 0,
					},
				},
			},
			"has_more": true,
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	invoices, pageInfo, err := client.GetInvoices(
		context.Background(),
		"org_123",
		types.ListInvoicesParams{Limit: 0}, // Should default to 20
	)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(invoices) != 2 {
		t.Fatalf("expected 2 invoices, got %d", len(invoices))
	}

	// Verify first invoice
	if invoices[0].ID != "inv_001" {
		t.Errorf("expected ID inv_001, got %s", invoices[0].ID)
	}
	if invoices[0].AmountCents != 2999 {
		t.Errorf("expected amount 2999, got %d", invoices[0].AmountCents)
	}
	if invoices[0].Status != "paid" {
		t.Errorf("expected status paid, got %s", invoices[0].Status)
	}
	if invoices[0].PaidAt == nil {
		t.Error("expected PaidAt to be set for paid invoice")
	}
	if invoices[0].PDFURL != "https://pay.stripe.com/invoice/inv_001.pdf" {
		t.Errorf("expected PDF URL, got %s", invoices[0].PDFURL)
	}

	// Verify second invoice has no PaidAt
	if invoices[1].PaidAt != nil {
		t.Error("expected PaidAt to be nil for open invoice")
	}

	// Verify pagination
	if !pageInfo.HasMore {
		t.Error("expected HasMore to be true")
	}
	if pageInfo.NextCursor != "inv_002" {
		t.Errorf("expected next cursor inv_002, got %s", pageInfo.NextCursor)
	}
}

func TestGetInvoices_WithCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify starting_after is set
		startingAfter := r.URL.Query().Get("starting_after")
		if startingAfter != "inv_prev" {
			t.Errorf("expected starting_after=inv_prev, got %s", startingAfter)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data":     []interface{}{},
			"has_more": false,
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	_, pageInfo, err := client.GetInvoices(
		context.Background(),
		"org_123",
		types.ListInvoicesParams{Cursor: "inv_prev", Limit: 10},
	)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if pageInfo.HasMore {
		t.Error("expected HasMore to be false")
	}
	if pageInfo.NextCursor != "" {
		t.Errorf("expected empty next cursor, got %s", pageInfo.NextCursor)
	}
}

func TestGetInvoices_EmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data":     []interface{}{},
			"has_more": false,
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	invoices, pageInfo, err := client.GetInvoices(
		context.Background(),
		"org_123",
		types.ListInvoicesParams{},
	)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(invoices) != 0 {
		t.Errorf("expected 0 invoices, got %d", len(invoices))
	}
	if pageInfo.HasMore {
		t.Error("expected HasMore to be false")
	}
}

// ---------------------------------------------------------------------------
// GetSubscription Tests
// ---------------------------------------------------------------------------

func TestGetSubscription_Active(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/subscriptions" {
			t.Errorf("expected path /v1/subscriptions, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"id":                   "sub_123",
					"status":               "active",
					"cancel_at_period_end": false,
					"current_period_start": 1706745600,
					"current_period_end":   1709424000,
					"items": map[string]interface{}{
						"data": []map[string]interface{}{
							{
								"price": map[string]interface{}{
									"id": "price_pro",
								},
							},
						},
					},
					"default_payment_method": map[string]interface{}{
						"id":   "pm_card",
						"type": "card",
						"card": map[string]interface{}{
							"last4":     "4242",
							"exp_month": 12,
							"exp_year":  2027,
							"brand":     "visa",
						},
					},
				},
			},
			"has_more": false,
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	details, err := client.GetSubscription(context.Background(), "org_123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if details.Plan != types.PlanPro {
		t.Errorf("expected plan pro, got %s", details.Plan)
	}
	if details.Status != types.SubStatusActive {
		t.Errorf("expected status active, got %s", details.Status)
	}
	if details.CancelAtPeriodEnd {
		t.Error("expected CancelAtPeriodEnd to be false")
	}
	if details.PaymentMethod == nil {
		t.Fatal("expected payment method to be set")
	}
	if details.PaymentMethod.Last4 != "4242" {
		t.Errorf("expected last4 4242, got %s", details.PaymentMethod.Last4)
	}
	if details.PaymentMethod.ExpMonth != 12 {
		t.Errorf("expected exp_month 12, got %d", details.PaymentMethod.ExpMonth)
	}
	if details.PaymentMethod.ExpYear != 2027 {
		t.Errorf("expected exp_year 2027, got %d", details.PaymentMethod.ExpYear)
	}
	if details.PaymentMethod.Type != "card" {
		t.Errorf("expected type card, got %s", details.PaymentMethod.Type)
	}
}

func TestGetSubscription_NoSubscription(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data":     []interface{}{},
			"has_more": false,
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	details, err := client.GetSubscription(context.Background(), "org_123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if details.Plan != types.PlanFree {
		t.Errorf("expected free plan, got %s", details.Plan)
	}
	if details.Status != types.SubStatusActive {
		t.Errorf("expected active status, got %s", details.Status)
	}
}

func TestGetSubscription_PastDue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"id":                   "sub_pastdue",
					"status":               "past_due",
					"cancel_at_period_end": true,
					"current_period_start": 1706745600,
					"current_period_end":   1709424000,
					"items": map[string]interface{}{
						"data": []map[string]interface{}{
							{
								"price": map[string]interface{}{
									"id": "price_starter",
								},
							},
						},
					},
				},
			},
			"has_more": false,
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	details, err := client.GetSubscription(context.Background(), "org_123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if details.Status != types.SubStatusPastDue {
		t.Errorf("expected past_due status, got %s", details.Status)
	}
	if details.Plan != types.PlanStarter {
		t.Errorf("expected starter plan, got %s", details.Plan)
	}
	if !details.CancelAtPeriodEnd {
		t.Error("expected CancelAtPeriodEnd to be true")
	}
	if details.PaymentMethod != nil {
		t.Error("expected no payment method")
	}
}

// ---------------------------------------------------------------------------
// Error Mapping Tests
// ---------------------------------------------------------------------------

func TestStripeError_CardDeclined(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":         "card_error",
				"code":         "card_declined",
				"decline_code": "insufficient_funds",
				"message":      "Your card has insufficient funds.",
			},
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	_, _, err := client.CreateCheckoutSession(
		context.Background(),
		"org_123",
		types.PlanPro,
		types.RedirectURLs{Success: "https://example.com/ok", Cancel: "https://example.com/cancel"},
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodePaymentDeclined {
		t.Errorf("expected error code %s, got %s", types.ErrCodePaymentDeclined, appErr.Code)
	}

	// Verify details contain decline info
	if appErr.Details == nil {
		t.Fatal("expected error details")
	}
	if dc, ok := appErr.Details["decline_code"]; !ok || dc != "insufficient_funds" {
		t.Errorf("expected decline_code insufficient_funds, got %v", dc)
	}
}

func TestStripeError_RateLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":    "rate_limit_error",
				"message": "Too many requests",
			},
		})
	}))
	defer server.Close()

	// Use a client with retries disabled that won't trigger BaseClient retry logic
	// on 429. We need MaxRetries: 0 so BaseClient tries once, gets 429, and since
	// there are no retries, it returns the error.
	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	_, err := client.GetSubscription(context.Background(), "org_123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}

	// BaseClient maps 429 to ErrCodeUpstreamRateLimited after retry exhaustion
	if appErr.Code != types.ErrCodeUpstreamRateLimited {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamRateLimited, appErr.Code)
	}
}

func TestStripeError_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":    "invalid_request_error",
				"message": "No such customer: 'cus_nonexistent'",
			},
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	_, err := client.GetSubscription(context.Background(), "org_123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}

	if appErr.Code != types.ErrCodeNotFoundOrg {
		t.Errorf("expected error code %s, got %s", types.ErrCodeNotFoundOrg, appErr.Code)
	}
}

func TestStripeError_GenericBadRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":    "invalid_request_error",
				"message": "Invalid param: something",
				"param":   "something",
			},
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	_, err := client.GetSubscription(context.Background(), "org_123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}

	if appErr.Code != types.ErrCodeUpstreamStripe {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamStripe, appErr.Code)
	}
}

// ---------------------------------------------------------------------------
// StripeVerifier Tests
// ---------------------------------------------------------------------------

func TestStripeVerifier_ValidSignature(t *testing.T) {
	verifier := &StripeVerifier{}
	secret := "whsec_test_secret"
	payload := []byte(`{"id":"evt_test","type":"checkout.session.completed"}`)

	// Generate a valid signature using stripe-go's helper.
	sp := stripe.GenerateTestSignedPayload(&stripe.UnsignedPayload{
		Payload: payload,
		Secret:  secret,
	})

	err := verifier.Verify(payload, sp.Header, secret)
	if err != nil {
		t.Errorf("expected valid signature, got error: %v", err)
	}
}

func TestStripeVerifier_InvalidSignature(t *testing.T) {
	verifier := &StripeVerifier{}
	payload := []byte(`{"id":"evt_test"}`)
	header := "t=1234567890,v1=badbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbad"

	err := verifier.Verify(payload, header, "whsec_test_secret")
	if err == nil {
		t.Error("expected error for invalid signature, got nil")
	}
}

func TestStripeVerifier_MissingHeader(t *testing.T) {
	verifier := &StripeVerifier{}
	payload := []byte(`{"id":"evt_test"}`)

	err := verifier.Verify(payload, "", "whsec_test_secret")
	if err == nil {
		t.Error("expected error for missing signature header, got nil")
	}
}

func TestStripeVerifier_ExpiredTimestamp(t *testing.T) {
	verifier := &StripeVerifier{}
	secret := "whsec_test_secret"
	payload := []byte(`{"id":"evt_test"}`)

	// Generate a signature with a very old timestamp.
	oldTime := time.Now().Add(-10 * time.Minute)
	sig := stripe.ComputeSignature(oldTime, payload, secret)
	header := fmt.Sprintf("t=%d,v1=%s", oldTime.Unix(), hex.EncodeToString(sig))

	err := verifier.Verify(payload, header, secret)
	if err == nil {
		t.Error("expected error for expired timestamp, got nil")
	}
}

// ---------------------------------------------------------------------------
// Subscription Status Mapping Tests
// ---------------------------------------------------------------------------

func TestMapSubscriptionStatus(t *testing.T) {
	tests := []struct {
		input    string
		expected types.SubscriptionStatus
	}{
		{"active", types.SubStatusActive},
		{"past_due", types.SubStatusPastDue},
		{"canceled", types.SubStatusCanceled},
		{"incomplete", types.SubStatusIncomplete},
		{"incomplete_expired", types.SubStatusIncompleteExpired},
		{"trialing", types.SubStatusTrialing},
		{"unpaid", types.SubStatusUnpaid},
		{"unknown_status", types.SubscriptionStatus("unknown_status")},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := mapSubscriptionStatus(tc.input)
			if result != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, result)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Price/Plan Mapping Tests
// ---------------------------------------------------------------------------

func TestMapPriceIDToPlan(t *testing.T) {
	tests := []struct {
		priceID  string
		expected types.PlanTier
	}{
		{"price_starter", types.PlanStarter},
		{"price_pro", types.PlanPro},
		{"price_business", types.PlanBusiness},
		{"price_enterprise", types.PlanEnterprise},
		{"price_unknown", types.PlanFree}, // Unknown falls back to free
	}

	for _, tc := range tests {
		t.Run(tc.priceID, func(t *testing.T) {
			result := mapPriceIDToPlan(tc.priceID)
			if result != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, result)
			}
		})
	}
}

func TestStripePriceID(t *testing.T) {
	tests := []struct {
		plan     types.PlanTier
		expected string
	}{
		{types.PlanStarter, "price_starter"},
		{types.PlanPro, "price_pro"},
		{types.PlanBusiness, "price_business"},
		{types.PlanEnterprise, "price_enterprise"},
		{types.PlanFree, "price_free"}, // Fallback
	}

	for _, tc := range tests {
		t.Run(string(tc.plan), func(t *testing.T) {
			result := stripePriceID(tc.plan)
			if result != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, result)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Authorization Header Tests
// ---------------------------------------------------------------------------

func TestStripeClient_AuthorizationHeader(t *testing.T) {
	var receivedAuth string
	var receivedStripeVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedStripeVersion = r.Header.Get("Stripe-Version")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data":     []interface{}{},
			"has_more": false,
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	_, _ = client.GetSubscription(context.Background(), "org_123")

	if receivedAuth != "Bearer sk_test_secret" {
		t.Errorf("expected Bearer auth header, got: %s", receivedAuth)
	}
	if receivedStripeVersion == "" {
		t.Error("expected Stripe-Version header to be set")
	}
}

// ---------------------------------------------------------------------------
// DB Update Failure Resilience Tests
// ---------------------------------------------------------------------------

func TestEnsureCustomer_DBUpdateFailure_StillReturnsCustomerID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/v1/customers/search" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data":     []interface{}{},
				"has_more": false,
			})
		} else if r.URL.Path == "/v1/customers" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":    "cus_new",
				"email": "test@example.com",
			})
		}
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{
		updateStripeCustomerFn: func(ctx context.Context, orgID string, customerID string) error {
			return fmt.Errorf("database connection lost")
		},
	}
	client := newTestStripeClient(t, server.URL, lookup)

	// Even if DB update fails, the customer ID should be returned.
	// The DB update failure is logged but not propagated (best effort).
	customerID, err := client.EnsureCustomer(context.Background(), "org_123", "test@example.com")
	if err != nil {
		t.Fatalf("expected no error despite DB failure, got: %v", err)
	}
	if customerID != "cus_new" {
		t.Errorf("expected cus_new, got %s", customerID)
	}
}

// ---------------------------------------------------------------------------
// DB Lookup Failure Tests
// ---------------------------------------------------------------------------

func TestGetSubscription_DBLookupFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not have made a Stripe API call when DB lookup fails")
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{
		getBillingInfoFn: func(ctx context.Context, orgID string) (string, string, error) {
			return "", "", types.NewAppError(
				types.ErrCodeInternalDB,
				"database connection failed",
				fmt.Errorf("connection refused"),
			)
		},
	}
	client := newTestStripeClient(t, server.URL, lookup)

	_, err := client.GetSubscription(context.Background(), "org_db_fail")
	if err == nil {
		t.Fatal("expected error when DB lookup fails, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", err)
	}
	if appErr.Code != types.ErrCodeInternalDB {
		t.Errorf("expected error code %s, got %s", types.ErrCodeInternalDB, appErr.Code)
	}
}

func TestGetInvoices_WithCustomLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limit := r.URL.Query().Get("limit"); limit != "5" {
			t.Errorf("expected limit 5, got %s", limit)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data":     []interface{}{},
			"has_more": false,
		})
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	_, _, err := client.GetInvoices(
		context.Background(),
		"org_123",
		types.ListInvoicesParams{Limit: 5},
	)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Stripe API Error with Non-JSON Body
// ---------------------------------------------------------------------------

func TestStripeError_NonJSONBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Bad Request - not JSON"))
	}))
	defer server.Close()

	lookup := &mockOrgBillingLookup{}
	client := newTestStripeClient(t, server.URL, lookup)

	_, err := client.GetSubscription(context.Background(), "org_123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeUpstreamStripe {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamStripe, appErr.Code)
	}
}

// ---------------------------------------------------------------------------
// Interface Compliance
// ---------------------------------------------------------------------------

// Compile-time assertion that StripeClient satisfies BillingService.
var _ BillingService = (*StripeClient)(nil)

// Compile-time assertion that StripeVerifier satisfies WebhookVerifier.
var _ WebhookVerifier = (*StripeVerifier)(nil)
