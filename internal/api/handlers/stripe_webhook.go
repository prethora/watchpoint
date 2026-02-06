// Package handlers contains the HTTP handler implementations for the WatchPoint API.
//
// This file implements the Stripe webhook handler as defined in 05e-api-billing.md
// Section 7 and flow simulation BILL-007.
//
// The handler is NOT behind auth middleware -- it is called directly by Stripe.
// Security is provided by verifying the Stripe-Signature header using HMAC-SHA256.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"watchpoint/internal/core"
	"watchpoint/internal/external"
	"watchpoint/internal/types"
)

// maxWebhookBodySize is the maximum allowed size of a Stripe webhook payload (64 KB).
// Stripe webhook payloads are typically small; this limit protects against abuse.
const maxWebhookBodySize = 64 * 1024

// ---------------------------------------------------------------------------
// Interfaces for webhook handler dependencies
// ---------------------------------------------------------------------------

// SubscriptionStateUpdater manages local billing state synchronization.
// This is the subset of SubscriptionStateRepository needed by the webhook handler.
type SubscriptionStateUpdater interface {
	// UpdateSubscriptionStatus atomically updates the plan and status.
	// Uses optimistic locking via last_subscription_event_at (Section 9.2).
	// Rejects updates for deleted organizations (zombie check, Section 9.1).
	UpdateSubscriptionStatus(
		ctx context.Context,
		orgID string,
		newPlan types.PlanTier,
		status types.SubscriptionStatus,
		eventTimestamp time.Time,
	) error

	// UpdatePaymentFailure records dunning state.
	UpdatePaymentFailure(ctx context.Context, orgID string, failedAt time.Time) error
}

// WatchPointResumer provides the ability to resume paused WatchPoints.
// This is a focused interface for the auto-resume functionality (Section 9.5).
type WatchPointResumer interface {
	// ResumeAllByOrgID resumes all WatchPoints paused for the given reason.
	// Only WatchPoints paused for the specified reason are resumed.
	ResumeAllByOrgID(ctx context.Context, orgID string, reason types.PausedReason) error
}

// ---------------------------------------------------------------------------
// Stripe Webhook Handler
// ---------------------------------------------------------------------------

// StripeWebhookHandler handles asynchronous events from Stripe.
// It is unauthenticated (no JWT) but verifies the provider signature.
// Per 05e-api-billing.md Section 7.
type StripeWebhookHandler struct {
	verifier external.WebhookVerifier
	subRepo  SubscriptionStateUpdater
	resumer  WatchPointResumer
	secret   string
	logger   *slog.Logger
}

// NewStripeWebhookHandler creates a new StripeWebhookHandler with the provided dependencies.
func NewStripeWebhookHandler(
	verifier external.WebhookVerifier,
	subRepo SubscriptionStateUpdater,
	resumer WatchPointResumer,
	secret string,
	logger *slog.Logger,
) *StripeWebhookHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &StripeWebhookHandler{
		verifier: verifier,
		subRepo:  subRepo,
		resumer:  resumer,
		secret:   secret,
		logger:   logger,
	}
}

// RegisterRoutes mounts the Stripe webhook endpoint.
// This is separate from BillingHandler.RegisterRoutes because webhook routes
// are public (no auth middleware).
// Per 05e-api-billing.md Section 8.
func (h *StripeWebhookHandler) RegisterRoutes(r chi.Router) {
	r.Post("/webhooks/stripe", h.Handle)
}

// Handle processes incoming Stripe webhook events.
//
// Per BILL-007 flow simulation and 05e-api-billing.md Section 7.2:
//  1. Reads body and "Stripe-Signature" header.
//  2. Verifies signature using the webhook signing secret.
//  3. Parses event JSON.
//  4. Extracts orgID from event metadata/client_reference_id.
//  5. Routes to the appropriate handler based on event type.
//  6. Returns 200 OK immediately.
func (h *StripeWebhookHandler) Handle(w http.ResponseWriter, r *http.Request) {
	// Step 1: Read the raw body with size limit.
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodySize)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.WarnContext(r.Context(), "failed to read webhook body",
			"error", err,
		)
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"failed to read request body",
			err,
		))
		return
	}

	// Step 2: Verify the Stripe-Signature header.
	sigHeader := r.Header.Get("Stripe-Signature")
	if sigHeader == "" {
		h.logger.WarnContext(r.Context(), "missing Stripe-Signature header")
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"missing Stripe-Signature header",
			nil,
		))
		return
	}

	if err := h.verifier.Verify(payload, sigHeader, h.secret); err != nil {
		h.logger.WarnContext(r.Context(), "webhook signature verification failed",
			"error", err,
		)
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenInvalid,
			"webhook signature verification failed",
			err,
		))
		return
	}

	// Step 3: Parse the event JSON.
	var event stripeWebhookEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		h.logger.ErrorContext(r.Context(), "failed to parse webhook event JSON",
			"error", err,
		)
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"invalid webhook event JSON",
			err,
		))
		return
	}

	h.logger.InfoContext(r.Context(), "processing stripe webhook event",
		"event_id", event.ID,
		"event_type", event.Type,
	)

	// Step 4-5: Extract orgID and route by event type.
	if err := h.routeEvent(r.Context(), &event); err != nil {
		h.logger.ErrorContext(r.Context(), "webhook event processing failed",
			"event_id", event.ID,
			"event_type", event.Type,
			"error", err,
		)
		// Return 200 anyway to prevent Stripe from retrying.
		// The error is logged for investigation. Per Stripe best practices,
		// we acknowledge receipt even if internal processing fails to avoid
		// infinite retry loops.
	}

	// Step 6: Return 200 OK.
	w.WriteHeader(http.StatusOK)
}

// routeEvent dispatches the webhook event to the appropriate handler method
// based on the event type.
func (h *StripeWebhookHandler) routeEvent(ctx context.Context, event *stripeWebhookEvent) error {
	switch event.Type {
	case external.EventStripeCheckoutCompleted:
		return h.handleCheckoutCompleted(ctx, event)

	case external.EventStripeSubUpdated:
		return h.handleSubscriptionUpdated(ctx, event)

	case external.EventStripeSubDeleted:
		return h.handleSubscriptionDeleted(ctx, event)

	case external.EventStripeInvoicePaid:
		return h.handleInvoicePaid(ctx, event)

	case external.EventStripePaymentFailed:
		return h.handlePaymentFailed(ctx, event)

	default:
		h.logger.InfoContext(ctx, "ignoring unhandled webhook event type",
			"event_type", event.Type,
		)
		return nil
	}
}

// handleCheckoutCompleted processes checkout.session.completed events.
// This confirms a new subscription after the user completes the Stripe Checkout flow.
// Per BILL-002 flow simulation.
func (h *StripeWebhookHandler) handleCheckoutCompleted(ctx context.Context, event *stripeWebhookEvent) error {
	orgID := event.extractOrgID()
	if orgID == "" {
		return fmt.Errorf("checkout.session.completed: missing org_id in event %s", event.ID)
	}

	plan := event.extractPlan()
	eventTime := event.eventTimestamp()

	h.logger.InfoContext(ctx, "processing checkout completed",
		"event_id", event.ID,
		"org_id", orgID,
		"plan", plan,
	)

	return h.subRepo.UpdateSubscriptionStatus(ctx, orgID, plan, types.SubStatusActive, eventTime)
}

// handleSubscriptionUpdated processes customer.subscription.updated events.
// This handles both upgrades (BILL-003) and downgrades (BILL-004).
func (h *StripeWebhookHandler) handleSubscriptionUpdated(ctx context.Context, event *stripeWebhookEvent) error {
	orgID := event.extractOrgID()
	if orgID == "" {
		return fmt.Errorf("customer.subscription.updated: missing org_id in event %s", event.ID)
	}

	plan := event.extractPlanFromSubscription()
	status := event.extractSubscriptionStatus()
	eventTime := event.eventTimestamp()

	h.logger.InfoContext(ctx, "processing subscription updated",
		"event_id", event.ID,
		"org_id", orgID,
		"plan", plan,
		"status", status,
	)

	return h.subRepo.UpdateSubscriptionStatus(ctx, orgID, plan, status, eventTime)
}

// handleSubscriptionDeleted processes customer.subscription.deleted events.
// This handles subscription cancellation -- the org reverts to free tier.
func (h *StripeWebhookHandler) handleSubscriptionDeleted(ctx context.Context, event *stripeWebhookEvent) error {
	orgID := event.extractOrgID()
	if orgID == "" {
		return fmt.Errorf("customer.subscription.deleted: missing org_id in event %s", event.ID)
	}

	eventTime := event.eventTimestamp()

	h.logger.InfoContext(ctx, "processing subscription deleted",
		"event_id", event.ID,
		"org_id", orgID,
	)

	return h.subRepo.UpdateSubscriptionStatus(ctx, orgID, types.PlanFree, types.SubStatusCanceled, eventTime)
}

// handleInvoicePaid processes invoice.paid events.
// Per BILL-007 and Section 9.5: triggers auto-resume of WatchPoints paused
// due to billing delinquency.
func (h *StripeWebhookHandler) handleInvoicePaid(ctx context.Context, event *stripeWebhookEvent) error {
	orgID := event.extractOrgID()
	if orgID == "" {
		return fmt.Errorf("invoice.paid: missing org_id in event %s", event.ID)
	}

	h.logger.InfoContext(ctx, "processing invoice paid",
		"event_id", event.ID,
		"org_id", orgID,
	)

	// Auto-resume: Restore WatchPoints paused for billing delinquency.
	// Only WatchPoints with paused_reason = "billing_delinquency" are resumed;
	// user-paused WatchPoints remain paused.
	//
	// NOTE: ResumeAllByOrgID is stubbed for now (implemented in Task 5.1).
	// The resumer may be nil if the dependency is not yet available.
	if h.resumer != nil {
		if err := h.resumer.ResumeAllByOrgID(ctx, orgID, types.PausedReasonBillingDelinquency); err != nil {
			h.logger.ErrorContext(ctx, "failed to auto-resume watchpoints after payment",
				"org_id", orgID,
				"error", err,
			)
			// Log but do not fail the webhook -- the payment was successful
			// and the resume can be retried manually or by a scheduled job.
		}
	}

	h.logger.InfoContext(ctx, "auto-resume completed for invoice.paid",
		"org_id", orgID,
	)

	// NOTE: Section 9.5 also requires inserting a billing_receipt notification
	// and Section 9.4 describes event propagation through the notification system.
	// These notification inserts will be implemented when the notification
	// infrastructure is available. For now, we log the event.

	return nil
}

// handlePaymentFailed processes invoice.payment_failed events.
// Per BILL-005 flow simulation and Section 9.6:
// Records dunning state and triggers a billing_alert notification.
func (h *StripeWebhookHandler) handlePaymentFailed(ctx context.Context, event *stripeWebhookEvent) error {
	orgID := event.extractOrgID()
	if orgID == "" {
		return fmt.Errorf("invoice.payment_failed: missing org_id in event %s", event.ID)
	}

	eventTime := event.eventTimestamp()

	h.logger.WarnContext(ctx, "processing payment failure",
		"event_id", event.ID,
		"org_id", orgID,
	)

	// Record dunning state.
	if err := h.subRepo.UpdatePaymentFailure(ctx, orgID, eventTime); err != nil {
		return fmt.Errorf("UpdatePaymentFailure: %w", err)
	}

	// NOTE: Section 9.6 requires inserting a billing_alert notification into
	// the notifications table to trigger the Email Worker. This will be
	// implemented when the notification infrastructure is available.
	// For now, the dunning state is recorded and the failure is logged.

	return nil
}

// ---------------------------------------------------------------------------
// Stripe Event Parsing
// ---------------------------------------------------------------------------

// stripeWebhookEvent is a minimal representation of a Stripe webhook event
// tailored to extract the fields needed for routing and processing.
// We avoid importing the full stripe.Event type to keep the handler
// decoupled from the stripe-go library and to make testing straightforward.
type stripeWebhookEvent struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Created int64           `json:"created"`
	Data    json.RawMessage `json:"data"`
}

// stripeEventData wraps the event data object.
type stripeEventData struct {
	Object json.RawMessage `json:"object"`
}

// stripeCheckoutSessionObj represents the minimal fields from a Stripe
// checkout.session.completed event's data object.
type stripeCheckoutSessionObj struct {
	ClientReferenceID string            `json:"client_reference_id"`
	Metadata          map[string]string `json:"metadata"`
	Subscription      string            `json:"subscription"`
}

// stripeSubscriptionObj represents the minimal fields from a Stripe
// customer.subscription.updated/deleted event's data object.
type stripeSubscriptionObj struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Metadata map[string]string `json:"metadata"`
	Items    stripeSubItems    `json:"items"`
}

type stripeSubItems struct {
	Data []stripeSubItem `json:"data"`
}

type stripeSubItem struct {
	Price stripeSubPrice `json:"price"`
}

type stripeSubPrice struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
}

// stripeInvoiceObj represents the minimal fields from an invoice event's
// data object.
type stripeInvoiceObj struct {
	Subscription        string            `json:"subscription"`
	Metadata            map[string]string `json:"metadata"`
	SubscriptionDetails *stripeSubDetails `json:"subscription_details"`
}

type stripeSubDetails struct {
	Metadata map[string]string `json:"metadata"`
}

// eventTimestamp returns the event's created timestamp as a time.Time.
func (e *stripeWebhookEvent) eventTimestamp() time.Time {
	return time.Unix(e.Created, 0).UTC()
}

// extractOrgID extracts the organization ID from the event payload.
// The org_id is stored differently depending on the event type:
//   - checkout.session.completed: client_reference_id or metadata.org_id
//   - subscription events: metadata.org_id on the subscription object
//   - invoice events: subscription_details.metadata.org_id or metadata.org_id
func (e *stripeWebhookEvent) extractOrgID() string {
	var data stripeEventData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return ""
	}

	switch e.Type {
	case external.EventStripeCheckoutCompleted:
		var session stripeCheckoutSessionObj
		if err := json.Unmarshal(data.Object, &session); err != nil {
			return ""
		}
		// Prefer client_reference_id (set by our CreateCheckoutSession).
		if session.ClientReferenceID != "" {
			return session.ClientReferenceID
		}
		return session.Metadata["org_id"]

	case external.EventStripeSubUpdated, external.EventStripeSubDeleted:
		var sub stripeSubscriptionObj
		if err := json.Unmarshal(data.Object, &sub); err != nil {
			return ""
		}
		return sub.Metadata["org_id"]

	case external.EventStripeInvoicePaid, external.EventStripePaymentFailed:
		var invoice stripeInvoiceObj
		if err := json.Unmarshal(data.Object, &invoice); err != nil {
			return ""
		}
		// Try subscription_details.metadata first (Stripe populates this).
		if invoice.SubscriptionDetails != nil {
			if orgID := invoice.SubscriptionDetails.Metadata["org_id"]; orgID != "" {
				return orgID
			}
		}
		// Fall back to invoice metadata.
		return invoice.Metadata["org_id"]

	default:
		return ""
	}
}

// extractPlan extracts the plan tier from a checkout.session.completed event.
func (e *stripeWebhookEvent) extractPlan() types.PlanTier {
	var data stripeEventData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return types.PlanFree
	}

	var session stripeCheckoutSessionObj
	if err := json.Unmarshal(data.Object, &session); err != nil {
		return types.PlanFree
	}

	if plan, ok := session.Metadata["plan"]; ok {
		return types.PlanTier(plan)
	}

	return types.PlanFree
}

// extractPlanFromSubscription extracts the plan tier from a subscription event
// by looking at the first item's price ID.
func (e *stripeWebhookEvent) extractPlanFromSubscription() types.PlanTier {
	var data stripeEventData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return types.PlanFree
	}

	var sub stripeSubscriptionObj
	if err := json.Unmarshal(data.Object, &sub); err != nil {
		return types.PlanFree
	}

	// Map price ID to plan tier using the external package mapping.
	if len(sub.Items.Data) > 0 {
		priceID := sub.Items.Data[0].Price.ID
		if plan, ok := external.PriceToPlan[priceID]; ok {
			return plan
		}
	}

	return types.PlanFree
}

// extractSubscriptionStatus extracts the subscription status from a
// subscription event.
func (e *stripeWebhookEvent) extractSubscriptionStatus() types.SubscriptionStatus {
	var data stripeEventData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return types.SubStatusActive
	}

	var sub stripeSubscriptionObj
	if err := json.Unmarshal(data.Object, &sub); err != nil {
		return types.SubStatusActive
	}

	switch sub.Status {
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
		return types.SubscriptionStatus(sub.Status)
	}
}
