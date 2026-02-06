package external

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"watchpoint/internal/types"

	stripe "github.com/stripe/stripe-go/v82"
)

// stripeAPIBase is the default Stripe API base URL.
// Overridable in tests via StripeClientConfig.BaseURL.
const stripeAPIBase = "https://api.stripe.com"

// OrgBillingLookup provides the minimal data access needed by StripeClient
// to resolve an orgID into the Stripe customer ID and billing email.
// This avoids pulling in the full OrganizationRepository interface.
type OrgBillingLookup interface {
	// GetBillingInfo returns the stripe_customer_id and billing_email for the given org.
	// Returns ("", "", nil) if the org exists but has no stripe_customer_id.
	// Returns an error if the org does not exist.
	GetBillingInfo(ctx context.Context, orgID string) (stripeCustomerID string, billingEmail string, err error)

	// UpdateStripeCustomerID sets the stripe_customer_id for the given org.
	UpdateStripeCustomerID(ctx context.Context, orgID string, customerID string) error
}

// StripeClientConfig holds the configuration for creating a StripeClient.
type StripeClientConfig struct {
	SecretKey string
	BaseURL   string // Override for testing; defaults to stripeAPIBase
	Logger    *slog.Logger
}

// StripeClient implements BillingService by making direct HTTP calls to the
// Stripe REST API through BaseClient. This approach routes all requests
// through the platform's resilience infrastructure (circuit breaker, retries,
// error mapping) and makes testing with httptest straightforward.
type StripeClient struct {
	base      *BaseClient
	secretKey string
	baseURL   string
	orgLookup OrgBillingLookup
	logger    *slog.Logger
}

// NewStripeClient creates a new StripeClient. The httpClient timeout should be
// set to 20 seconds as specified in the architecture.
func NewStripeClient(
	httpClient *http.Client,
	orgLookup OrgBillingLookup,
	cfg StripeClientConfig,
) *StripeClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = stripeAPIBase
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	base := NewBaseClient(
		httpClient,
		"stripe",
		RetryPolicy{
			MaxRetries: 2,
			MinWait:    500 * time.Millisecond,
			MaxWait:    5 * time.Second,
		},
		"WatchPoint/1.0",
		WithSleepFunc(time.Sleep),
	)

	return &StripeClient{
		base:      base,
		secretKey: cfg.SecretKey,
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		orgLookup: orgLookup,
		logger:    logger,
	}
}

// NewStripeClientWithBase creates a StripeClient with a pre-configured BaseClient.
// This is useful for testing when you want to control the BaseClient configuration.
func NewStripeClientWithBase(
	base *BaseClient,
	orgLookup OrgBillingLookup,
	cfg StripeClientConfig,
) *StripeClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = stripeAPIBase
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &StripeClient{
		base:      base,
		secretKey: cfg.SecretKey,
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		orgLookup: orgLookup,
		logger:    logger,
	}
}

// ---------------------------------------------------------------------------
// BillingService Implementation
// ---------------------------------------------------------------------------

// EnsureCustomer retrieves or creates a Stripe customer for the given org.
// Uses search-first logic to prevent duplicates (BILL-001):
//  1. Query Stripe Search API for metadata['org_id'] match
//  2. If found, return existing customer ID
//  3. If not found, create a new customer with org_id metadata
//  4. Update local DB with the customer ID
func (s *StripeClient) EnsureCustomer(ctx context.Context, orgID string, email string) (string, error) {
	// Step 1: Search for existing customer by org_id metadata.
	searchQuery := fmt.Sprintf("metadata['org_id']:'%s'", orgID)
	params := url.Values{}
	params.Set("query", searchQuery)

	searchResp, err := s.doGet(ctx, "/v1/customers/search", params)
	if err != nil {
		return "", s.wrapStripeError("EnsureCustomer.search", err)
	}
	defer searchResp.Body.Close()

	if searchResp.StatusCode != http.StatusOK {
		return "", s.handleErrorResponse(searchResp, "EnsureCustomer.search")
	}

	var searchResult stripeSearchResult
	if err := json.NewDecoder(searchResp.Body).Decode(&searchResult); err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode Stripe customer search response",
			err,
		)
	}

	// Step 2: If customer already exists, update DB and return.
	if len(searchResult.Data) > 0 {
		customerID := searchResult.Data[0].ID
		if dbErr := s.orgLookup.UpdateStripeCustomerID(ctx, orgID, customerID); dbErr != nil {
			s.logger.WarnContext(ctx, "failed to update stripe_customer_id in DB",
				"org_id", orgID,
				"customer_id", customerID,
				"error", dbErr,
			)
		}
		return customerID, nil
	}

	// Step 3: Create new customer.
	createParams := url.Values{}
	createParams.Set("email", email)
	createParams.Set("metadata[org_id]", orgID)

	createResp, err := s.doPost(ctx, "/v1/customers", createParams)
	if err != nil {
		return "", s.wrapStripeError("EnsureCustomer.create", err)
	}
	defer createResp.Body.Close()

	if createResp.StatusCode != http.StatusOK {
		return "", s.handleErrorResponse(createResp, "EnsureCustomer.create")
	}

	var customer stripeCustomer
	if err := json.NewDecoder(createResp.Body).Decode(&customer); err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode Stripe customer creation response",
			err,
		)
	}

	// Step 4: Update local DB.
	if dbErr := s.orgLookup.UpdateStripeCustomerID(ctx, orgID, customer.ID); dbErr != nil {
		s.logger.WarnContext(ctx, "failed to update stripe_customer_id in DB after creation",
			"org_id", orgID,
			"customer_id", customer.ID,
			"error", dbErr,
		)
	}

	return customer.ID, nil
}

// CreateCheckoutSession generates a Stripe Checkout Session URL.
// Sets client_reference_id to orgID for webhook correlation (BILL-002).
func (s *StripeClient) CreateCheckoutSession(
	ctx context.Context,
	orgID string,
	plan types.PlanTier,
	urls types.RedirectURLs,
) (checkoutURL string, sessionID string, err error) {
	customerID, _, err := s.resolveCustomerID(ctx, orgID)
	if err != nil {
		return "", "", err
	}

	params := url.Values{}
	params.Set("customer", customerID)
	params.Set("mode", "subscription")
	params.Set("client_reference_id", orgID)
	params.Set("success_url", urls.Success)
	params.Set("cancel_url", urls.Cancel)
	params.Set("metadata[org_id]", orgID)
	params.Set("metadata[plan]", string(plan))
	// line_items would be configured with the Stripe Price ID corresponding to the plan tier.
	// The actual price ID mapping is an environment/configuration concern handled at a higher level.
	params.Set("line_items[0][price]", stripePriceID(plan))
	params.Set("line_items[0][quantity]", "1")

	resp, err := s.doPost(ctx, "/v1/checkout/sessions", params)
	if err != nil {
		return "", "", s.wrapStripeError("CreateCheckoutSession", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", s.handleErrorResponse(resp, "CreateCheckoutSession")
	}

	var session stripeCheckoutSession
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "", "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode Stripe checkout session response",
			err,
		)
	}

	return session.URL, session.ID, nil
}

// CreatePortalSession generates a Stripe Billing Portal URL.
func (s *StripeClient) CreatePortalSession(
	ctx context.Context,
	orgID string,
	returnURL string,
) (portalURL string, err error) {
	customerID, _, err := s.resolveCustomerID(ctx, orgID)
	if err != nil {
		return "", err
	}

	params := url.Values{}
	params.Set("customer", customerID)
	params.Set("return_url", returnURL)

	resp, err := s.doPost(ctx, "/v1/billing_portal/sessions", params)
	if err != nil {
		return "", s.wrapStripeError("CreatePortalSession", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", s.handleErrorResponse(resp, "CreatePortalSession")
	}

	var session stripePortalSession
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode Stripe portal session response",
			err,
		)
	}

	return session.URL, nil
}

// GetInvoices retrieves invoices for the organization with cursor-based pagination.
// Maps ListInvoicesParams.Cursor to Stripe's starting_after parameter (INFO-005).
func (s *StripeClient) GetInvoices(
	ctx context.Context,
	orgID string,
	params types.ListInvoicesParams,
) ([]*types.Invoice, types.PageInfo, error) {
	customerID, _, err := s.resolveCustomerID(ctx, orgID)
	if err != nil {
		return nil, types.PageInfo{}, err
	}

	queryParams := url.Values{}
	queryParams.Set("customer", customerID)

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}
	queryParams.Set("limit", fmt.Sprintf("%d", limit))

	if params.Cursor != "" {
		queryParams.Set("starting_after", params.Cursor)
	}

	resp, err := s.doGet(ctx, "/v1/invoices", queryParams)
	if err != nil {
		return nil, types.PageInfo{}, s.wrapStripeError("GetInvoices", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, types.PageInfo{}, s.handleErrorResponse(resp, "GetInvoices")
	}

	var listResp stripeInvoiceList
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, types.PageInfo{}, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode Stripe invoices response",
			err,
		)
	}

	// Map Stripe invoices to domain types.
	invoices := make([]*types.Invoice, 0, len(listResp.Data))
	for _, si := range listResp.Data {
		invoices = append(invoices, mapStripeInvoice(&si))
	}

	// Build pagination info.
	pageInfo := types.PageInfo{
		HasMore: listResp.HasMore,
	}
	if listResp.HasMore && len(listResp.Data) > 0 {
		pageInfo.NextCursor = listResp.Data[len(listResp.Data)-1].ID
	}

	return invoices, pageInfo, nil
}

// GetSubscription retrieves the current subscription details for the organization.
func (s *StripeClient) GetSubscription(ctx context.Context, orgID string) (*types.SubscriptionDetails, error) {
	customerID, _, err := s.resolveCustomerID(ctx, orgID)
	if err != nil {
		return nil, err
	}

	// List subscriptions for customer (typically there should be exactly one active).
	queryParams := url.Values{}
	queryParams.Set("customer", customerID)
	queryParams.Set("limit", "1")

	resp, err := s.doGet(ctx, "/v1/subscriptions", queryParams)
	if err != nil {
		return nil, s.wrapStripeError("GetSubscription", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, s.handleErrorResponse(resp, "GetSubscription")
	}

	var listResp stripeSubscriptionList
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode Stripe subscriptions response",
			err,
		)
	}

	if len(listResp.Data) == 0 {
		// No subscription -- return a free-tier result.
		return &types.SubscriptionDetails{
			Plan:   types.PlanFree,
			Status: types.SubStatusActive,
		}, nil
	}

	return mapStripeSubscription(&listResp.Data[0]), nil
}

// ---------------------------------------------------------------------------
// HTTP Helpers
// ---------------------------------------------------------------------------

// doGet performs an authenticated GET request to the Stripe API.
func (s *StripeClient) doGet(ctx context.Context, path string, params url.Values) (*http.Response, error) {
	reqURL := s.baseURL + path
	if len(params) > 0 {
		reqURL += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	s.setAuthHeaders(req)

	return s.base.Do(req)
}

// doPost performs an authenticated POST request to the Stripe API with form-encoded body.
func (s *StripeClient) doPost(ctx context.Context, path string, params url.Values) (*http.Response, error) {
	reqURL := s.baseURL + path
	body := params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.setAuthHeaders(req)

	return s.base.Do(req)
}

// setAuthHeaders sets the Stripe API authentication and content headers.
func (s *StripeClient) setAuthHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+s.secretKey)
	req.Header.Set("Stripe-Version", stripe.APIVersion)
}

// resolveCustomerID fetches the Stripe customer ID for the given org from the database.
func (s *StripeClient) resolveCustomerID(ctx context.Context, orgID string) (string, string, error) {
	customerID, email, err := s.orgLookup.GetBillingInfo(ctx, orgID)
	if err != nil {
		return "", "", err
	}
	if customerID == "" {
		return "", "", types.NewAppError(
			types.ErrCodeNotFoundOrg,
			fmt.Sprintf("organization %s has no Stripe customer ID; call EnsureCustomer first", orgID),
			nil,
		)
	}
	return customerID, email, nil
}

// ---------------------------------------------------------------------------
// Error Handling
// ---------------------------------------------------------------------------

// stripeErrorResponse represents the JSON error body returned by the Stripe API.
type stripeErrorResponse struct {
	Error stripeErrorBody `json:"error"`
}

type stripeErrorBody struct {
	Type           string `json:"type"`
	Code           string `json:"code"`
	DeclineCode    string `json:"decline_code"`
	Message        string `json:"message"`
	Param          string `json:"param"`
	DocURL         string `json:"doc_url"`
	HTTPStatusCode int    `json:"-"` // Not in JSON; populated from HTTP status
}

// handleErrorResponse reads a Stripe error response and maps it to a types.AppError.
func (s *StripeClient) handleErrorResponse(resp *http.Response, operation string) error {
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return types.NewAppError(
			types.ErrCodeUpstreamStripe,
			fmt.Sprintf("%s: Stripe returned status %d and response body was unreadable", operation, resp.StatusCode),
			readErr,
		)
	}

	var stripeErr stripeErrorResponse
	if jsonErr := json.Unmarshal(body, &stripeErr); jsonErr != nil {
		return types.NewAppError(
			types.ErrCodeUpstreamStripe,
			fmt.Sprintf("%s: Stripe returned status %d with non-JSON body", operation, resp.StatusCode),
			jsonErr,
		)
	}

	return s.mapStripeError(operation, resp.StatusCode, &stripeErr.Error)
}

// mapStripeError translates a Stripe error into a types.AppError.
func (s *StripeClient) mapStripeError(operation string, statusCode int, stripeErr *stripeErrorBody) error {
	// Map card_declined to ErrCodePaymentDeclined.
	if stripeErr.Code == "card_declined" || stripeErr.DeclineCode != "" {
		return types.NewAppErrorWithDetails(
			types.ErrCodePaymentDeclined,
			fmt.Sprintf("%s: payment declined: %s", operation, stripeErr.Message),
			nil,
			map[string]any{
				"decline_code": stripeErr.DeclineCode,
				"stripe_code":  stripeErr.Code,
			},
		)
	}

	// Map based on HTTP status code.
	switch {
	case statusCode == http.StatusTooManyRequests:
		return types.NewAppError(
			types.ErrCodeUpstreamRateLimited,
			fmt.Sprintf("%s: Stripe rate limit exceeded", operation),
			nil,
		)
	case statusCode >= 500:
		return types.NewAppError(
			types.ErrCodeUpstreamUnavailable,
			fmt.Sprintf("%s: Stripe server error: %s", operation, stripeErr.Message),
			nil,
		)
	case statusCode == http.StatusNotFound:
		return types.NewAppError(
			types.ErrCodeNotFoundOrg,
			fmt.Sprintf("%s: Stripe resource not found: %s", operation, stripeErr.Message),
			nil,
		)
	default:
		return types.NewAppError(
			types.ErrCodeUpstreamStripe,
			fmt.Sprintf("%s: Stripe error (%d): %s", operation, statusCode, stripeErr.Message),
			nil,
		)
	}
}

// wrapStripeError wraps a BaseClient transport error with context.
func (s *StripeClient) wrapStripeError(operation string, err error) error {
	// If it's already an AppError from BaseClient (circuit breaker, retries exhausted),
	// return it as-is since it already has the right error code.
	if _, ok := err.(*types.AppError); ok {
		return err
	}
	return types.NewAppError(
		types.ErrCodeUpstreamStripe,
		fmt.Sprintf("%s: Stripe request failed: %v", operation, err),
		err,
	)
}

// ---------------------------------------------------------------------------
// Stripe Response Types (for JSON deserialization)
// ---------------------------------------------------------------------------

type stripeCustomer struct {
	ID       string            `json:"id"`
	Email    string            `json:"email"`
	Metadata map[string]string `json:"metadata"`
}

type stripeSearchResult struct {
	Data    []stripeCustomer `json:"data"`
	HasMore bool             `json:"has_more"`
}

type stripeCheckoutSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type stripePortalSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type stripeInvoice struct {
	ID                 string `json:"id"`
	AmountDue          int64  `json:"amount_due"`
	Status             string `json:"status"`
	PeriodStart        int64  `json:"period_start"`
	PeriodEnd          int64  `json:"period_end"`
	InvoicePDF         string `json:"invoice_pdf"`
	StatusTransitions  stripeStatusTransitions `json:"status_transitions"`
}

type stripeStatusTransitions struct {
	PaidAt int64 `json:"paid_at"`
}

type stripeInvoiceList struct {
	Data    []stripeInvoice `json:"data"`
	HasMore bool            `json:"has_more"`
}

type stripeSubscription struct {
	ID                string                    `json:"id"`
	Status            string                    `json:"status"`
	CancelAtPeriodEnd bool                      `json:"cancel_at_period_end"`
	CurrentPeriodStart int64                    `json:"current_period_start"`
	CurrentPeriodEnd   int64                    `json:"current_period_end"`
	Items             stripeSubscriptionItems   `json:"items"`
	DefaultPaymentMethod *stripePaymentMethodRef `json:"default_payment_method"`
}

type stripeSubscriptionItems struct {
	Data []stripeSubscriptionItem `json:"data"`
}

type stripeSubscriptionItem struct {
	Price stripePrice `json:"price"`
}

type stripePrice struct {
	ID       string         `json:"id"`
	Metadata map[string]string `json:"metadata"`
	Lookup   string         `json:"lookup_key"`
}

type stripePaymentMethodRef struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Card *stripeCardInfo `json:"card"`
}

type stripeCardInfo struct {
	Last4    string `json:"last4"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
	Brand    string `json:"brand"`
}

type stripeSubscriptionList struct {
	Data    []stripeSubscription `json:"data"`
	HasMore bool                 `json:"has_more"`
}

// ---------------------------------------------------------------------------
// Mapping Functions
// ---------------------------------------------------------------------------

// mapStripeInvoice converts a Stripe invoice to a domain types.Invoice.
func mapStripeInvoice(si *stripeInvoice) *types.Invoice {
	inv := &types.Invoice{
		ID:          si.ID,
		AmountCents: si.AmountDue,
		Status:      si.Status,
		PeriodStart: time.Unix(si.PeriodStart, 0).UTC(),
		PeriodEnd:   time.Unix(si.PeriodEnd, 0).UTC(),
		PDFURL:      si.InvoicePDF,
	}

	if si.StatusTransitions.PaidAt > 0 {
		paidAt := time.Unix(si.StatusTransitions.PaidAt, 0).UTC()
		inv.PaidAt = &paidAt
	}

	return inv
}

// mapStripeSubscription converts a Stripe subscription to a domain types.SubscriptionDetails.
func mapStripeSubscription(sub *stripeSubscription) *types.SubscriptionDetails {
	details := &types.SubscriptionDetails{
		Status:             mapSubscriptionStatus(sub.Status),
		CancelAtPeriodEnd:  sub.CancelAtPeriodEnd,
		CurrentPeriodStart: time.Unix(sub.CurrentPeriodStart, 0).UTC(),
		CurrentPeriodEnd:   time.Unix(sub.CurrentPeriodEnd, 0).UTC(),
	}

	// Map plan from the first subscription item's price.
	if len(sub.Items.Data) > 0 {
		details.Plan = mapPriceIDToPlan(sub.Items.Data[0].Price.ID)
	}

	// Map payment method if available.
	if sub.DefaultPaymentMethod != nil && sub.DefaultPaymentMethod.Card != nil {
		details.PaymentMethod = &types.PaymentMethodInfo{
			Type:     sub.DefaultPaymentMethod.Type,
			Last4:    sub.DefaultPaymentMethod.Card.Last4,
			ExpMonth: sub.DefaultPaymentMethod.Card.ExpMonth,
			ExpYear:  sub.DefaultPaymentMethod.Card.ExpYear,
		}
	}

	return details
}

// mapSubscriptionStatus converts a Stripe subscription status string to the domain enum.
func mapSubscriptionStatus(status string) types.SubscriptionStatus {
	switch status {
	case "active":
		return types.SubStatusActive
	case "past_due":
		return types.SubStatusPastDue
	case "canceled":
		return types.SubStatusCanceled
	case "incomplete":
		return types.SubStatusIncomplete
	case "incomplete_expired":
		return types.SubStatusIncompleteExpired
	case "trialing":
		return types.SubStatusTrialing
	case "unpaid":
		return types.SubStatusUnpaid
	default:
		return types.SubscriptionStatus(status)
	}
}

// ---------------------------------------------------------------------------
// Price ID <-> Plan Tier Mapping
// ---------------------------------------------------------------------------

// These price IDs are placeholder references. In production, they would be
// loaded from configuration or environment variables. The mapping is used
// for both creating checkout sessions and interpreting subscription data.

// PriceToPlan maps Stripe Price IDs to domain plan tiers.
// This map is populated at initialization from configuration.
var PriceToPlan = map[string]types.PlanTier{
	"price_starter":    types.PlanStarter,
	"price_pro":        types.PlanPro,
	"price_business":   types.PlanBusiness,
	"price_enterprise": types.PlanEnterprise,
}

// PlanToPrice maps domain plan tiers to Stripe Price IDs.
var PlanToPrice = map[types.PlanTier]string{
	types.PlanStarter:    "price_starter",
	types.PlanPro:        "price_pro",
	types.PlanBusiness:   "price_business",
	types.PlanEnterprise: "price_enterprise",
}

// stripePriceID returns the Stripe Price ID for a given plan tier.
func stripePriceID(plan types.PlanTier) string {
	if id, ok := PlanToPrice[plan]; ok {
		return id
	}
	return "price_" + string(plan)
}

// mapPriceIDToPlan returns the domain PlanTier for a Stripe Price ID.
func mapPriceIDToPlan(priceID string) types.PlanTier {
	if plan, ok := PriceToPlan[priceID]; ok {
		return plan
	}
	// Fallback: try to extract plan name from price ID.
	return types.PlanFree
}

// ---------------------------------------------------------------------------
// Webhook Verification (Section 4.2)
// ---------------------------------------------------------------------------

// StripeVerifier implements WebhookVerifier using stripe-go's webhook
// signature verification. This provides HMAC-SHA256 signature checking
// with timestamp tolerance.
type StripeVerifier struct{}

// Verify validates a Stripe webhook payload against the signature header
// and signing secret. Uses stripe-go's ValidatePayload which checks both
// the HMAC signature and the timestamp tolerance.
func (v *StripeVerifier) Verify(payload []byte, header string, secret string) error {
	return stripe.ValidatePayload(payload, header, secret)
}
