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
// Email Integration (SendGrid) — Section 5
// ---------------------------------------------------------------------------

// EmailProvider abstracts interactions with the email delivery service (SendGrid).
// Implementations map domain inputs to provider-specific template calls.
type EmailProvider interface {
	// Send transmits an email using the provider's template engine.
	// Returns the provider's message ID for tracking and correlation.
	Send(ctx context.Context, input types.SendInput) (providerMsgID string, err error)
}

// EmailVerifier abstracts SendGrid webhook ECDSA signature verification.
type EmailVerifier interface {
	// Verify checks the ECDSA signature from X-Twilio-Email-Event-Webhook-Signature.
	// Returns (true, nil) if the signature is valid, (false, nil) if invalid,
	// or (false, err) if verification could not be performed.
	Verify(payload []byte, signature string, timestamp string, publicKey string) (bool, error)
}

// SendGrid event type constants for bounce/complaint handling.
const (
	EventSendGridBounce    = "bounce"
	EventSendGridComplaint = "spamreport"
)

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
