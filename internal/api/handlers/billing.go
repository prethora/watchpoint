// Package handlers contains the HTTP handler implementations for the WatchPoint API.
//
// This file implements billing and usage handlers as defined in 05e-api-billing.md.
// It covers:
//   - Subscription management (checkout sessions, portal sessions)
//   - Usage reporting (current usage snapshot, usage history)
//   - Route registration for billing and usage endpoints
package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"watchpoint/internal/config"
	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// --- Service Interfaces ---
//
// These interfaces are defined locally to follow the handler pattern established
// in auth.go: define the service contract in the handler file and inject
// implementations via the constructor. This avoids coupling to concrete types
// and enables test mocking.

// BillingService abstracts interactions with the payment provider (Stripe).
// Per 05e-api-billing.md Section 4.1.
type BillingService interface {
	// EnsureCustomer checks if the org has a Stripe Customer ID.
	// If not, it creates one idempotently. Required before Checkout.
	EnsureCustomer(ctx context.Context, orgID string, email string) (customerID string, err error)

	// CreateCheckoutSession generates a URL for the user to enter payment info.
	CreateCheckoutSession(
		ctx context.Context,
		orgID string,
		targetPlan types.PlanTier,
		redirectURLs types.RedirectURLs,
	) (checkoutURL string, sessionID string, err error)

	// CreatePortalSession generates a URL for self-serve billing management.
	CreatePortalSession(
		ctx context.Context,
		orgID string,
		returnURL string,
	) (portalURL string, err error)

	// GetInvoices retrieves billing history directly from the provider.
	GetInvoices(
		ctx context.Context,
		orgID string,
		params types.ListInvoicesParams,
	) ([]*types.Invoice, types.PageInfo, error)

	// GetSubscription returns the current raw subscription state from the provider.
	GetSubscription(ctx context.Context, orgID string) (*types.SubscriptionDetails, error)
}

// UsageReporter aggregates usage metrics for a specific period.
// Per 05e-api-billing.md Section 4.2.
type UsageReporter interface {
	// GetCurrentUsage returns a snapshot of usage against limits for the current period.
	GetCurrentUsage(ctx context.Context, orgID string) (*types.UsageSnapshot, error)

	// GetUsageHistory returns timeseries data for charting.
	GetUsageHistory(
		ctx context.Context,
		orgID string,
		rangeStart, rangeEnd time.Time,
		granularity types.TimeGranularity,
	) ([]*types.UsageDataPoint, error)
}

// AuditLogger defines the contract for recording business events.
// Per 05e-api-billing.md Section 4.1.
type AuditLogger interface {
	Log(ctx context.Context, event types.AuditEvent) error
}

// OrgBillingReader provides the minimal read access the billing handler needs
// for organization data. This is a focused interface to avoid depending on the
// full OrganizationRepository.
type OrgBillingReader interface {
	// GetByID returns the organization for the given ID.
	// Returns an error if the organization does not exist or is deleted.
	GetByID(ctx context.Context, orgID string) (*types.Organization, error)
}

// --- Request/Response Models ---

// CreateCheckoutRequest is the request body for POST /v1/billing/checkout-session.
// Per 05e-api-billing.md Section 5.2.
//
// Note: SuccessURL and CancelURL are intentionally omitted from the request struct.
// These URLs are constructed server-side using DashboardURL from configuration
// to prevent Open Redirect vulnerabilities.
type CreateCheckoutRequest struct {
	Plan types.PlanTier `json:"plan" validate:"required,oneof=starter pro business"`
}

// CheckoutResponse is the response for POST /v1/billing/checkout-session.
// Per 05e-api-billing.md Section 5.2.
type CheckoutResponse struct {
	CheckoutURL string    `json:"checkout_url"`
	SessionID   string    `json:"session_id"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// PortalResponse is the response for POST /v1/billing/portal-session.
// Per 05e-api-billing.md Section 5.2.
type PortalResponse struct {
	PortalURL string `json:"portal_url"`
}

// UsageResponse is the response for GET /v1/usage.
// Per 05e-api-billing.md Section 6.2.
type UsageResponse struct {
	Period   DateRange          `json:"period"`
	Plan     types.PlanTier     `json:"plan"`
	Snapshot *types.UsageSnapshot `json:"snapshot"`
}

// DateRange defines a start/end time pair for usage periods.
// Per 05e-api-billing.md Section 6.2.
type DateRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// --- Billing Handler ---

// BillingHandler handles synchronous billing actions initiated by the user.
// Per 05e-api-billing.md Section 5.1.
type BillingHandler struct {
	service      BillingService
	orgRepo      OrgBillingReader
	usageHandler *UsageHandler
	audit        AuditLogger
	validator    *core.Validator
	dashboardURL string
	logger       *slog.Logger
}

// NewBillingHandler creates a new BillingHandler with the provided dependencies.
// Per 05e-api-billing.md Section 5.1.
func NewBillingHandler(
	svc BillingService,
	orgRepo OrgBillingReader,
	usage *UsageHandler,
	audit AuditLogger,
	cfg *config.Config,
	v *core.Validator,
	l *slog.Logger,
) *BillingHandler {
	if l == nil {
		l = slog.Default()
	}

	dashboardURL := ""
	if cfg != nil {
		dashboardURL = cfg.Server.DashboardURL
	}

	return &BillingHandler{
		service:      svc,
		orgRepo:      orgRepo,
		usageHandler: usage,
		audit:        audit,
		validator:    v,
		dashboardURL: dashboardURL,
		logger:       l,
	}
}

// RegisterRoutes mounts all billing and usage endpoints.
// Per 05e-api-billing.md Section 8.
func (h *BillingHandler) RegisterRoutes(r chi.Router) {
	// Authenticated Routes (core middleware already applied via parent router)
	r.Group(func(r chi.Router) {
		// Billing
		r.Post("/billing/checkout-session", h.CreateCheckoutSession)
		r.Post("/billing/portal-session", h.CreatePortalSession)
		// GET /billing/invoices requires Admin or Owner due to financial sensitivity.
		// Using requireMinRole(RoleAdmin) which allows both Admin and Owner
		// since the role hierarchy is Owner > Admin > Member.
		r.With(requireMinRole(types.RoleAdmin)).Get("/billing/invoices", h.GetInvoices)
		r.Get("/billing/subscription", h.GetSubscription)

		// Usage (Composed handler)
		r.Get("/usage", h.usageHandler.GetCurrent)
		r.Get("/usage/history", h.usageHandler.GetHistory)
	})
}

// requireMinRole returns middleware that checks if the authenticated Actor
// has at least the specified role level. This is a local helper that provides
// role-based access control for billing routes without requiring a reference
// to the core.Server.
//
// The role hierarchy is: Owner > Admin > Member.
// System actors bypass role checks entirely.
func requireMinRole(minRole types.UserRole) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := types.GetActor(r.Context())
			if !ok {
				core.Error(w, r, types.NewAppError(
					types.ErrCodeAuthTokenMissing,
					"Authentication required",
					nil,
				))
				return
			}

			// System actors bypass role checks.
			if actor.Type == types.ActorTypeSystem {
				next.ServeHTTP(w, r)
				return
			}

			if !actor.RoleHasAtLeast(minRole) {
				core.Error(w, r, types.NewAppError(
					types.ErrCodePermissionRole,
					"Insufficient role for this operation",
					nil,
				))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// --- Billing Handler Methods ---

// CreateCheckoutSession handles POST /v1/billing/checkout-session.
//
// Per DASH-005 flow simulation and 05e-api-billing.md Section 5.3:
//  1. Decode and validate the CreateCheckoutRequest.
//  2. Plan Validation: Reject requests where plan='free' with 400. Downgrades
//     to free tier must be handled via the Stripe Portal, not checkout.
//  3. Self-Healing Customer ID: Calls service.EnsureCustomer to guarantee a
//     Stripe Customer ID exists.
//  4. Server-Controlled Redirect URLs: Constructs SuccessURL and CancelURL
//     server-side using cfg.DashboardURL. This prevents Open Redirect
//     vulnerabilities by never accepting redirect URLs from client input.
//  5. Calls service.CreateCheckoutSession with the constructed URLs.
//  6. Logs "billing.checkout.created" audit event.
//  7. Returns 200 with URL.
func (h *BillingHandler) CreateCheckoutSession(w http.ResponseWriter, r *http.Request) {
	var req CreateCheckoutRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 2: Reject free plan requests -- downgrades go through the portal.
	if req.Plan == types.PlanFree {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"Cannot create checkout session for the free plan. Use the billing portal to downgrade.",
			nil,
		))
		return
	}

	// Extract org ID from authenticated context.
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	// Step 1: Validate org exists.
	org, err := h.orgRepo.GetByID(r.Context(), orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 3: Self-Healing Customer ID.
	_, err = h.service.EnsureCustomer(r.Context(), orgID, org.BillingEmail)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "failed to ensure Stripe customer",
			"org_id", orgID,
			"error", err,
		)
		core.Error(w, r, err)
		return
	}

	// Step 4: Construct server-controlled redirect URLs (Open Redirect prevention).
	redirectURLs := types.RedirectURLs{
		Success: h.dashboardURL + "/billing?success=true",
		Cancel:  h.dashboardURL + "/billing?canceled=true",
	}

	// Step 5: Create checkout session.
	checkoutURL, sessionID, err := h.service.CreateCheckoutSession(
		r.Context(), orgID, req.Plan, redirectURLs,
	)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "failed to create checkout session",
			"org_id", orgID,
			"plan", req.Plan,
			"error", err,
		)
		core.Error(w, r, err)
		return
	}

	// Step 6: Log audit event.
	if h.audit != nil {
		actor, _ := types.GetActor(r.Context())
		auditEvent := types.AuditEvent{
			Actor:        actor,
			Action:       "billing.checkout.created",
			ResourceID:   orgID,
			ResourceType: "organization",
			Timestamp:    time.Now().UTC(),
		}
		if auditErr := h.audit.Log(r.Context(), auditEvent); auditErr != nil {
			h.logger.WarnContext(r.Context(), "failed to log audit event for checkout",
				"org_id", orgID,
				"error", auditErr,
			)
		}
	}

	// Step 7: Return response.
	// Checkout sessions expire after 24 hours per Stripe's default behavior.
	resp := CheckoutResponse{
		CheckoutURL: checkoutURL,
		SessionID:   sessionID,
		ExpiresAt:   time.Now().UTC().Add(24 * time.Hour),
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: resp})
}

// CreatePortalSession handles POST /v1/billing/portal-session.
//
// Per DASH-006 flow simulation and 05e-api-billing.md Section 5.3:
//  1. Self-Healing Customer ID: Calls service.EnsureCustomer to guarantee a
//     Stripe Customer ID exists.
//  2. Server-Controlled Return URL: Constructs the return URL server-side
//     using cfg.DashboardURL. This prevents Open Redirect vulnerabilities.
//  3. Calls service.CreatePortalSession with the constructed return URL.
//  4. Returns 200 with URL.
func (h *BillingHandler) CreatePortalSession(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	// Step 1: Validate org exists and get billing email for EnsureCustomer.
	org, err := h.orgRepo.GetByID(r.Context(), orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Self-Healing Customer ID.
	_, err = h.service.EnsureCustomer(r.Context(), orgID, org.BillingEmail)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "failed to ensure Stripe customer for portal",
			"org_id", orgID,
			"error", err,
		)
		core.Error(w, r, err)
		return
	}

	// Step 2: Construct server-controlled return URL (Open Redirect prevention).
	returnURL := h.dashboardURL + "/billing"

	// Step 3: Create portal session.
	portalURL, err := h.service.CreatePortalSession(r.Context(), orgID, returnURL)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "failed to create portal session",
			"org_id", orgID,
			"error", err,
		)
		core.Error(w, r, err)
		return
	}

	// Step 4: Return response.
	resp := PortalResponse{
		PortalURL: portalURL,
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: resp})
}

// GetInvoices handles GET /v1/billing/invoices.
//
// Per INFO-005 flow simulation and 05e-api-billing.md Section 5.3:
//  1. Proxies request to service.GetInvoices.
//  2. Returns 200 with list and pagination info.
func (h *BillingHandler) GetInvoices(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	// Parse query parameters for pagination.
	params := types.ListInvoicesParams{
		Limit: 20, // Default per spec
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit < 1 || limit > 100 {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"limit must be a number between 1 and 100",
				nil,
			))
			return
		}
		params.Limit = limit
	}

	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		params.Cursor = cursor
	}

	invoices, pageInfo, err := h.service.GetInvoices(r.Context(), orgID, params)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	resp := core.APIResponse{
		Data: invoices,
		Meta: &types.ResponseMeta{
			Pagination: &pageInfo,
		},
	}

	core.JSON(w, r, http.StatusOK, resp)
}

// GetSubscription handles GET /v1/billing/subscription.
//
// Per INFO-006 flow simulation and 05e-api-billing.md Section 5.3:
//  1. Calls service.GetSubscription to fetch latest state from Stripe.
//  2. Opportunistic Sync (INFO-006): Compares the returned Stripe subscription
//     status and plan against the local database record. If mismatched, the
//     handler issues a synchronous update to align local state with Stripe.
//  3. Returns current status from provider.
func (h *BillingHandler) GetSubscription(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	details, err := h.service.GetSubscription(r.Context(), orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Opportunistic Sync: Compare remote state with local DB and sync if needed.
	// This handles race conditions where the webhook lags behind Stripe Checkout
	// redirect (e.g., user just paid and was redirected before webhook arrived).
	//
	// Note: The actual DB update for opportunistic sync requires a write-capable
	// OrgRepository method. Since OrganizationRepository is still a stub interface,
	// the handler performs the comparison and logs drift but defers the actual update
	// to when the repository gains UpdateSubscriptionStatus. The subscription details
	// returned to the client are always from Stripe (the source of truth).
	org, orgErr := h.orgRepo.GetByID(r.Context(), orgID)
	if orgErr == nil && org != nil {
		// Check for drift between Stripe and local state.
		if details.Plan != org.Plan {
			h.logger.InfoContext(r.Context(), "billing state drift detected during read",
				"org_id", orgID,
				"local_plan", org.Plan,
				"stripe_plan", details.Plan,
			)
		}
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: details})
}

// --- Usage Handler ---

// UsageHandler provides visibility into consumption and limits.
// Per 05e-api-billing.md Section 6.1.
type UsageHandler struct {
	reporter  UsageReporter
	orgRepo   OrgBillingReader
	validator *core.Validator
}

// NewUsageHandler creates a new UsageHandler with the provided dependencies.
func NewUsageHandler(
	reporter UsageReporter,
	orgRepo OrgBillingReader,
	v *core.Validator,
) *UsageHandler {
	return &UsageHandler{
		reporter:  reporter,
		orgRepo:   orgRepo,
		validator: v,
	}
}

// GetCurrent handles GET /v1/usage.
//
// Per INFO-003 flow simulation and 05e-api-billing.md Section 6.3:
//  1. Calls reporter.GetCurrentUsage(orgID).
//  2. Returns usage snapshot with limit metadata.
func (h *UsageHandler) GetCurrent(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	snapshot, err := h.reporter.GetCurrentUsage(r.Context(), orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Fetch org to include plan tier and billing period in the response.
	org, orgErr := h.orgRepo.GetByID(r.Context(), orgID)
	if orgErr != nil {
		core.Error(w, r, orgErr)
		return
	}

	// The period is derived from the current billing cycle.
	// For simplicity, use the start of the current month and end of current month
	// as the billing period. This will be refined when the billing period is stored
	// on the organization record.
	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0).Add(-time.Nanosecond)

	resp := UsageResponse{
		Period: DateRange{
			Start: periodStart,
			End:   periodEnd,
		},
		Plan:     org.Plan,
		Snapshot: snapshot,
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: resp})
}

// GetHistory handles GET /v1/usage/history.
//
// Per INFO-004 flow simulation and 05e-api-billing.md Section 6.3:
//  1. Parses query params (start, end, granularity).
//  2. Calls reporter.GetUsageHistory, passing the granularity param to the repository.
//  3. Returns timeseries data.
func (h *UsageHandler) GetHistory(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	// Parse query parameters.
	now := time.Now().UTC()

	// Default range: last 30 days.
	rangeStart := now.AddDate(0, 0, -30)
	rangeEnd := now

	if startStr := r.URL.Query().Get("start"); startStr != "" {
		parsed, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"start must be a valid RFC3339 timestamp",
				nil,
			))
			return
		}
		rangeStart = parsed.UTC()
	}

	if endStr := r.URL.Query().Get("end"); endStr != "" {
		parsed, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"end must be a valid RFC3339 timestamp",
				nil,
			))
			return
		}
		rangeEnd = parsed.UTC()
	}

	// Validate range.
	if rangeEnd.Before(rangeStart) {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"end must be after start",
			nil,
		))
		return
	}

	// Parse granularity (default: daily).
	granularity := types.GranularityDaily
	if g := r.URL.Query().Get("granularity"); g != "" {
		switch types.TimeGranularity(g) {
		case types.GranularityDaily, types.GranularityMonthly:
			granularity = types.TimeGranularity(g)
		default:
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"granularity must be 'daily' or 'monthly'",
				nil,
			))
			return
		}
	}

	dataPoints, err := h.reporter.GetUsageHistory(r.Context(), orgID, rangeStart, rangeEnd, granularity)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: dataPoints})
}
