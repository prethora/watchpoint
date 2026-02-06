// Package handlers contains the HTTP handler implementations for the WatchPoint API.
//
// This file implements the API Key management handler as defined in
// 05d-api-organization.md Section 5. It covers:
//   - Listing keys (with prefix filter, masked secrets)
//   - Creating keys (secure generation, bcrypt hashing, one-time secret return)
//   - Rotating keys (dual-validity with 24h grace period)
//   - Revoking keys (soft delete via revoked_at timestamp)
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// --- Service Interfaces ---
//
// These interfaces are defined locally following the handler injection pattern
// established in auth.go and billing.go. This avoids coupling to concrete types
// and enables test mocking.

// APIKeyRepo defines the data access contract for API key operations.
// Per 05d-api-organization.md Section 2.2.
type APIKeyRepo interface {
	List(ctx context.Context, orgID string, params APIKeyListParams) ([]*types.APIKey, error)
	Create(ctx context.Context, key *types.APIKey) error
	GetByID(ctx context.Context, id string, orgID string) (*types.APIKey, error)
	Delete(ctx context.Context, id string, orgID string) error
	Rotate(ctx context.Context, oldKeyID string, orgID string, newKey *types.APIKey, graceEnd time.Time) error
	CountRecentByUser(ctx context.Context, userID string, since time.Time) (int, error)
}

// APIKeyListParams mirrors the db.ListAPIKeysParams type to avoid importing db
// package directly into the handler layer.
type APIKeyListParams struct {
	ActiveOnly bool
	Prefix     string
	Limit      int
	Cursor     string
}

// APIKeyAuditLogger defines the audit logging contract for API key operations.
type APIKeyAuditLogger interface {
	Log(ctx context.Context, event types.AuditEvent) error
}

// --- Request/Response Models ---
// Per 05d-api-organization.md Section 5.2.

// CreateAPIKeyRequest is the request body for POST /v1/api-keys.
type CreateAPIKeyRequest struct {
	Name          string   `json:"name" validate:"required,max=100"`
	Scopes        []string `json:"scopes" validate:"required,min=1"`
	ExpiresInDays int      `json:"expires_in_days" validate:"min=0,max=365"`
	Source        string   `json:"source,omitempty" validate:"omitempty,max=50,alphanum"`
}

// APIKeySecretResponse is the one-time response returned when creating or
// rotating an API key. It contains the plaintext secret that is never stored
// and cannot be retrieved again.
// Per 05d-api-organization.md Section 5.2.
type APIKeySecretResponse struct {
	APIKeyResponse
	NewKey                string     `json:"new_key"`
	NewKeyPrefix          string     `json:"new_key_prefix"`
	PreviousKeyValidUntil *time.Time `json:"previous_key_valid_until,omitempty"`
}

// APIKeyResponse is the safe response for List/Get operations.
// It never includes the full secret, only the prefix.
// Per 05d-api-organization.md Section 5.2.
type APIKeyResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	Scopes     []string   `json:"scopes"`
	Source     string     `json:"source,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// --- Constants ---

const (
	// apiKeySecretLength is the number of random bytes used for API key secrets.
	apiKeySecretLength = 32

	// apiKeyPrefixLength is the number of characters from the secret used as the
	// visible prefix (after the sk_live_/sk_test_ prefix).
	apiKeyPrefixLength = 8

	// apiKeyBcryptCost is the bcrypt cost factor for hashing API key secrets.
	// Per USER-011 flow simulation step 4.
	apiKeyBcryptCost = 12

	// apiKeyGracePeriod is the dual-validity window for rotated keys.
	// Per USER-012 flow simulation: old key remains valid for 24 hours.
	apiKeyGracePeriod = 24 * time.Hour

	// apiKeyRateLimitWindow is the time window for proactive key creation rate limiting.
	apiKeyRateLimitWindow = 1 * time.Hour

	// apiKeyRateLimitMax is the maximum number of keys a user can create per window.
	// Per 05d-api-organization.md Section 5.4 item 3.
	apiKeyRateLimitMax = 5

	// livePrefixTag and testPrefixTag define the prefix tags for API keys.
	livePrefixTag = "sk_live_"
	testPrefixTag = "sk_test_"
)

// --- Handler ---

// APIKeyHandler manages programmatic access credentials.
// Per 05d-api-organization.md Section 5.1.
type APIKeyHandler struct {
	repo   APIKeyRepo
	audit  APIKeyAuditLogger
	logger *slog.Logger
}

// NewAPIKeyHandler creates a new APIKeyHandler with the provided dependencies.
func NewAPIKeyHandler(
	repo APIKeyRepo,
	audit APIKeyAuditLogger,
	l *slog.Logger,
) *APIKeyHandler {
	if l == nil {
		l = slog.Default()
	}
	return &APIKeyHandler{
		repo:   repo,
		audit:  audit,
		logger: l,
	}
}

// RegisterRoutes mounts API key routes onto the provided router.
// Per 05d-api-organization.md Section 6: all key ops require Admin+.
// The caller is responsible for applying RequireRole(RoleAdmin) middleware.
func (h *APIKeyHandler) RegisterRoutes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Post("/{id}/rotate", h.Rotate)
	r.Delete("/{id}", h.Revoke)
}

// --- Handler Methods ---

// List handles GET /v1/api-keys.
// Returns API keys for the organization, never exposing full secrets.
// Per 05d-api-organization.md Section 5.4 item 7: List endpoint never
// returns the full secret, only the prefix.
func (h *APIKeyHandler) List(w http.ResponseWriter, r *http.Request) {
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
	params := APIKeyListParams{
		Limit: 20,
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

	if prefix := r.URL.Query().Get("prefix"); prefix != "" {
		params.Prefix = prefix
	}

	if r.URL.Query().Get("active") == "true" {
		params.ActiveOnly = true
	}

	keys, err := h.repo.List(r.Context(), orgID, params)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Build pagination info. The repo returns up to limit+1 rows; if we got
	// more than limit, there are additional pages.
	pageInfo := types.PageInfo{}
	if len(keys) > params.Limit {
		pageInfo.HasMore = true
		keys = keys[:params.Limit] // Trim the extra row.
	}

	// Convert to safe response models (no key_hash exposed).
	data := make([]APIKeyResponse, 0, len(keys))
	for _, k := range keys {
		data = append(data, toAPIKeyResponse(k))
	}

	// Set the cursor to the created_at of the last item returned.
	if pageInfo.HasMore && len(data) > 0 {
		pageInfo.NextCursor = data[len(data)-1].CreatedAt.Format(time.RFC3339Nano)
	}

	resp := core.APIResponse{
		Data: data,
		Meta: &types.ResponseMeta{
			Pagination: &pageInfo,
		},
	}

	core.JSON(w, r, http.StatusOK, resp)
}

// Create handles POST /v1/api-keys.
//
// Per USER-011 flow simulation:
//  1. Decode and validate request.
//  2. Validate scopes against types.AllScopes.
//  3. Check proactive rate limit (max 5 keys per user per hour).
//  4. Generate cryptographically secure secret.
//  5. Compute bcrypt hash.
//  6. Save hash to DB (plaintext never stored).
//  7. Return plaintext in response (one time only).
func (h *APIKeyHandler) Create(w http.ResponseWriter, r *http.Request) {
	// Extract actor and org context.
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

	// Step 0: Actor validation - only human users can create API keys.
	// Per 05d-api-organization.md Section 5.4 item 1.
	if actor.Type != types.ActorTypeUser {
		core.Error(w, r, types.NewAppError(
			types.ErrCodePermissionRole,
			"Only human users can create API keys. API keys cannot create other keys.",
			nil,
		))
		return
	}

	// Step 1: Decode request.
	var req CreateAPIKeyRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 2: Validate scopes against the allowed set.
	if err := validateScopes(req.Scopes); err != nil {
		core.Error(w, r, err)
		return
	}

	// Validate name is not empty (also caught by struct validation but belt-and-suspenders).
	if req.Name == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"name is required",
			nil,
		))
		return
	}

	// Validate source field: only Admin/Owner can set Source.
	// Per 05d-api-organization.md Section 5.4 item 4.
	if req.Source != "" && !actor.RoleHasAtLeast(types.RoleAdmin) {
		core.Error(w, r, types.NewAppError(
			types.ErrCodePermissionRole,
			"Only Admin or Owner roles can provision source-tagged API keys",
			nil,
		))
		return
	}

	// Step 3: Proactive rate limit check.
	// Per 05d-api-organization.md Section 5.4 item 3.
	since := time.Now().UTC().Add(-apiKeyRateLimitWindow)
	count, err := h.repo.CountRecentByUser(r.Context(), actor.ID, since)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "failed to check API key rate limit",
			"user_id", actor.ID,
			"error", err,
		)
		core.Error(w, r, err)
		return
	}
	if count >= apiKeyRateLimitMax {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeRateLimit,
			"Too many API keys created recently. Try again later.",
			nil,
		))
		return
	}

	// Step 4-5: Generate secret and compute hash.
	isTestMode := actor.IsTestMode
	plaintext, keyPrefix, hash, err := generateAPIKeySecret(isTestMode)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "failed to generate API key secret",
			"error", err,
		)
		core.Error(w, r, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to generate API key",
			err,
		))
		return
	}

	// Compute expiry time if ExpiresInDays is set.
	var expiresAt *time.Time
	if req.ExpiresInDays > 0 {
		t := time.Now().UTC().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		expiresAt = &t
	}

	// Build the APIKey record. Plaintext is NOT included.
	keyID := "key_" + uuid.New().String()
	userID := actor.ID
	apiKey := &types.APIKey{
		ID:              keyID,
		OrganizationID:  orgID,
		CreatedByUserID: &userID,
		KeyHash:         hash,
		KeyPrefix:       keyPrefix,
		Scopes:          req.Scopes,
		TestMode:        isTestMode,
		Source:          req.Source,
		Name:            req.Name,
		ExpiresAt:       expiresAt,
		CreatedAt:       time.Now().UTC(),
	}

	// Step 6: Persist to DB (hash only).
	if err := h.repo.Create(r.Context(), apiKey); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 7: Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "apikey.created", keyID, "api_key")

	// Step 8: Return one-time response with plaintext.
	resp := APIKeySecretResponse{
		APIKeyResponse: toAPIKeyResponse(apiKey),
		NewKey:         plaintext,
		NewKeyPrefix:   keyPrefix,
	}

	core.JSON(w, r, http.StatusCreated, core.APIResponse{Data: resp})
}

// Rotate handles POST /v1/api-keys/{id}/rotate.
//
// Per USER-012 flow simulation:
//  1. Verify old key exists and is active.
//  2. Generate new secret and hash.
//  3. Set old key to expire in 24 hours (grace period).
//  4. Insert new key record.
//  5. Return new plaintext and old key expiry.
func (h *APIKeyHandler) Rotate(w http.ResponseWriter, r *http.Request) {
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

	// Only human users can rotate keys.
	if actor.Type != types.ActorTypeUser {
		core.Error(w, r, types.NewAppError(
			types.ErrCodePermissionRole,
			"Only human users can rotate API keys",
			nil,
		))
		return
	}

	keyID := chi.URLParam(r, "id")
	if keyID == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"API key ID is required",
			nil,
		))
		return
	}

	// Step 1: Verify old key exists and is active.
	oldKey, err := h.repo.GetByID(r.Context(), keyID, orgID)
	if err != nil {
		core.Error(w, r, err)
		return
	}

	// Check the old key is not already revoked.
	if oldKey.RevokedAt != nil {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeNotFoundAPIKey,
			"API key is already revoked",
			nil,
		))
		return
	}

	// Step 2: Generate new secret.
	plaintext, keyPrefix, hash, err := generateAPIKeySecret(oldKey.TestMode)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "failed to generate API key secret for rotation",
			"error", err,
		)
		core.Error(w, r, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to generate API key",
			err,
		))
		return
	}

	// Step 3-4: Build new key record, inheriting metadata from old key.
	graceEnd := time.Now().UTC().Add(apiKeyGracePeriod)
	newKeyID := "key_" + uuid.New().String()
	userID := actor.ID
	newKey := &types.APIKey{
		ID:              newKeyID,
		OrganizationID:  orgID,
		CreatedByUserID: &userID,
		KeyHash:         hash,
		KeyPrefix:       keyPrefix,
		Scopes:          oldKey.Scopes,
		TestMode:        oldKey.TestMode,
		Source:          oldKey.Source,
		Name:            oldKey.Name,
		ExpiresAt:       oldKey.ExpiresAt, // Inherit original expiry for new key
		CreatedAt:       time.Now().UTC(),
	}

	// Execute the rotation: update old key expiry and insert new key.
	if err := h.repo.Rotate(r.Context(), keyID, orgID, newKey, graceEnd); err != nil {
		core.Error(w, r, err)
		return
	}

	// Step 5: Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "apikey.rotated", keyID, "api_key")

	// Step 6: Return response with new plaintext and grace period.
	resp := APIKeySecretResponse{
		APIKeyResponse:        toAPIKeyResponse(newKey),
		NewKey:                plaintext,
		NewKeyPrefix:          keyPrefix,
		PreviousKeyValidUntil: &graceEnd,
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: resp})
}

// Revoke handles DELETE /v1/api-keys/{id}.
//
// Per USER-013 flow simulation:
//  1. Soft-revoke the key (set revoked_at = NOW()).
//  2. Emit audit event.
//  3. Return 204 No Content.
func (h *APIKeyHandler) Revoke(w http.ResponseWriter, r *http.Request) {
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

	keyID := chi.URLParam(r, "id")
	if keyID == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"API key ID is required",
			nil,
		))
		return
	}

	// Revoke the key.
	if err := h.repo.Delete(r.Context(), keyID, orgID); err != nil {
		core.Error(w, r, err)
		return
	}

	// Emit audit event.
	h.emitAuditEvent(r.Context(), actor, "apikey.revoked", keyID, "api_key")

	// Return 204 No Content.
	w.WriteHeader(http.StatusNoContent)
}

// --- Helper Functions ---

// generateAPIKeySecret generates a cryptographically secure API key.
// Returns the plaintext secret, the visible prefix, and the bcrypt hash.
// The prefix format is "sk_live_" or "sk_test_" followed by the first
// apiKeyPrefixLength characters of the base64-encoded random bytes.
//
// Per USER-011 flow simulation step 4: bcrypt.GenerateFromPassword(plaintext_secret, 12).
// The plaintext key is kept under 72 bytes (bcrypt's input limit) by design:
// 8-byte prefix + 43-byte base64 = 51 bytes total, well within the limit.
func generateAPIKeySecret(testMode bool) (plaintext, prefix, hash string, err error) {
	// Generate random bytes.
	randomBytes := make([]byte, apiKeySecretLength)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", "", "", fmt.Errorf("crypto/rand read failed: %w", err)
	}

	// Encode to URL-safe base64 (no padding).
	encoded := base64.RawURLEncoding.EncodeToString(randomBytes)

	// Construct the full plaintext key.
	tag := livePrefixTag
	if testMode {
		tag = testPrefixTag
	}
	plaintext = tag + encoded

	// The prefix is the visible portion: tag + first N chars.
	prefixLen := apiKeyPrefixLength
	if len(encoded) < prefixLen {
		prefixLen = len(encoded)
	}
	prefix = tag + encoded[:prefixLen]

	// Hash with bcrypt directly, per architecture spec.
	// The plaintext is guaranteed to be under 72 bytes (bcrypt limit)
	// because: len(tag)=8 + len(base64(32 bytes))=43 = 51 bytes.
	hashBytes, err := bcrypt.GenerateFromPassword([]byte(plaintext), apiKeyBcryptCost)
	if err != nil {
		return "", "", "", fmt.Errorf("bcrypt hash failed: %w", err)
	}
	hash = string(hashBytes)

	return plaintext, prefix, hash, nil
}

// validateScopes checks that all requested scopes exist in types.AllScopes.
// Per 05d-api-organization.md Section 5.4 item 2.
func validateScopes(scopes []string) error {
	if len(scopes) == 0 {
		return types.NewAppError(
			types.ErrCodeValidationMissingField,
			"at least one scope is required",
			nil,
		)
	}

	allowed := make(map[string]bool, len(types.AllScopes))
	for _, s := range types.AllScopes {
		allowed[s] = true
	}

	var invalid []string
	for _, s := range scopes {
		if !allowed[s] {
			invalid = append(invalid, s)
		}
	}

	if len(invalid) > 0 {
		return types.NewAppErrorWithDetails(
			types.ErrCodeValidationMissingField,
			"invalid scopes",
			nil,
			map[string]any{
				"invalid_scopes": invalid,
				"valid_scopes":   types.AllScopes,
			},
		)
	}

	return nil
}

// toAPIKeyResponse converts a types.APIKey to the safe APIKeyResponse DTO.
// The key_hash is intentionally omitted to prevent leakage.
func toAPIKeyResponse(k *types.APIKey) APIKeyResponse {
	return APIKeyResponse{
		ID:         k.ID,
		Name:       k.Name,
		KeyPrefix:  k.KeyPrefix,
		Scopes:     k.Scopes,
		Source:     k.Source,
		LastUsedAt: k.LastUsedAt,
		ExpiresAt:  k.ExpiresAt,
		CreatedAt:  k.CreatedAt,
	}
}

// emitAuditEvent logs an audit event. Errors are logged but not propagated
// to avoid failing the primary operation due to audit log failures.
func (h *APIKeyHandler) emitAuditEvent(ctx context.Context, actor types.Actor, action, resourceID, resourceType string) {
	if h.audit == nil {
		return
	}

	event := types.AuditEvent{
		Actor:        actor,
		Action:       action,
		ResourceID:   resourceID,
		ResourceType: resourceType,
		Timestamp:    time.Now().UTC(),
	}

	if err := h.audit.Log(ctx, event); err != nil {
		h.logger.WarnContext(ctx, "failed to log audit event",
			"action", action,
			"resource_id", resourceID,
			"error", err,
		)
	}
}
