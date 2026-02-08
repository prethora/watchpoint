package external

import (
	"context"

	"watchpoint/internal/types"
)

// ---------------------------------------------------------------------------
// Billing Integration (Stripe) — Section 4
// ---------------------------------------------------------------------------

// BillingService abstracts interactions with the payment provider (Stripe).
// Implementations translate between domain types and vendor-specific APIs.
type BillingService interface {
	// EnsureCustomer retrieves or creates a Stripe customer for the given org.
	// Returns the Stripe customer ID. Uses search-first logic to prevent duplicates.
	EnsureCustomer(ctx context.Context, orgID string, email string) (string, error)

	// CreateCheckoutSession generates a Stripe Checkout URL for the user to enter
	// payment info. orgID is set as client_reference_id for webhook correlation.
	CreateCheckoutSession(ctx context.Context, orgID string, plan types.PlanTier, urls types.RedirectURLs) (checkoutURL string, sessionID string, err error)

	// CreatePortalSession generates a Stripe Billing Portal URL for self-serve
	// billing management.
	CreatePortalSession(ctx context.Context, orgID string, returnURL string) (portalURL string, err error)

	// GetInvoices retrieves invoices for the organization with cursor-based pagination.
	// The cursor maps to Stripe's starting_after parameter.
	GetInvoices(ctx context.Context, orgID string, params types.ListInvoicesParams) ([]*types.Invoice, types.PageInfo, error)

	// GetSubscription retrieves the current subscription details for the organization.
	GetSubscription(ctx context.Context, orgID string) (*types.SubscriptionDetails, error)
}

// WebhookVerifier abstracts Stripe webhook signature checking.
type WebhookVerifier interface {
	// Verify validates a webhook payload against the provided signature header
	// and signing secret. Returns nil on success, an error on failure.
	Verify(payload []byte, header string, secret string) error
}

// Stripe event type constants prevent magic strings in webhook handlers.
const (
	EventStripeCheckoutCompleted = "checkout.session.completed"
	EventStripeInvoicePaid       = "invoice.paid"
	EventStripePaymentFailed     = "invoice.payment_failed"
	EventStripeSubUpdated        = "customer.subscription.updated"
	EventStripeSubDeleted        = "customer.subscription.deleted"
)

// ---------------------------------------------------------------------------
// Email Integration (AWS SES) — Section 5
// ---------------------------------------------------------------------------

// EmailProvider abstracts interactions with the email delivery service (AWS SES).
// Implementations transmit pre-rendered email content (Subject, BodyHTML, BodyText).
type EmailProvider interface {
	// Send transmits an email with pre-rendered content.
	// Returns the provider's message ID for tracking and correlation.
	Send(ctx context.Context, input types.SendInput) (providerMsgID string, err error)
}

// ---------------------------------------------------------------------------
// Identity Integration (OAuth) — Section 6
// ---------------------------------------------------------------------------

// OAuthProvider abstracts a single OAuth identity provider (e.g., Google, GitHub).
type OAuthProvider interface {
	// Name returns the provider identifier (e.g., "google", "github").
	Name() string

	// GetLoginURL generates the OAuth authorization URL with the given state parameter.
	GetLoginURL(state string) string

	// Exchange trades an authorization code for a normalized user profile.
	// Does NOT return access/refresh tokens — scope is authentication only.
	Exchange(ctx context.Context, code string) (*types.OAuthProfile, error)
}

// OAuthManager provides access to registered OAuth providers by name.
type OAuthManager interface {
	// GetProvider returns the OAuthProvider registered under the given name.
	// Returns an error if no provider is registered with that name.
	GetProvider(name string) (OAuthProvider, error)
}

// ---------------------------------------------------------------------------
// RunPod Integration (Inference) — Section 5.1 of 09-scheduled-jobs.md
// ---------------------------------------------------------------------------

// RunPodClient abstracts interactions with the RunPod Serverless inference API.
// Implementations translate between domain types and RunPod's REST endpoints.
type RunPodClient interface {
	// TriggerInference calls the RunPod API to start an inference job.
	// Returns the external Job ID (e.g., RunPod ID) on success.
	TriggerInference(ctx context.Context, payload types.InferencePayload) (string, error)

	// CancelJob terminates a running job on the external provider.
	// Used by reconciliation to stop hanging jobs and prevent cost accumulation.
	CancelJob(ctx context.Context, externalID string) error

	// GetJobStatus retrieves the current status of a RunPod job.
	// Used for polling job completion in test tooling and operational scripts.
	GetJobStatus(ctx context.Context, jobID string) (*RunPodJobStatus, error)
}

// RunPodJobStatus represents the status of a RunPod serverless job.
type RunPodJobStatus struct {
	ID            string         `json:"id"`
	Status        string         `json:"status"` // IN_QUEUE, IN_PROGRESS, COMPLETED, FAILED, CANCELLED, TIMED_OUT
	Output        map[string]any `json:"output,omitempty"`
	Error         string         `json:"error,omitempty"`
	ExecutionTime float64        `json:"executionTime,omitempty"`
}
