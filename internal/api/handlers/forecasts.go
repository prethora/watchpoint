// Package handlers contains the HTTP handler implementations for the WatchPoint API.
//
// This file implements the Forecast handler as defined in architecture/05c-api-forecasts.md
// Section 5. It covers:
//   - Point forecast retrieval (GET /v1/forecasts/point)
//   - Batch forecast retrieval (POST /v1/forecasts/points)
//   - Variable listing (GET /v1/forecasts/variables)
//   - Pipeline status (GET /v1/forecasts/status)
//   - Verification metrics (GET /v1/forecasts/verification)
package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"watchpoint/internal/core"
	"watchpoint/internal/forecasts"
	"watchpoint/internal/types"
)

// ForecastServiceInterface defines the service contract for the forecast handler.
// Matches the ForecastService interface from the forecasts package but is defined
// locally to avoid tight coupling per the handler injection pattern.
type ForecastServiceInterface interface {
	GetPointForecast(ctx context.Context, lat, lon float64, start, end time.Time) (*forecasts.ForecastResponse, error)
	GetBatchForecast(ctx context.Context, req forecasts.BatchForecastRequest) (*forecasts.BatchForecastResult, error)
	GetVariables(ctx context.Context) ([]forecasts.VariableResponseMetadata, error)
	GetStatus(ctx context.Context) (*forecasts.SystemStatus, error)
	GetSnapshot(ctx context.Context, lat, lon float64) (*types.ForecastSnapshot, error)
	GetVerificationMetrics(ctx context.Context, model string, start, end time.Time) (*forecasts.VerificationReport, error)
}

// ForecastHandler maps HTTP requests to ForecastService methods.
// Per architecture/05c-api-forecasts.md Section 5.
type ForecastHandler struct {
	service   ForecastServiceInterface
	validator *core.Validator
	logger    *slog.Logger
}

// NewForecastHandler creates a new ForecastHandler with the provided dependencies.
func NewForecastHandler(
	svc ForecastServiceInterface,
	val *core.Validator,
	logger *slog.Logger,
) *ForecastHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ForecastHandler{
		service:   svc,
		validator: val,
		logger:    logger,
	}
}

// RegisterRoutes mounts the forecast endpoints onto the mux.
// Per architecture/05c-api-forecasts.md Section 5.
// All routes assume Authentication Middleware is already applied.
func (h *ForecastHandler) RegisterRoutes(r chi.Router) {
	r.Get("/point", h.HandleGetPoint)
	r.Post("/points", h.HandleGetBatch)
	r.Get("/variables", h.HandleListVars)
	r.Get("/status", h.HandleGetStatus)
	r.Get("/verification", h.HandleGetVerification)
}

// HandleGetPoint handles GET /v1/forecasts/point.
// Per FQRY-001 flow simulation:
//  1. Parse query params: lat, lon, start, end.
//  2. Call ForecastService.GetPointForecast.
//  3. Return JSON response with cache headers.
func (h *ForecastHandler) HandleGetPoint(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Parse latitude.
	latStr := q.Get("lat")
	if latStr == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"lat query parameter is required",
			nil,
		))
		return
	}
	lat, err := strconv.ParseFloat(latStr, 64)
	if err != nil {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationInvalidLat,
			"lat must be a valid number",
			nil,
		))
		return
	}

	// Parse longitude.
	lonStr := q.Get("lon")
	if lonStr == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"lon query parameter is required",
			nil,
		))
		return
	}
	lon, err := strconv.ParseFloat(lonStr, 64)
	if err != nil {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationInvalidLon,
			"lon must be a valid number",
			nil,
		))
		return
	}

	// Parse time range (optional, default to now and +24h).
	now := time.Now().UTC()
	start := now
	end := now.Add(24 * time.Hour)

	if startStr := q.Get("start"); startStr != "" {
		parsed, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"start must be a valid RFC3339 timestamp",
				nil,
			))
			return
		}
		start = parsed.UTC()
	}

	if endStr := q.Get("end"); endStr != "" {
		parsed, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"end must be a valid RFC3339 timestamp",
				nil,
			))
			return
		}
		end = parsed.UTC()
	}

	// Call the service.
	result, err := h.service.GetPointForecast(r.Context(), lat, lon, start, end)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Set cache headers per architecture/05c-api-forecasts.md Section 6.3.
	w.Header().Set("Cache-Control", "private, max-age=300")
	if result.GeneratedAt.After(time.Time{}) {
		w.Header().Set("Last-Modified", result.GeneratedAt.UTC().Format(http.TimeFormat))
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: result})
}

// HandleGetBatch handles POST /v1/forecasts/points.
// Per FQRY-002 flow simulation:
//  1. Validate batch size <= 50.
//  2. Call ForecastService.GetBatchForecast.
//  3. Return results.
func (h *ForecastHandler) HandleGetBatch(w http.ResponseWriter, r *http.Request) {
	var req forecasts.BatchForecastRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Validate the request.
	if len(req.Locations) > forecasts.MaxBatchLocations {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationBatchSize,
			"batch size exceeds maximum of 50 locations",
			nil,
		))
		return
	}

	// Default time range if not specified.
	now := time.Now().UTC()
	if req.Start.IsZero() {
		req.Start = now
	}
	if req.End.IsZero() {
		req.End = now.Add(24 * time.Hour)
	}

	result, err := h.service.GetBatchForecast(r.Context(), req)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Set cache headers.
	w.Header().Set("Cache-Control", "private, max-age=300")

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: result})
}

// HandleListVars handles GET /v1/forecasts/variables.
// Per FQRY-003 flow simulation: returns static variable metadata.
func (h *ForecastHandler) HandleListVars(w http.ResponseWriter, r *http.Request) {
	vars, err := h.service.GetVariables(r.Context())
	if err != nil {
		core.Error(w, r, err)
		return
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: vars})
}

// HandleGetStatus handles GET /v1/forecasts/status.
// Per FQRY-004 flow simulation: returns pipeline health status.
func (h *ForecastHandler) HandleGetStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.service.GetStatus(r.Context())
	if err != nil {
		core.Error(w, r, err)
		return
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: status})
}

// HandleGetVerification handles GET /v1/forecasts/verification.
// Per INFO-007 flow. Returns aggregated verification metrics.
// Query params: model (required), start (optional), end (optional).
// Defaults to last 7 days if time range not specified.
func (h *ForecastHandler) HandleGetVerification(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	model := q.Get("model")
	if model == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"model query parameter is required",
			nil,
		))
		return
	}

	// Default to last 7 days if time range not specified.
	now := time.Now().UTC()
	end := now
	start := now.Add(-7 * 24 * time.Hour)

	if startStr := q.Get("start"); startStr != "" {
		parsed, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"start must be a valid RFC3339 timestamp",
				nil,
			))
			return
		}
		start = parsed.UTC()
	}

	if endStr := q.Get("end"); endStr != "" {
		parsed, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			core.Error(w, r, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"end must be a valid RFC3339 timestamp",
				nil,
			))
			return
		}
		end = parsed.UTC()
	}

	report, err := h.service.GetVerificationMetrics(r.Context(), model, start, end)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: report})
}
