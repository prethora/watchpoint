// Package handlers contains the HTTP handler implementations for the WatchPoint API.
//
// This file implements the Organization handler as defined in 05d-api-organization.md
// Section 3. It covers:
//   - Organization profile retrieval and update
//   - Organization soft delete (with billing cancellation and WatchPoint pause)
//   - Notification preferences management
//   - Organization-wide notification history
//   - Audit log queries
package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"watchpoint/internal/billing"
	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// --- Service Interfaces ---
//
// These interfaces are defined locally following the handler injection pattern
// established in auth.go, billing.go, and apikey.go. The handlers depend on
// abstractions for testability and to avoid coupling to concrete implementations.

// OrgRepo defines the data access contract for organization operations.
// Mirrors the concrete db.OrganizationRepository methods used by this handler.
type OrgRepo interface {
	GetByID(ctx context.Context, id string) (*types.Organization, error)
	Create(ctx context.Context, org *types.Organization) error
	Update(ctx context.Context, org *types.Organization) error
	Delete(ctx context.Context, id string) error
}

// OrgWatchPointRepo provides the WatchPoint repository methods needed by the
// organization handler. Specifically the cascading pause on org deletion.
type OrgWatchPointRepo interface {
	PauseAllByOrgID(ctx context.Context, orgID string) error
}

// OrgNotificationRepo provides notification history access for the organization.
type OrgNotificationRepo interface {
	List(ctx context.Context, filter types.NotificationFilter) ([]types.NotificationHistoryItem, types.PageInfo, error)
}

// OrgAuditRepo provides audit log access for the organization handler.
type OrgAuditRepo interface {
	Log(ctx context.Context, entry *types.AuditEvent) error
	List(ctx context.Context, params types.AuditQueryFilters) ([]*types.AuditEvent, types.PageInfo, error)
}

// OrgBillingService abstracts billing operations needed during org lifecycle.
// Per 05d-api-organization.md Section 2.1.
type OrgBillingService interface {
	// EnsureCustomer creates/retrieves a Stripe customer for the org.
	// Used during org creation (best effort).
	EnsureCustomer(ctx context.Context, orgID string, email string) (string, error)

	// CancelSubscription stops billing immediately.
	// Used during org soft delete (fail-fast).
	CancelSubscription(ctx context.Context, orgID string) error
}

// OrgUserRepo provides user repository methods needed by the organization
// handler. Used during org creation to create the initial owner user.
type OrgUserRepo interface {
	CreateWithProvider(ctx context.Context, user *types.User) error
}

// --- Request/Response Models ---
// Per 05d-api-organization.md Section 3.2.

// OrganizationResponse includes static config only. Dynamic usage is in /v1/usage.
type OrganizationResponse struct {
	ID                      string                         `json:"id"`
	Name                    string                         `json:"name"`
	BillingEmail            string                         `json:"billing_email"`
	Plan                    types.PlanTier                 `json:"plan"`
	PlanLimits              types.PlanLimits               `json:"plan_limits"`
	NotificationPreferences types.NotificationPreferences  `json:"notification_preferences"`
	CreatedAt               time.Time                      `json:"created_at"`
}

// CreateOrganizationRequest is the request body for POST /v1/organization.
// Used during the signup flow (USER-001).
type CreateOrganizationRequest struct {
	Name         string `json:"name" validate:"required,max=200"`
	BillingEmail string `json:"billing_email" validate:"required,email"`
}

// UpdateOrganizationRequest is the request body for PATCH /v1/organization.
// Per 05d-api-organization.md Section 3.2.
type UpdateOrganizationRequest struct {
	Name         *string `json:"name,omitempty" validate:"omitempty,max=200"`
	BillingEmail *string `json:"billing_email,omitempty" validate:"omitempty,email"`
}

// UpdatePreferencesRequest is the request body for PATCH /v1/organization/notification-preferences.
// Per 05d-api-organization.md Section 3.2.
type UpdatePreferencesRequest struct {
	QuietHours *QuietHoursUpdate `json:"quiet_hours,omitempty"`
	Digest     *DigestUpdate     `json:"digest,omitempty"`
}

// QuietHoursUpdate contains the fields that can be updated for quiet hours.
type QuietHoursUpdate struct {
	Enabled  *bool                `json:"enabled,omitempty"`
	Schedule *[]types.QuietPeriod `json:"schedule,omitempty" validate:"omitempty"`
	Timezone *string              `json:"timezone,omitempty" validate:"omitempty,is_timezone"`
}

// DigestUpdate contains the fields that can be updated for digest preferences.
type DigestUpdate struct {
	Enabled      *bool   `json:"enabled,omitempty"`
	Frequency    *string `json:"frequency,omitempty" validate:"omitempty,oneof=daily weekly"`
	DeliveryTime *string `json:"delivery_time,omitempty" validate:"omitempty"`
}

// AuditLogResponse wraps audit events with pagination info.
// Per 05d-api-organization.md Section 3.4.
type AuditLogResponse struct {
	Data     []*types.AuditEvent `json:"data"`
	PageInfo types.PageInfo      `json:"pagination"`
}

// --- Handler ---

// OrganizationHandler manages organization-wide settings.
// Per 05d-api-organization.md Section 3.1.
type OrganizationHandler struct {
	repo           OrgRepo
	wpRepo         OrgWatchPointRepo
	notifRepo      OrgNotificationRepo
	auditRepo      OrgAuditRepo
	userRepo       OrgUserRepo
	planRegistry   billing.PlanRegistry
	billingService OrgBillingService
	validator      *core.Validator
	logger         *slog.Logger
}

// NewOrganizationHandler creates a new OrganizationHandler with the provided dependencies.
// Per 05d-api-organization.md Section 3.1.
func NewOrganizationHandler(
	repo OrgRepo,
	wpRepo OrgWatchPointRepo,
	notifRepo OrgNotificationRepo,
	auditRepo OrgAuditRepo,
	userRepo OrgUserRepo,
	planRegistry billing.PlanRegistry,
	billingSvc OrgBillingService,
	v *core.Validator,
	l *slog.Logger,
) *OrganizationHandler {
	if l == nil {
		l = slog.Default()
	}
	return &OrganizationHandler{
		repo:           repo,
		wpRepo:         wpRepo,
		notifRepo:      notifRepo,
		auditRepo:      auditRepo,
		userRepo:       userRepo,
		planRegistry:   planRegistry,
		billingService: billingSvc,
		validator:      v,
		logger:         l,
	}
}

// RegisterRoutes mounts organization, user, and API key routes.
// Per 05d-api-organization.md Section 6. The userHandler and apiKeyHandler
// are composed into the organization route tree per the architecture spec.
func (h *OrganizationHandler) RegisterRoutes(r chi.Router, userHandler *UserHandler, apiKeyHandler *APIKeyHandler) {
	// Organization
	r.Route("/organization", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.Get)
		r.With(requireMinRole(types.RoleAdmin)).Patch("/", h.Update)
		r.With(requireMinRole(types.RoleOwner)).Delete("/", h.Delete)

		r.Get("/notification-preferences", h.GetPreferences)
		r.With(requireMinRole(types.RoleAdmin)).Patch("/notification-preferences", h.UpdatePreferences)

		// Organization-wide notification history - transparency for all members
		r.Get("/notifications", h.GetNotificationHistory)

		// Audit Log Query - requires Admin or Owner role
		r.With(requireMinRole(types.RoleAdmin)).Get("/audit-logs", h.ListAuditLogs)
	})

	// Users - composed under the organization route space
	if userHandler != nil {
		r.Route("/users", func(r chi.Router) {
			r.Get("/", userHandler.List)
			r.With(requireMinRole(types.RoleAdmin)).Post("/invite", userHandler.Invite)

			r.Route("/{id}", func(r chi.Router) {
				r.Patch("/", userHandler.UpdateRole)
				r.Delete("/", userHandler.Delete)
				r.With(requireMinRole(types.RoleAdmin)).Post("/resend-invite", userHandler.ResendInvite)
			})
		})
	}

	// API Keys - composed under the organization route space
	if apiKeyHandler != nil {
		r.Route("/api-keys", func(r chi.Router) {
			r.Use(requireMinRole(types.RoleAdmin))
			apiKeyHandler.RegisterRoutes(r)
		})
	}
}

// --- Handler Methods ---

// Create handles POST /v1/organization.
//
// Per USER-001 flow simulation and 05d-api-organization.md Section 3.5:
//  1. Decode and validate request.
//  2. Fetch plan limits from PlanRegistry.GetLimits("free").
//  3. Insert Organization record.
//  4. Insert User record (owner role, status='active').
//  5. Call BillingService.EnsureCustomer (Best Effort).
//  6. Return 201 Created.
//
// Note: The spec indicates this wraps DB inserts and Stripe in a logic flow
// that handles partial failures. The DB operations must succeed; Stripe is
// best-effort. Since we don't have a transaction manager wired yet, we perform
// sequential creates and rely on the DB layer's error handling.
func (h *OrganizationHandler) Create(w http.ResponseWriter, r *http.Request) {
	actor, ok := types.GetActor(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Authentication required",
			nil,
		))
		return
	}

	var req CreateOrganizationRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 2: Fetch default (free) plan limits.
	planLimits := h.planRegistry.GetLimits(types.PlanFree)

	// Step 3: Create organization.
	orgID := "org_" + uuid.New().String()
	now := time.Now().UTC()
	org := &types.Organization{
		ID:                      orgID,
		Name:                    req.Name,
		BillingEmail:            req.BillingEmail,
		Plan:                    types.PlanFree,
		PlanLimits:              planLimits,
		NotificationPreferences: types.NotificationPreferences{},
		CreatedAt:               now,
		UpdatedAt:               now,
	}

	if err := h.repo.Create(r.Context(), org); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 4: Create the initial owner user.
	userID := "usr_" + uuid.New().String()
	user := &types.User{
		ID:             userID,
		OrganizationID: orgID,
		Email:          req.BillingEmail,
		Role:           types.RoleOwner,
		Status:         types.UserStatusActive,
		CreatedAt:      now,
	}

	if h.userRepo != nil {
		if err := h.userRepo.CreateWithProvider(r.Context(), user); err != nil {
			core.Error(w, r, err)
			return
		}
	}

	// Step 5: Stripe EnsureCustomer (Best Effort per USER-001 flow).
	// Log error if Stripe fails but do not fail the request.
	if h.billingService != nil {
		if _, err := h.billingService.EnsureCustomer(r.Context(), orgID, req.BillingEmail); err != nil {
			h.logger.WarnContext(r.Context(), "BILLING_SETUP_FAIL: best-effort Stripe customer creation failed",
				"org_id", orgID,
				"error", err,
			)
		}
	}

	// Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "organization.created", orgID, "organization")

	// Step 6: Return 201 Created.
	core.JSON(w, r, http.StatusCreated, core.APIResponse{Data: toOrgResponse(org)})
}

// Get handles GET /v1/organization.
// Per 05d-api-organization.md Section 3.3 and INFO-002.
func (h *OrganizationHandler) Get(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	org, err := h.repo.GetByID(r.Context(), orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: toOrgResponse(org)})
}

// Update handles PATCH /v1/organization.
//
// Per USER-002 flow simulation and 05d-api-organization.md Section 3.5:
//  1. Validate Admin+ role (enforced by middleware).
//  2. Decode and validate request.
//  3. Fetch current org state and apply partial updates.
//  4. Save updated org.
//  5. If billing_email changed, trigger best-effort Stripe sync.
//  6. Emit audit event.
//  7. Return 200 OK.
func (h *OrganizationHandler) Update(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	actor, _ := types.GetActor(r.Context())

	var req UpdateOrganizationRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Fetch current org to apply partial updates.
	org, err := h.repo.GetByID(r.Context(), orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Track if billing email changed for Stripe sync.
	oldBillingEmail := org.BillingEmail

	// Apply partial updates.
	if req.Name != nil {
		org.Name = *req.Name
	}
	if req.BillingEmail != nil {
		org.BillingEmail = *req.BillingEmail
	}

	// Persist the update.
	if err := h.repo.Update(r.Context(), org); err != nil {
		core.Error(w, r, err)
		return
	}

	// Best-effort Stripe sync if billing_email changed.
	if req.BillingEmail != nil && *req.BillingEmail != oldBillingEmail && h.billingService != nil {
		// Fire best-effort: log warning on failure, do not fail API request.
		if _, err := h.billingService.EnsureCustomer(r.Context(), orgID, *req.BillingEmail); err != nil {
			h.logger.WarnContext(r.Context(), "best-effort Stripe email sync failed",
				"org_id", orgID,
				"new_email", *req.BillingEmail,
				"error", err,
			)
		}
	}

	// Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "organization.updated", orgID, "organization")

	// Return updated org response.
	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: toOrgResponse(org)})
}

// Delete handles DELETE /v1/organization.
//
// Per USER-003 flow simulation and 05d-api-organization.md Section 3.5:
//  1. Validate Owner role (enforced by middleware).
//  2. Call BillingService.CancelSubscription (Fail Fast - 500 if error).
//  3. Call wpRepo.PauseAllByOrgID to stop evaluation overhead.
//  4. Set deleted_at = NOW() (soft delete).
//  5. Emit audit event.
//  6. Return 204 No Content.
func (h *OrganizationHandler) Delete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	actor, _ := types.GetActor(r.Context())

	// Step 2: Cancel subscription (Fail Fast).
	// Per USER-003: If Stripe returns error, return 500 to prevent zombie billing.
	if h.billingService != nil {
		if err := h.billingService.CancelSubscription(r.Context(), orgID); err != nil {
			h.logger.ErrorContext(r.Context(), "failed to cancel subscription during org deletion",
				"org_id", orgID,
				"error", err,
			)
			core.Error(w, r, types.NewAppError(
				types.ErrCodeInternalUnexpected,
				"failed to cancel billing subscription; organization deletion aborted",
				err,
			))
			return
		}
	}

	// Step 3: Pause all WatchPoints.
	if h.wpRepo != nil {
		if err := h.wpRepo.PauseAllByOrgID(r.Context(), orgID); err != nil {
			h.logger.ErrorContext(r.Context(), "failed to pause watchpoints during org deletion",
				"org_id", orgID,
				"error", err,
			)
			// Continue with deletion even if pause fails -- the org is being deleted.
		}
	}

	// Step 4: Soft delete the organization.
	if err := h.repo.Delete(r.Context(), orgID); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 5: Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "organization.deleted", orgID, "organization")

	// Step 6: Return 204 No Content.
	w.WriteHeader(http.StatusNoContent)
}

// GetPreferences handles GET /v1/organization/notification-preferences.
// Per 05d-api-organization.md Section 3.3.
func (h *OrganizationHandler) GetPreferences(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	org, err := h.repo.GetByID(r.Context(), orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: org.NotificationPreferences})
}

// UpdatePreferences handles PATCH /v1/organization/notification-preferences.
// Per 05d-api-organization.md Section 3.3 and 3.5.
func (h *OrganizationHandler) UpdatePreferences(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	actor, _ := types.GetActor(r.Context())

	var req UpdatePreferencesRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Fetch current org to apply partial preference updates.
	org, err := h.repo.GetByID(r.Context(), orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	prefs := org.NotificationPreferences

	// Apply quiet hours updates.
	if req.QuietHours != nil {
		if prefs.QuietHours == nil {
			prefs.QuietHours = &types.QuietHoursConfig{}
		}
		if req.QuietHours.Enabled != nil {
			prefs.QuietHours.Enabled = *req.QuietHours.Enabled
		}
		if req.QuietHours.Schedule != nil {
			prefs.QuietHours.Schedule = *req.QuietHours.Schedule
		}
		if req.QuietHours.Timezone != nil {
			prefs.QuietHours.Timezone = *req.QuietHours.Timezone
		}
	}

	// Apply digest updates.
	if req.Digest != nil {
		if prefs.Digest == nil {
			prefs.Digest = &types.DigestConfig{}
		}
		if req.Digest.Enabled != nil {
			prefs.Digest.Enabled = *req.Digest.Enabled
		}
		if req.Digest.Frequency != nil {
			prefs.Digest.Frequency = *req.Digest.Frequency
		}
		if req.Digest.DeliveryTime != nil {
			prefs.Digest.DeliveryTime = *req.Digest.DeliveryTime
		}
	}

	org.NotificationPreferences = prefs

	// Persist the update.
	if err := h.repo.Update(r.Context(), org); err != nil {
		core.Error(w, r, err)
		return
	}

	// Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "organization.preferences_updated", orgID, "organization")

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: prefs})
}

// GetNotificationHistory handles GET /v1/organization/notifications.
// Per 05d-api-organization.md Section 3.5.
// Provides transparency to all members by showing notifications across all WatchPoints.
func (h *OrganizationHandler) GetNotificationHistory(w http.ResponseWriter, r *http.Request) {
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
	filter := types.NotificationFilter{
		OrganizationID: orgID,
	}

	// Parse event_type filter.
	if eventType := r.URL.Query().Get("event_type"); eventType != "" {
		filter.EventTypes = []types.EventType{types.EventType(eventType)}
	}

	// Parse urgency filter.
	if urgency := r.URL.Query().Get("urgency"); urgency != "" {
		filter.Urgency = []types.UrgencyLevel{types.UrgencyLevel(urgency)}
	}

	// Parse pagination.
	limit := 20
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed < 1 || parsed > 100 {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"limit must be a number between 1 and 100",
				nil,
			))
			return
		}
		limit = parsed
	}
	filter.Pagination.HasMore = false
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		filter.Pagination.NextCursor = cursor
	}

	if h.notifRepo == nil {
		core.JSON(w, r, http.StatusOK, core.APIResponse{
			Data: []types.NotificationHistoryItem{},
			Meta: &types.ResponseMeta{
				Pagination: &types.PageInfo{},
			},
		})
		return
	}

	items, pageInfo, err := h.notifRepo.List(r.Context(), filter)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Apply limit trimming if the repo returned more than requested.
	if len(items) > limit {
		pageInfo.HasMore = true
		items = items[:limit]
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{
		Data: items,
		Meta: &types.ResponseMeta{
			Pagination: &pageInfo,
		},
	})
}

// ListAuditLogs handles GET /v1/organization/audit-logs.
//
// Per OBS-011 flow simulation and 05d-api-organization.md Section 3.4:
//  1. Parse query params into types.AuditQueryFilters.
//  2. Call auditRepo.List(ctx, filters).
//  3. Return AuditLogResponse.
func (h *OrganizationHandler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	// Parse query parameters into AuditQueryFilters.
	filters := types.AuditQueryFilters{
		OrganizationID: orgID,
		Limit:          20, // Default per spec
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
		filters.Limit = limit
	}

	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		filters.Cursor = cursor
	}

	if resourceType := r.URL.Query().Get("resource_type"); resourceType != "" {
		filters.ResourceType = resourceType
	}

	if actorID := r.URL.Query().Get("actor_id"); actorID != "" {
		filters.ActorID = actorID
	}

	if startStr := r.URL.Query().Get("start_time"); startStr != "" {
		parsed, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"start_time must be a valid RFC3339 timestamp",
				nil,
			))
			return
		}
		filters.StartTime = parsed.UTC()
	}

	if endStr := r.URL.Query().Get("end_time"); endStr != "" {
		parsed, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"end_time must be a valid RFC3339 timestamp",
				nil,
			))
			return
		}
		filters.EndTime = parsed.UTC()
	}

	events, pageInfo, err := h.auditRepo.List(r.Context(), filters)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	resp := AuditLogResponse{
		Data:     events,
		PageInfo: pageInfo,
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: resp})
}

// --- Helper Functions ---

// emitAuditEvent logs an audit event. Errors are logged but not propagated
// to avoid failing the primary operation due to audit log failures.
func (h *OrganizationHandler) emitAuditEvent(ctx context.Context, actor types.Actor, action, resourceID, resourceType string) {
	if h.auditRepo == nil {
		return
	}

	event := types.AuditEvent{
		Actor:        actor,
		Action:       action,
		ResourceID:   resourceID,
		ResourceType: resourceType,
		Timestamp:    time.Now().UTC(),
	}

	if err := h.auditRepo.Log(ctx, &event); err != nil {
		h.logger.WarnContext(ctx, "failed to log audit event",
			"action", action,
			"resource_id", resourceID,
			"error", err,
		)
	}
}

// toOrgResponse converts a types.Organization to the OrganizationResponse DTO.
func toOrgResponse(org *types.Organization) OrganizationResponse {
	return OrganizationResponse{
		ID:                      org.ID,
		Name:                    org.Name,
		BillingEmail:            org.BillingEmail,
		Plan:                    org.Plan,
		PlanLimits:              org.PlanLimits,
		NotificationPreferences: org.NotificationPreferences,
		CreatedAt:               org.CreatedAt,
	}
}
