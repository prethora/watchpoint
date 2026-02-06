// Package handlers contains the HTTP handler implementations for the WatchPoint API.
//
// This file implements the WatchPoint handler as defined in 05b-api-watchpoints.md.
// It covers:
//   - Create (Event Mode and Monitor Mode), Get, Update, Delete
//   - Pause and Resume with side effects (cancel deferred, trigger evaluation)
//   - Notification history retrieval
//   - Route registration
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// --- Service Interfaces ---
//
// These interfaces are defined locally following the handler injection pattern
// established in auth.go, billing.go, and organization.go. The handlers depend on
// abstractions for testability and to avoid coupling to concrete implementations.

// WPRepo defines the data access contract for WatchPoint operations.
// Mirrors the concrete db.WatchPointRepository methods used by this handler.
type WPRepo interface {
	Create(ctx context.Context, wp *types.WatchPoint) error
	GetByID(ctx context.Context, id string, orgID string) (*types.WatchPoint, error)
	Update(ctx context.Context, wp *types.WatchPoint) error
	Delete(ctx context.Context, id string, orgID string) error
	CreateBatch(ctx context.Context, wps []*types.WatchPoint) (createdIndices []int, failedIndices map[int]error, err error)
	UpdateTagsBatch(ctx context.Context, orgID string, filter types.BulkFilter, addTags []string, removeTags []string) (int64, error)
}

// WPNotifRepo provides notification history access for the WatchPoint handler.
type WPNotifRepo interface {
	List(ctx context.Context, filter types.NotificationFilter) ([]*types.NotificationHistoryItem, types.PageInfo, error)
	CancelDeferredDeliveries(ctx context.Context, watchpointID string) error
}

// WPForecastProvider fetches a lightweight snapshot of current conditions.
// Returns nil error on failure (graceful degradation).
type WPForecastProvider interface {
	GetSnapshot(ctx context.Context, lat, lon float64) (*types.ForecastSnapshot, error)
}

// WPUsageEnforcer checks plan limits before creation.
type WPUsageEnforcer interface {
	CheckLimit(ctx context.Context, orgID string, resource types.ResourceType, count int) error
}

// WPAuditLogger records business events.
type WPAuditLogger interface {
	Log(ctx context.Context, entry *types.AuditEvent) error
}

// WPEvalTrigger enqueues an immediate evaluation check.
type WPEvalTrigger interface {
	TriggerEvaluation(ctx context.Context, wpID string, reason string) error
}

// --- Request/Response Models ---
// Per 05b-api-watchpoints.md Section 4.

// CreateWatchPointRequest is the request body for POST /v1/watchpoints.
type CreateWatchPointRequest struct {
	Name           string                `json:"name" validate:"required,max=200"`
	Location       types.Location        `json:"location" validate:"required"`
	Timezone       string                `json:"timezone" validate:"required,is_timezone"`
	TimeWindow     *types.TimeWindow     `json:"time_window,omitempty" validate:"required_without=MonitorConfig,excluded_with=MonitorConfig"`
	MonitorConfig  *types.MonitorConfig  `json:"monitor_config,omitempty" validate:"required_without=TimeWindow,excluded_with=TimeWindow"`
	Conditions     []types.Condition     `json:"conditions" validate:"required,min=1,max=10,dive"`
	ConditionLogic types.ConditionLogic  `json:"condition_logic" validate:"required,oneof=ANY ALL"`
	Channels       []types.Channel       `json:"channels" validate:"required,min=1,dive"`
	Preferences    *types.Preferences    `json:"preferences,omitempty"`
	TemplateSet    string                `json:"template_set,omitempty" validate:"omitempty,max=50"`
	Tags           []string              `json:"tags,omitempty" validate:"max=10,dive,max=50"`
	Metadata       map[string]any        `json:"metadata,omitempty"`
}

// UpdateWatchPointRequest is the request body for PATCH /v1/watchpoints/{id}.
type UpdateWatchPointRequest struct {
	Name        *string              `json:"name,omitempty" validate:"omitempty,max=200"`
	Conditions  *[]types.Condition   `json:"conditions,omitempty" validate:"omitempty,min=1,max=10,dive"`
	Channels    *[]types.Channel     `json:"channels,omitempty" validate:"omitempty,min=1,dive"`
	Status      *types.Status        `json:"status,omitempty" validate:"omitempty,oneof=active paused archived"`
	Preferences *types.Preferences   `json:"preferences,omitempty"`
}

// WatchPointDetail aggregates config, runtime state, and forecast snapshot.
// Per 05b-api-watchpoints.md Section 4.1.
type WatchPointDetail struct {
	*types.WatchPoint
	EvaluationState *types.EvaluationState `json:"evaluation_state,omitempty"`
	CurrentForecast *types.ForecastSnapshot `json:"current_forecast,omitempty"`
}

// --- Bulk Request/Response Models ---
// Per 05b-api-watchpoints.md Section 4.2.

// BulkCreateResponse for imports (POST /bulk).
// Returns partial success: items that passed validation are created; items that
// failed are returned with per-item error details.
type BulkCreateResponse struct {
	Successes []BulkCreateSuccess `json:"successes"`
	Failures  []BulkCreateFailure `json:"failures"`
}

// BulkCreateSuccess represents a successfully created WatchPoint within a bulk import.
type BulkCreateSuccess struct {
	Index      int               `json:"index"`
	ID         string            `json:"id"`
	WatchPoint *types.WatchPoint `json:"watchpoint"`
}

// BulkCreateFailure represents a failed item within a bulk import, with per-item error details.
type BulkCreateFailure struct {
	Index   int             `json:"index"`
	Code    types.ErrorCode `json:"code"`
	Message string          `json:"message"`
}

// BulkResponse for status changes/deletes/tag updates.
type BulkResponse struct {
	SuccessCount int `json:"success_count"`
	FailureCount int `json:"failure_count"`
}

// BulkTagUpdateRequest for PATCH /bulk/tags.
type BulkTagUpdateRequest struct {
	Filter     types.BulkFilter `json:"filter" validate:"required"`
	AddTags    []string         `json:"add_tags,omitempty" validate:"dive,max=50"`
	RemoveTags []string         `json:"remove_tags,omitempty" validate:"dive,max=50"`
}

// maxBulkCreateSize is the maximum number of items in a bulk create request.
const maxBulkCreateSize = 100

// bulkValidationConcurrency bounds the number of parallel validation goroutines.
const bulkValidationConcurrency = 10

// --- Handler ---

// WatchPointHandler manages WatchPoint CRUD, lifecycle, and related operations.
// Per 05b-api-watchpoints.md Section 2.
type WatchPointHandler struct {
	wpRepo           WPRepo
	notifRepo        WPNotifRepo
	validator        *core.Validator
	logger           *slog.Logger
	forecastProvider  WPForecastProvider
	usageEnforcer    WPUsageEnforcer
	auditLogger      WPAuditLogger
	evalTrigger      WPEvalTrigger
}

// NewWatchPointHandler creates a new WatchPointHandler with the provided dependencies.
// Per 05b-api-watchpoints.md Section 2.
func NewWatchPointHandler(
	wpRepo WPRepo,
	notifRepo WPNotifRepo,
	v *core.Validator,
	l *slog.Logger,
	forecastProvider WPForecastProvider,
	usageEnforcer WPUsageEnforcer,
	auditLogger WPAuditLogger,
	evalTrigger WPEvalTrigger,
) *WatchPointHandler {
	if l == nil {
		l = slog.Default()
	}
	return &WatchPointHandler{
		wpRepo:          wpRepo,
		notifRepo:       notifRepo,
		validator:       v,
		logger:          l,
		forecastProvider: forecastProvider,
		usageEnforcer:   usageEnforcer,
		auditLogger:     auditLogger,
		evalTrigger:     evalTrigger,
	}
}

// RegisterRoutes mounts WatchPoint routes on the provided chi.Router.
// Per 05b-api-watchpoints.md Section 5.
func (h *WatchPointHandler) RegisterRoutes(r chi.Router) {
	r.Route("/watchpoints", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)

		// Bulk operations (must be registered before /{id} to avoid route conflicts).
		r.Post("/bulk", h.BulkCreate)
		r.Patch("/bulk/tags", h.BulkTagUpdate)

		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.Get)
			r.Patch("/", h.Update)
			r.Delete("/", h.Delete)
			r.Post("/pause", h.Pause)
			r.Post("/resume", h.Resume)
			r.Get("/notifications", h.GetNotificationHistory)
		})
	})
}

// --- Handler Methods ---

// Create handles POST /v1/watchpoints.
//
// Per WPLC-001 and WPLC-002 flow simulations and 05b-api-watchpoints.md Section 6.1:
//  1. Decode and validate request (polymorphic mode check).
//  2. Validate conditions against types.StandardVariables.
//  3. Enforce limits via UsageEnforcer.CheckLimit.
//  4. Apply defaults (Status="active", TemplateSet="default").
//  5. Inject Actor.Source (VERT-002).
//  6. Persist via Repo.Create.
//  7. Hydrate forecast snapshot (soft dependency - failure does not block create).
//  8. Emit audit event.
//  9. Return 201 Created with WatchPointDetail.
func (h *WatchPointHandler) Create(w http.ResponseWriter, r *http.Request) {
	actor, ok := types.GetActor(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Authentication required",
			nil,
		))
		return
	}

	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	var req CreateWatchPointRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 2: Validate conditions against StandardVariables.
	if err := h.validateConditions(req.Conditions); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 2b: Validate time window if present.
	if req.TimeWindow != nil {
		if err := types.ValidateTimeWindow(req.TimeWindow); err != nil {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationTimeWindow,
				err.Error(),
				nil,
			))
			return
		}
	}

	// Step 3: Enforce limits.
	if h.usageEnforcer != nil {
		if err := h.usageEnforcer.CheckLimit(r.Context(), orgID, types.ResourceWatchPoints, 1); err != nil {
			core.Error(w, r, err)
			return
		}
	}

	// Step 4: Apply defaults.
	now := time.Now().UTC()
	wpID := "wp_" + uuid.New().String()

	// Step 5: Source injection (VERT-002).
	source := actor.Source
	if source == "" {
		source = "default"
	}

	// Default template set.
	templateSet := req.TemplateSet
	if templateSet == "" {
		templateSet = "default"
	}

	// Generate stable IDs for each channel.
	channels := make(types.ChannelList, len(req.Channels))
	for i, ch := range req.Channels {
		ch.ID = uuid.New().String()
		channels[i] = ch
	}

	wp := &types.WatchPoint{
		ID:             wpID,
		OrganizationID: orgID,
		Name:           req.Name,
		Location:       req.Location,
		Timezone:       req.Timezone,
		TimeWindow:     req.TimeWindow,
		MonitorConfig:  req.MonitorConfig,
		Conditions:     types.Conditions(req.Conditions),
		ConditionLogic: req.ConditionLogic,
		Channels:       channels,
		TemplateSet:    templateSet,
		NotificationPrefs: req.Preferences,
		Status:         types.StatusActive,
		TestMode:       actor.IsTestMode,
		Tags:           req.Tags,
		ConfigVersion:  1,
		Source:         source,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// Step 6: Persist.
	if err := h.wpRepo.Create(r.Context(), wp); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 7: Hydrate forecast (soft dependency).
	var forecast *types.ForecastSnapshot
	if h.forecastProvider != nil {
		snapshot, err := h.forecastProvider.GetSnapshot(r.Context(), req.Location.Lat, req.Location.Lon)
		if err != nil {
			h.logger.WarnContext(r.Context(), "forecast snapshot fetch failed during create (graceful degradation)",
				"watchpoint_id", wpID,
				"error", err,
			)
		} else {
			forecast = snapshot
		}
	}

	// Step 8: Audit.
	h.emitAuditEvent(r.Context(), actor, "watchpoint.created", wpID, "watchpoint")

	// Step 9: Return 201 Created.
	detail := WatchPointDetail{
		WatchPoint:      wp,
		CurrentForecast: forecast,
	}

	core.JSON(w, r, http.StatusCreated, core.APIResponse{Data: detail})
}

// Get handles GET /v1/watchpoints/{id}.
//
// Per INFO-008 flow and 05b-api-watchpoints.md Section 6.2:
//  1. Extract id from URL, orgID from context.
//  2. Fetch config via Repo.GetByID.
//  3. Fetch forecast via ForecastProvider.GetSnapshot (graceful failure).
//  4. Return 200 OK with WatchPointDetail.
func (h *WatchPointHandler) Get(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"WatchPoint ID is required",
			nil,
		))
		return
	}

	// Step 2: Fetch WatchPoint.
	wp, err := h.wpRepo.GetByID(r.Context(), id, orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 3: Fetch forecast (graceful failure).
	var forecast *types.ForecastSnapshot
	if h.forecastProvider != nil {
		snapshot, err := h.forecastProvider.GetSnapshot(r.Context(), wp.Location.Lat, wp.Location.Lon)
		if err != nil {
			h.logger.WarnContext(r.Context(), "forecast snapshot fetch failed for get (graceful degradation)",
				"watchpoint_id", id,
				"error", err,
			)
		} else {
			forecast = snapshot
		}
	}

	detail := WatchPointDetail{
		WatchPoint:      wp,
		CurrentForecast: forecast,
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: detail})
}

// Update handles PATCH /v1/watchpoints/{id}.
//
// Per WPLC-003 flow simulation and 05b-api-watchpoints.md Section 6.3:
//  1. Decode and validate (pointer fields allow partial updates).
//  2. Fetch current WatchPoint.
//  3. Apply partial updates (Source is immutable per VERT-003).
//  4. Persist via Repo.Update.
//  5. Emit audit event.
//  6. Return 200 OK.
func (h *WatchPointHandler) Update(w http.ResponseWriter, r *http.Request) {
	actor, _ := types.GetActor(r.Context())

	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"WatchPoint ID is required",
			nil,
		))
		return
	}

	var req UpdateWatchPointRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 2: Fetch current WatchPoint.
	wp, err := h.wpRepo.GetByID(r.Context(), id, orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 3: Apply partial updates. Source is immutable (VERT-003).
	if req.Name != nil {
		wp.Name = *req.Name
	}
	if req.Conditions != nil {
		// Validate new conditions against StandardVariables.
		if err := h.validateConditions(*req.Conditions); err != nil {
			core.Error(w, r, err)
			return
		}
		wp.Conditions = types.Conditions(*req.Conditions)
	}
	if req.Channels != nil {
		// Array handling: replacement, not merge (per spec).
		channels := make(types.ChannelList, len(*req.Channels))
		for i, ch := range *req.Channels {
			if ch.ID == "" {
				ch.ID = uuid.New().String()
			}
			channels[i] = ch
		}
		wp.Channels = channels
	}
	if req.Status != nil {
		wp.Status = *req.Status
	}
	if req.Preferences != nil {
		wp.NotificationPrefs = req.Preferences
	}

	// Step 4: Persist. Source is preserved (not modified).
	if err := h.wpRepo.Update(r.Context(), wp); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 5: Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "watchpoint.updated", id, "watchpoint")

	// Step 6: Return 200 OK.
	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: wp})
}

// Delete handles DELETE /v1/watchpoints/{id}.
//
// Per WPLC-008 flow simulation and 05b-api-watchpoints.md Section 6.4:
//  1. Extract id from URL, orgID from context.
//  2. Soft delete via Repo.Delete.
//  3. Emit audit event.
//  4. Return 204 No Content.
func (h *WatchPointHandler) Delete(w http.ResponseWriter, r *http.Request) {
	actor, _ := types.GetActor(r.Context())

	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"WatchPoint ID is required",
			nil,
		))
		return
	}

	// Step 2: Soft delete.
	if err := h.wpRepo.Delete(r.Context(), id, orgID); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 3: Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "watchpoint.deleted", id, "watchpoint")

	// Step 4: Return 204 No Content.
	w.WriteHeader(http.StatusNoContent)
}

// Pause handles POST /v1/watchpoints/{id}/pause.
//
// Per WPLC-005 flow simulation and 05b-api-watchpoints.md Section 6.4:
//  1. State check: verify current status is active.
//  2. Cancel deferred deliveries to prevent stale alerts after pause.
//  3. Update status to paused.
//  4. Emit audit event.
func (h *WatchPointHandler) Pause(w http.ResponseWriter, r *http.Request) {
	actor, _ := types.GetActor(r.Context())

	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"WatchPoint ID is required",
			nil,
		))
		return
	}

	// Step 1: Fetch and check current status.
	wp, err := h.wpRepo.GetByID(r.Context(), id, orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	if wp.Status == types.StatusPaused {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeConflictPaused,
			"WatchPoint is already paused",
			nil,
		))
		return
	}

	if wp.Status == types.StatusArchived {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeConflictPaused,
			"Cannot pause an archived WatchPoint",
			nil,
		))
		return
	}

	// Step 2: Cancel deferred deliveries (per spec Section 6.4 side effect).
	if h.notifRepo != nil {
		if err := h.notifRepo.CancelDeferredDeliveries(r.Context(), id); err != nil {
			h.logger.WarnContext(r.Context(), "failed to cancel deferred deliveries on pause",
				"watchpoint_id", id,
				"error", err,
			)
		}
	}

	// Step 3: Update status to paused.
	wp.Status = types.StatusPaused
	if err := h.wpRepo.Update(r.Context(), wp); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 4: Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "watchpoint.paused", id, "watchpoint")

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: wp})
}

// Resume handles POST /v1/watchpoints/{id}/resume.
//
// Per WPLC-006 flow simulation and 05b-api-watchpoints.md Section 6.4:
//  1. State check: verify current status is paused.
//  2. Cancel deferred deliveries to clear stale backlog.
//  3. Update status to active.
//  4. Trigger evaluation via EvalTrigger.
//  5. Emit audit event.
func (h *WatchPointHandler) Resume(w http.ResponseWriter, r *http.Request) {
	actor, _ := types.GetActor(r.Context())

	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"WatchPoint ID is required",
			nil,
		))
		return
	}

	// Step 1: Fetch and check current status.
	wp, err := h.wpRepo.GetByID(r.Context(), id, orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	if wp.Status == types.StatusActive {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeConflictActive,
			"WatchPoint is already active",
			nil,
		))
		return
	}

	if wp.Status == types.StatusArchived {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeConflictActive,
			"Cannot resume an archived WatchPoint",
			nil,
		))
		return
	}

	// Step 2: Cancel deferred deliveries to clear stale backlog.
	if h.notifRepo != nil {
		if err := h.notifRepo.CancelDeferredDeliveries(r.Context(), id); err != nil {
			h.logger.WarnContext(r.Context(), "failed to cancel deferred deliveries on resume",
				"watchpoint_id", id,
				"error", err,
			)
		}
	}

	// Step 3: Update status to active.
	wp.Status = types.StatusActive
	if err := h.wpRepo.Update(r.Context(), wp); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 4: Trigger evaluation (per WPLC-006).
	if h.evalTrigger != nil {
		if err := h.evalTrigger.TriggerEvaluation(r.Context(), id, "resume"); err != nil {
			h.logger.ErrorContext(r.Context(), "failed to trigger evaluation on resume",
				"watchpoint_id", id,
				"error", err,
			)
			// Do not fail the request; the evaluation will be picked up by the next batcher cycle.
		}
	}

	// Step 5: Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "watchpoint.resumed", id, "watchpoint")

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: wp})
}

// GetNotificationHistory handles GET /v1/watchpoints/{id}/notifications.
//
// Per INFO-001 flow simulation and 05b-api-watchpoints.md Section 6.11:
//  1. Extract id (WatchPoint ID) from URL, orgID from context.
//  2. Build NotificationFilter with WatchPointID set.
//  3. Query notifRepo.List.
//  4. Return 200 OK with paginated notification history.
func (h *WatchPointHandler) GetNotificationHistory(w http.ResponseWriter, r *http.Request) {
	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"WatchPoint ID is required",
			nil,
		))
		return
	}

	// Build filter.
	filter := types.NotificationFilter{
		OrganizationID: orgID,
		WatchPointID:   id,
	}

	// Parse pagination limit.
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

	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		filter.Pagination.NextCursor = cursor
	}

	if h.notifRepo == nil {
		core.JSON(w, r, http.StatusOK, core.APIResponse{
			Data: []*types.NotificationHistoryItem{},
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

// List handles GET /v1/watchpoints.
// Stub for future implementation per 05b-api-watchpoints.md Section 6.13.
// This is outside the current task scope but needed for route registration.
func (h *WatchPointHandler) List(w http.ResponseWriter, r *http.Request) {
	core.JSON(w, r, http.StatusOK, core.APIResponse{
		Data: []*types.WatchPoint{},
		Meta: &types.ResponseMeta{
			Pagination: &types.PageInfo{},
		},
	})
}

// BulkCreate handles POST /v1/watchpoints/bulk.
//
// Per WPLC-011 flow simulation and 05b-api-watchpoints.md Section 6.5:
//  1. Validate batch size (max 100).
//  2. Parallel validation using errgroup with bounded concurrency.
//  3. Check plan limits for current + valid batch size.
//  4. Persist valid items via Repo.CreateBatch.
//  5. Emit aggregated audit event.
//  6. Return 200 OK with partial success response (even if some items failed).
//
// Quiet Create Policy: Does NOT fetch forecast snapshots and does NOT trigger
// immediate evaluation. WatchPoints will be picked up by the next Batcher run.
func (h *WatchPointHandler) BulkCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := types.GetActor(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Authentication required",
			nil,
		))
		return
	}

	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	// Step 1: Decode and validate batch size.
	var items []CreateWatchPointRequest
	if err := core.DecodeJSON(w, r, &items); err != nil {
		core.Error(w, r, err)
		return
	}

	if len(items) == 0 {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"request body must contain at least one item",
			nil,
		))
		return
	}

	if len(items) > maxBulkCreateSize {
		core.Error(w, r, types.NewAppErrorWithDetails(
			types.ErrCodeValidationBatchSize,
			fmt.Sprintf("batch size %d exceeds maximum of %d", len(items), maxBulkCreateSize),
			nil,
			map[string]any{"max": maxBulkCreateSize, "received": len(items)},
		))
		return
	}

	// Step 2: Parallel validation using errgroup with bounded concurrency.
	// We collect per-item results into thread-safe slices.
	type validationResult struct {
		index int
		wp    *types.WatchPoint
		err   error
	}

	results := make([]validationResult, len(items))

	g, gCtx := errgroup.WithContext(r.Context())
	g.SetLimit(bulkValidationConcurrency)

	// Source injection (VERT-002).
	source := actor.Source
	if source == "" {
		source = "default"
	}

	now := time.Now().UTC()

	for i, item := range items {
		i, item := i, item // capture loop variables
		g.Go(func() error {
			_ = gCtx // available for context-aware validation

			// Struct validation.
			if err := h.validator.ValidateStruct(item); err != nil {
				results[i] = validationResult{index: i, err: err}
				return nil // Per-item error, not a group-level error
			}

			// Validate conditions against StandardVariables.
			if err := h.validateConditions(item.Conditions); err != nil {
				results[i] = validationResult{index: i, err: err}
				return nil
			}

			// Validate time window if present.
			if item.TimeWindow != nil {
				if err := types.ValidateTimeWindow(item.TimeWindow); err != nil {
					results[i] = validationResult{
						index: i,
						err: types.NewAppError(
							types.ErrCodeValidationTimeWindow,
							err.Error(),
							nil,
						),
					}
					return nil
				}
			}

			// Build the WatchPoint.
			wpID := "wp_" + uuid.New().String()

			templateSet := item.TemplateSet
			if templateSet == "" {
				templateSet = "default"
			}

			channels := make(types.ChannelList, len(item.Channels))
			for j, ch := range item.Channels {
				ch.ID = uuid.New().String()
				channels[j] = ch
			}

			wp := &types.WatchPoint{
				ID:             wpID,
				OrganizationID: orgID,
				Name:           item.Name,
				Location:       item.Location,
				Timezone:       item.Timezone,
				TimeWindow:     item.TimeWindow,
				MonitorConfig:  item.MonitorConfig,
				Conditions:     types.Conditions(item.Conditions),
				ConditionLogic: item.ConditionLogic,
				Channels:       channels,
				TemplateSet:    templateSet,
				NotificationPrefs: item.Preferences,
				Status:         types.StatusActive,
				TestMode:       actor.IsTestMode,
				Tags:           item.Tags,
				ConfigVersion:  1,
				Source:         source,
				CreatedAt:      now,
				UpdatedAt:      now,
			}

			results[i] = validationResult{index: i, wp: wp}
			return nil
		})
	}

	// Wait for all validation goroutines. Errors are stored per-item, not propagated.
	if err := g.Wait(); err != nil {
		// This should not happen since we return nil from all goroutines,
		// but handle defensively.
		core.Error(w, r, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"bulk validation encountered an unexpected error",
			err,
		))
		return
	}

	// Phase 1: Partition into valid and failed.
	var validWPs []*types.WatchPoint
	var validOrigIndices []int
	var failures []BulkCreateFailure

	for _, res := range results {
		if res.err != nil {
			code := types.ErrCodeValidationMissingField
			message := res.err.Error()

			// Extract error code from AppError if possible.
			if appErr, ok := res.err.(*types.AppError); ok {
				code = appErr.Code
				message = appErr.Message
			}

			failures = append(failures, BulkCreateFailure{
				Index:   res.index,
				Code:    code,
				Message: message,
			})
		} else if res.wp != nil {
			validWPs = append(validWPs, res.wp)
			validOrigIndices = append(validOrigIndices, res.index)
		}
	}

	// Step 3: Check plan limits for the valid items.
	if h.usageEnforcer != nil && len(validWPs) > 0 {
		if err := h.usageEnforcer.CheckLimit(r.Context(), orgID, types.ResourceWatchPoints, len(validWPs)); err != nil {
			core.Error(w, r, err)
			return
		}
	}

	// Step 4: Persist valid items.
	var successes []BulkCreateSuccess

	if len(validWPs) > 0 {
		createdIndices, failedIndices, err := h.wpRepo.CreateBatch(r.Context(), validWPs)
		if err != nil {
			// Systemic DB error: fail the entire request.
			core.Error(w, r, err)
			return
		}

		// Map created indices back to original request indices.
		for _, ci := range createdIndices {
			origIdx := validOrigIndices[ci]
			successes = append(successes, BulkCreateSuccess{
				Index:      origIdx,
				ID:         validWPs[ci].ID,
				WatchPoint: validWPs[ci],
			})
		}

		// Map DB-level failures back to original request indices.
		for fi, ferr := range failedIndices {
			origIdx := validOrigIndices[fi]
			code := types.ErrCodeInternalDB
			message := "database insertion failed"
			if appErr, ok := ferr.(*types.AppError); ok {
				code = appErr.Code
				message = appErr.Message
			}
			failures = append(failures, BulkCreateFailure{
				Index:   origIdx,
				Code:    code,
				Message: message,
			})
		}
	}

	// Step 5: Emit aggregated audit event.
	h.emitAuditEventWithMetadata(r.Context(), actor, types.AuditActionWatchPointBulkCreated, "", "watchpoint", map[string]any{
		"count":         len(successes),
		"failure_count": len(failures),
	})

	// Step 6: Return 200 OK with partial success response.
	resp := BulkCreateResponse{
		Successes: successes,
		Failures:  failures,
	}

	// Ensure non-nil slices for clean JSON output.
	if resp.Successes == nil {
		resp.Successes = []BulkCreateSuccess{}
	}
	if resp.Failures == nil {
		resp.Failures = []BulkCreateFailure{}
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: resp})
}

// BulkTagUpdate handles PATCH /v1/watchpoints/bulk/tags.
//
// Per BULK-005 flow simulation and 05b-api-watchpoints.md Section 6.10:
//  1. Validate request: at least one of add_tags or remove_tags must be provided.
//  2. Atomic update via Repo.UpdateTagsBatch (PostgreSQL array operators).
//  3. Emit aggregated audit event.
//  4. Return 200 OK with BulkResponse containing the count of updated WatchPoints.
func (h *WatchPointHandler) BulkTagUpdate(w http.ResponseWriter, r *http.Request) {
	actor, ok := types.GetActor(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Authentication required",
			nil,
		))
		return
	}

	orgID, ok := types.GetOrgID(r.Context())
	if !ok {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenMissing,
			"Organization context is required",
			nil,
		))
		return
	}

	// Step 1: Decode and validate.
	var req BulkTagUpdateRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	// At least one of add_tags or remove_tags must be provided.
	if len(req.AddTags) == 0 && len(req.RemoveTags) == 0 {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"at least one of add_tags or remove_tags must be provided",
			nil,
		))
		return
	}

	// Step 2: Atomic update via repository.
	count, err := h.wpRepo.UpdateTagsBatch(r.Context(), orgID, req.Filter, req.AddTags, req.RemoveTags)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 3: Emit aggregated audit event.
	h.emitAuditEventWithMetadata(r.Context(), actor, types.AuditActionWatchPointBulkTagUpdated, "", "watchpoint", map[string]any{
		"count":       count,
		"filter":      req.Filter,
		"add_tags":    req.AddTags,
		"remove_tags": req.RemoveTags,
	})

	// Step 4: Return 200 OK with BulkResponse.
	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: BulkResponse{
		SuccessCount: int(count),
		FailureCount: 0,
	}})
}

// --- Helper Functions ---

// validateConditions checks each condition's variable and threshold against
// types.StandardVariables, as required by the architecture spec.
func (h *WatchPointHandler) validateConditions(conditions []types.Condition) error {
	for _, cond := range conditions {
		// Verify variable exists in StandardVariables.
		meta, exists := types.StandardVariables[cond.Variable]
		if !exists {
			return types.NewAppErrorWithDetails(
				types.ErrCodeValidationInvalidVariable,
				"unsupported condition variable: "+cond.Variable,
				nil,
				map[string]any{"variable": cond.Variable},
			)
		}

		// Verify thresholds are within valid range.
		for _, threshold := range cond.Threshold {
			if threshold < meta.Range[0] || threshold > meta.Range[1] {
				return types.NewAppErrorWithDetails(
					types.ErrCodeValidationThresholdRange,
					"threshold out of valid range for variable "+cond.Variable,
					nil,
					map[string]any{
						"variable":  cond.Variable,
						"threshold": threshold,
						"min":       meta.Range[0],
						"max":       meta.Range[1],
					},
				)
			}
		}
	}
	return nil
}

// emitAuditEvent logs an audit event. Errors are logged but not propagated
// to avoid failing the primary operation due to audit log failures.
func (h *WatchPointHandler) emitAuditEvent(ctx context.Context, actor types.Actor, action, resourceID, resourceType string) {
	if h.auditLogger == nil {
		return
	}

	event := types.AuditEvent{
		Actor:        actor,
		Action:       action,
		ResourceID:   resourceID,
		ResourceType: resourceType,
		Timestamp:    time.Now().UTC(),
	}

	if err := h.auditLogger.Log(ctx, &event); err != nil {
		h.logger.WarnContext(ctx, "failed to log audit event",
			"action", action,
			"resource_id", resourceID,
			"error", err,
		)
	}
}

// emitAuditEventWithMetadata logs an audit event with additional metadata stored
// in the NewValue field as JSON. Used by bulk operations to include aggregated
// information (count, filter, etc.) per the architecture spec.
func (h *WatchPointHandler) emitAuditEventWithMetadata(ctx context.Context, actor types.Actor, action, resourceID, resourceType string, metadata map[string]any) {
	if h.auditLogger == nil {
		return
	}

	var newValue json.RawMessage
	if metadata != nil {
		data, err := json.Marshal(metadata)
		if err != nil {
			h.logger.WarnContext(ctx, "failed to marshal audit metadata",
				"action", action,
				"error", err,
			)
		} else {
			newValue = data
		}
	}

	event := types.AuditEvent{
		Actor:        actor,
		Action:       action,
		ResourceID:   resourceID,
		ResourceType: resourceType,
		NewValue:     newValue,
		Timestamp:    time.Now().UTC(),
	}

	if err := h.auditLogger.Log(ctx, &event); err != nil {
		h.logger.WarnContext(ctx, "failed to log audit event",
			"action", action,
			"resource_id", resourceID,
			"error", err,
		)
	}
}
