package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// =============================================================================
// Mock Implementations for API Key Handler
// =============================================================================

// mockAPIKeyRepo implements APIKeyRepo for testing.
type mockAPIKeyRepo struct {
	listFn              func(ctx context.Context, orgID string, params APIKeyListParams) ([]*types.APIKey, error)
	createFn            func(ctx context.Context, key *types.APIKey) error
	getByIDFn           func(ctx context.Context, id, orgID string) (*types.APIKey, error)
	deleteFn            func(ctx context.Context, id, orgID string) error
	rotateFn            func(ctx context.Context, oldKeyID, orgID string, newKey *types.APIKey, graceEnd time.Time) error
	countRecentByUserFn func(ctx context.Context, userID string, since time.Time) (int, error)

	// capturedCreateKey stores the key passed to Create for inspection.
	capturedCreateKey *types.APIKey
}

func (m *mockAPIKeyRepo) List(ctx context.Context, orgID string, params APIKeyListParams) ([]*types.APIKey, error) {
	if m.listFn != nil {
		return m.listFn(ctx, orgID, params)
	}
	return nil, nil
}

func (m *mockAPIKeyRepo) Create(ctx context.Context, key *types.APIKey) error {
	m.capturedCreateKey = key
	if m.createFn != nil {
		return m.createFn(ctx, key)
	}
	return nil
}

func (m *mockAPIKeyRepo) GetByID(ctx context.Context, id, orgID string) (*types.APIKey, error) {
	if m.getByIDFn != nil {
		return m.getByIDFn(ctx, id, orgID)
	}
	return nil, types.NewAppError(types.ErrCodeNotFoundAPIKey, "not found", nil)
}

func (m *mockAPIKeyRepo) Delete(ctx context.Context, id, orgID string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id, orgID)
	}
	return nil
}

func (m *mockAPIKeyRepo) Rotate(ctx context.Context, oldKeyID, orgID string, newKey *types.APIKey, graceEnd time.Time) error {
	m.capturedCreateKey = newKey
	if m.rotateFn != nil {
		return m.rotateFn(ctx, oldKeyID, orgID, newKey, graceEnd)
	}
	return nil
}

func (m *mockAPIKeyRepo) CountRecentByUser(ctx context.Context, userID string, since time.Time) (int, error) {
	if m.countRecentByUserFn != nil {
		return m.countRecentByUserFn(ctx, userID, since)
	}
	return 0, nil
}

// mockAPIKeyAudit implements APIKeyAuditLogger for testing.
type mockAPIKeyAudit struct {
	events []types.AuditEvent
}

func (m *mockAPIKeyAudit) Log(ctx context.Context, event types.AuditEvent) error {
	m.events = append(m.events, event)
	return nil
}

// =============================================================================
// Test Helpers
// =============================================================================

// newTestAPIKeyHandler creates a handler with default mocks for testing.
func newTestAPIKeyHandler(repo *mockAPIKeyRepo) (*APIKeyHandler, *mockAPIKeyAudit) {
	audit := &mockAPIKeyAudit{}
	handler := NewAPIKeyHandler(repo, audit, slog.Default())
	return handler, audit
}

// ctxWithActor returns a context with a test Actor set.
func ctxWithActor(actorType types.ActorType, role types.UserRole, orgID string) context.Context {
	actor := types.Actor{
		ID:             "user_test1",
		Type:           actorType,
		OrganizationID: orgID,
		Role:           role,
		Scopes:         types.RoleScopeMap[role],
	}
	return types.WithActor(context.Background(), actor)
}

// makeCreateRequest creates a JSON request body for the Create endpoint.
func makeCreateRequest(t *testing.T, req CreateAPIKeyRequest) *bytes.Buffer {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	return bytes.NewBuffer(body)
}

// =============================================================================
// Create Tests
// =============================================================================

func TestAPIKeyHandler_Create_Success(t *testing.T) {
	repo := &mockAPIKeyRepo{}
	handler, audit := newTestAPIKeyHandler(repo)

	reqBody := makeCreateRequest(t, CreateAPIKeyRequest{
		Name:          "Production Key",
		Scopes:        []string{"watchpoints:read", "forecasts:read"},
		ExpiresInDays: 30,
	})

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys", reqBody)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Create(w, r)

	// Verify 201 Created.
	assert.Equal(t, http.StatusCreated, w.Code)

	// Parse response.
	var resp core.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Access the data as a map to verify the response structure.
	dataBytes, _ := json.Marshal(resp.Data)
	var secretResp APIKeySecretResponse
	err = json.Unmarshal(dataBytes, &secretResp)
	require.NoError(t, err)

	// The response MUST contain the plaintext secret.
	assert.NotEmpty(t, secretResp.NewKey, "plaintext secret must be returned in response")
	assert.True(t, strings.HasPrefix(secretResp.NewKey, "sk_live_") || strings.HasPrefix(secretResp.NewKey, "sk_test_"),
		"secret must have proper prefix")

	// The response must include the prefix.
	assert.NotEmpty(t, secretResp.NewKeyPrefix, "key prefix must be returned")
	assert.True(t, strings.HasPrefix(secretResp.NewKey, secretResp.NewKeyPrefix),
		"plaintext must start with the returned prefix")

	// Verify the key ID was assigned.
	assert.True(t, strings.HasPrefix(secretResp.ID, "key_"), "ID must have key_ prefix")

	// Verify scopes are echoed back.
	assert.Equal(t, []string{"watchpoints:read", "forecasts:read"}, secretResp.Scopes)

	// CRITICAL: Verify plaintext was NEVER passed to the repository.
	require.NotNil(t, repo.capturedCreateKey, "repo.Create must have been called")
	assert.NotEmpty(t, repo.capturedCreateKey.KeyHash, "hash must be stored")
	assert.True(t, strings.HasPrefix(repo.capturedCreateKey.KeyHash, "$2a$"),
		"stored hash must be bcrypt format")

	// Verify the hash actually verifies against the plaintext.
	// Per USER-011 spec: bcrypt is applied directly to the plaintext.
	err = bcrypt.CompareHashAndPassword([]byte(repo.capturedCreateKey.KeyHash), []byte(secretResp.NewKey))
	assert.NoError(t, err, "bcrypt hash must verify against the plaintext secret")

	// Verify audit event was emitted.
	require.Len(t, audit.events, 1)
	assert.Equal(t, "apikey.created", audit.events[0].Action)
	assert.Equal(t, "api_key", audit.events[0].ResourceType)
}

func TestAPIKeyHandler_Create_PlaintextNeverStored(t *testing.T) {
	// This is the most critical test: verify the plaintext secret is
	// NEVER passed to the repository layer.
	repo := &mockAPIKeyRepo{
		createFn: func(ctx context.Context, key *types.APIKey) error {
			// The KeyHash must be a bcrypt hash, not plaintext.
			assert.True(t, strings.HasPrefix(key.KeyHash, "$2a$"),
				"KeyHash must be bcrypt format, not plaintext")

			// Verify the hash is NOT the same as any sk_live_ or sk_test_ prefixed string.
			assert.False(t, strings.HasPrefix(key.KeyHash, "sk_live_"),
				"KeyHash must not be plaintext")
			assert.False(t, strings.HasPrefix(key.KeyHash, "sk_test_"),
				"KeyHash must not be plaintext")

			return nil
		},
	}
	handler, _ := newTestAPIKeyHandler(repo)

	reqBody := makeCreateRequest(t, CreateAPIKeyRequest{
		Name:   "Security Test Key",
		Scopes: []string{"watchpoints:read"},
	})

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleOwner, "org_sec")
	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys", reqBody)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Create(w, r)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestAPIKeyHandler_Create_InvalidScopes(t *testing.T) {
	repo := &mockAPIKeyRepo{}
	handler, _ := newTestAPIKeyHandler(repo)

	reqBody := makeCreateRequest(t, CreateAPIKeyRequest{
		Name:   "Bad Scopes Key",
		Scopes: []string{"watchpoints:read", "admin:nuke"},
	})

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys", reqBody)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Create(w, r)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Parse error response.
	var errResp core.APIErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp.Error.Message, "invalid scopes")
}

func TestAPIKeyHandler_Create_EmptyScopes(t *testing.T) {
	repo := &mockAPIKeyRepo{}
	handler, _ := newTestAPIKeyHandler(repo)

	reqBody := makeCreateRequest(t, CreateAPIKeyRequest{
		Name:   "Empty Scopes Key",
		Scopes: []string{},
	})

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys", reqBody)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Create(w, r)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAPIKeyHandler_Create_APIKeyActorRejected(t *testing.T) {
	// API keys cannot create other API keys.
	repo := &mockAPIKeyRepo{}
	handler, _ := newTestAPIKeyHandler(repo)

	reqBody := makeCreateRequest(t, CreateAPIKeyRequest{
		Name:   "Nested Key",
		Scopes: []string{"watchpoints:read"},
	})

	// Set actor type to API key.
	actor := types.Actor{
		ID:             "key_existing",
		Type:           types.ActorTypeAPIKey,
		OrganizationID: "org_test1",
		Role:           types.RoleAdmin,
	}
	ctx := types.WithActor(context.Background(), actor)

	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys", reqBody)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Create(w, r)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAPIKeyHandler_Create_RateLimitExceeded(t *testing.T) {
	repo := &mockAPIKeyRepo{
		countRecentByUserFn: func(ctx context.Context, userID string, since time.Time) (int, error) {
			return 5, nil // At the limit
		},
	}
	handler, _ := newTestAPIKeyHandler(repo)

	reqBody := makeCreateRequest(t, CreateAPIKeyRequest{
		Name:   "Rate Limited Key",
		Scopes: []string{"watchpoints:read"},
	})

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys", reqBody)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Create(w, r)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	var errResp core.APIErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Equal(t, string(types.ErrCodeRateLimit), errResp.Error.Code)
}

func TestAPIKeyHandler_Create_SourceRequiresAdminRole(t *testing.T) {
	// Members cannot set Source on API keys.
	repo := &mockAPIKeyRepo{}
	handler, _ := newTestAPIKeyHandler(repo)

	reqBody := makeCreateRequest(t, CreateAPIKeyRequest{
		Name:   "Source Key",
		Scopes: []string{"watchpoints:read"},
		Source: "wedding_app",
	})

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleMember, "org_test1")
	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys", reqBody)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Create(w, r)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAPIKeyHandler_Create_SourceAllowedForAdmin(t *testing.T) {
	repo := &mockAPIKeyRepo{}
	handler, _ := newTestAPIKeyHandler(repo)

	reqBody := makeCreateRequest(t, CreateAPIKeyRequest{
		Name:   "Source Key",
		Scopes: []string{"watchpoints:read"},
		Source: "weddingapp",
	})

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys", reqBody)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Create(w, r)

	assert.Equal(t, http.StatusCreated, w.Code)

	// Verify source was stored.
	require.NotNil(t, repo.capturedCreateKey)
	assert.Equal(t, "weddingapp", repo.capturedCreateKey.Source)
}

// =============================================================================
// Rotate Tests
// =============================================================================

func TestAPIKeyHandler_Rotate_Success(t *testing.T) {
	now := time.Now().UTC()
	existingKey := &types.APIKey{
		ID:             "key_old1",
		OrganizationID: "org_test1",
		KeyHash:        "$2a$12$oldhash",
		KeyPrefix:      "sk_live_oldprefix",
		Scopes:         []string{"watchpoints:read"},
		TestMode:       false,
		Name:           "My Key",
		CreatedAt:      now.Add(-24 * time.Hour),
	}

	repo := &mockAPIKeyRepo{
		getByIDFn: func(ctx context.Context, id, orgID string) (*types.APIKey, error) {
			return existingKey, nil
		},
	}
	handler, audit := newTestAPIKeyHandler(repo)

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")

	// Create a chi route context with the key ID.
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "key_old1")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys/key_old1/rotate", nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Rotate(w, r)

	assert.Equal(t, http.StatusOK, w.Code)

	// Parse response.
	var resp core.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	dataBytes, _ := json.Marshal(resp.Data)
	var secretResp APIKeySecretResponse
	err = json.Unmarshal(dataBytes, &secretResp)
	require.NoError(t, err)

	// Verify the new key is returned.
	assert.NotEmpty(t, secretResp.NewKey)
	assert.True(t, strings.HasPrefix(secretResp.NewKey, "sk_live_"))

	// Verify the grace period is set (approximately 24 hours from now).
	require.NotNil(t, secretResp.PreviousKeyValidUntil)
	expectedGrace := now.Add(24 * time.Hour)
	assert.WithinDuration(t, expectedGrace, *secretResp.PreviousKeyValidUntil, 5*time.Second,
		"Previous key valid until should be ~24 hours from now")

	// Verify scopes are inherited from old key.
	assert.Equal(t, existingKey.Scopes, secretResp.Scopes)

	// Verify the new key was stored with a bcrypt hash, not plaintext.
	require.NotNil(t, repo.capturedCreateKey)
	assert.True(t, strings.HasPrefix(repo.capturedCreateKey.KeyHash, "$2a$"),
		"Rotated key hash must be bcrypt format")

	// Verify the hash matches the returned plaintext.
	// Per USER-011 spec: bcrypt is applied directly to the plaintext.
	err = bcrypt.CompareHashAndPassword([]byte(repo.capturedCreateKey.KeyHash), []byte(secretResp.NewKey))
	assert.NoError(t, err, "Rotated key bcrypt hash must verify against the plaintext")

	// Verify audit event was emitted.
	require.Len(t, audit.events, 1)
	assert.Equal(t, "apikey.rotated", audit.events[0].Action)
}

func TestAPIKeyHandler_Rotate_KeyNotFound(t *testing.T) {
	repo := &mockAPIKeyRepo{
		getByIDFn: func(ctx context.Context, id, orgID string) (*types.APIKey, error) {
			return nil, types.NewAppError(types.ErrCodeNotFoundAPIKey, "not found", nil)
		},
	}
	handler, _ := newTestAPIKeyHandler(repo)

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "key_nonexistent")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys/key_nonexistent/rotate", nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Rotate(w, r)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAPIKeyHandler_Rotate_AlreadyRevokedKey(t *testing.T) {
	revokedAt := time.Now().UTC().Add(-1 * time.Hour)
	repo := &mockAPIKeyRepo{
		getByIDFn: func(ctx context.Context, id, orgID string) (*types.APIKey, error) {
			return &types.APIKey{
				ID:        "key_revoked",
				RevokedAt: &revokedAt,
			}, nil
		},
	}
	handler, _ := newTestAPIKeyHandler(repo)

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "key_revoked")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys/key_revoked/rotate", nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Rotate(w, r)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAPIKeyHandler_Rotate_APIKeyActorRejected(t *testing.T) {
	repo := &mockAPIKeyRepo{}
	handler, _ := newTestAPIKeyHandler(repo)

	actor := types.Actor{
		ID:             "key_existing",
		Type:           types.ActorTypeAPIKey,
		OrganizationID: "org_test1",
		Role:           types.RoleAdmin,
	}
	ctx := types.WithActor(context.Background(), actor)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "key_target")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

	r := httptest.NewRequest(http.MethodPost, "/v1/api-keys/key_target/rotate", nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Rotate(w, r)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =============================================================================
// Revoke Tests
// =============================================================================

func TestAPIKeyHandler_Revoke_Success(t *testing.T) {
	repo := &mockAPIKeyRepo{
		deleteFn: func(ctx context.Context, id, orgID string) error {
			assert.Equal(t, "key_to_revoke", id)
			assert.Equal(t, "org_test1", orgID)
			return nil
		},
	}
	handler, audit := newTestAPIKeyHandler(repo)

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "key_to_revoke")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

	r := httptest.NewRequest(http.MethodDelete, "/v1/api-keys/key_to_revoke", nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Revoke(w, r)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, w.Body.String(), "204 response must have no body")

	// Verify audit event.
	require.Len(t, audit.events, 1)
	assert.Equal(t, "apikey.revoked", audit.events[0].Action)
}

func TestAPIKeyHandler_Revoke_NotFound(t *testing.T) {
	repo := &mockAPIKeyRepo{
		deleteFn: func(ctx context.Context, id, orgID string) error {
			return types.NewAppError(types.ErrCodeNotFoundAPIKey, "not found", nil)
		},
	}
	handler, _ := newTestAPIKeyHandler(repo)

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "key_ghost")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

	r := httptest.NewRequest(http.MethodDelete, "/v1/api-keys/key_ghost", nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.Revoke(w, r)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =============================================================================
// List Tests
// =============================================================================

func TestAPIKeyHandler_List_Success(t *testing.T) {
	now := time.Now().UTC()
	repo := &mockAPIKeyRepo{
		listFn: func(ctx context.Context, orgID string, params APIKeyListParams) ([]*types.APIKey, error) {
			assert.Equal(t, "org_test1", orgID)
			return []*types.APIKey{
				{
					ID:        "key_1",
					KeyPrefix: "sk_live_abc",
					Name:      "Key One",
					Scopes:    []string{"watchpoints:read"},
					CreatedAt: now,
				},
				{
					ID:        "key_2",
					KeyPrefix: "sk_live_def",
					Name:      "Key Two",
					Scopes:    []string{"watchpoints:read", "watchpoints:write"},
					CreatedAt: now.Add(-1 * time.Hour),
				},
			}, nil
		},
	}
	handler, _ := newTestAPIKeyHandler(repo)

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	r := httptest.NewRequest(http.MethodGet, "/v1/api-keys", nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.List(w, r)

	assert.Equal(t, http.StatusOK, w.Code)

	// Parse response.
	var resp core.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Convert data to list of APIKeyResponse.
	dataBytes, _ := json.Marshal(resp.Data)
	var keys []APIKeyResponse
	err = json.Unmarshal(dataBytes, &keys)
	require.NoError(t, err)
	assert.Len(t, keys, 2)

	// Verify the full secret (key_hash) is never in the response.
	responseStr := w.Body.String()
	assert.NotContains(t, responseStr, "key_hash",
		"key_hash must never appear in list response")
	assert.NotContains(t, responseStr, "$2a$",
		"bcrypt hash must never appear in list response")
}

func TestAPIKeyHandler_List_WithPrefixFilter(t *testing.T) {
	repo := &mockAPIKeyRepo{
		listFn: func(ctx context.Context, orgID string, params APIKeyListParams) ([]*types.APIKey, error) {
			assert.Equal(t, "sk_live_leaked", params.Prefix)
			return []*types.APIKey{}, nil
		},
	}
	handler, _ := newTestAPIKeyHandler(repo)

	ctx := ctxWithActor(types.ActorTypeUser, types.RoleAdmin, "org_test1")
	r := httptest.NewRequest(http.MethodGet, "/v1/api-keys?prefix=sk_live_leaked", nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.List(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
}

// =============================================================================
// Scope Validation Tests
// =============================================================================

func TestValidateScopes_AllValid(t *testing.T) {
	err := validateScopes([]string{"watchpoints:read", "forecasts:read"})
	assert.NoError(t, err)
}

func TestValidateScopes_SomeInvalid(t *testing.T) {
	err := validateScopes([]string{"watchpoints:read", "admin:nuke"})
	require.Error(t, err)

	var appErr *types.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, types.ErrCodeValidationMissingField, appErr.Code)
	assert.Contains(t, appErr.Message, "invalid scopes")
}

func TestValidateScopes_AllInvalid(t *testing.T) {
	err := validateScopes([]string{"fake:scope1", "fake:scope2"})
	require.Error(t, err)
}

func TestValidateScopes_Empty(t *testing.T) {
	err := validateScopes([]string{})
	require.Error(t, err)

	var appErr *types.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, types.ErrCodeValidationMissingField, appErr.Code)
}

// =============================================================================
// Secret Generation Tests
// =============================================================================

func TestGenerateAPIKeySecret_LiveMode(t *testing.T) {
	plaintext, prefix, hash, err := generateAPIKeySecret(false)
	require.NoError(t, err)

	// Verify prefix format.
	assert.True(t, strings.HasPrefix(plaintext, "sk_live_"), "live mode key must start with sk_live_")
	assert.True(t, strings.HasPrefix(prefix, "sk_live_"), "live mode prefix must start with sk_live_")

	// Verify the prefix is a proper substring of the plaintext.
	assert.True(t, strings.HasPrefix(plaintext, prefix),
		"plaintext must start with its own prefix")

	// Verify the prefix is shorter than the full key.
	assert.True(t, len(prefix) < len(plaintext),
		"prefix must be shorter than full key")

	// Verify hash is bcrypt format.
	assert.True(t, strings.HasPrefix(hash, "$2a$"), "hash must be bcrypt format")

	// Verify the hash validates against the plaintext directly.
	// Per USER-011 spec: bcrypt is applied directly to the plaintext.
	err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext))
	assert.NoError(t, err, "hash must verify against plaintext")
}

func TestGenerateAPIKeySecret_TestMode(t *testing.T) {
	plaintext, prefix, hash, err := generateAPIKeySecret(true)
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(plaintext, "sk_test_"), "test mode key must start with sk_test_")
	assert.True(t, strings.HasPrefix(prefix, "sk_test_"), "test mode prefix must start with sk_test_")
	assert.NotEmpty(t, hash)
}

func TestGenerateAPIKeySecret_Uniqueness(t *testing.T) {
	// Generate multiple keys and verify they are all unique.
	secrets := make(map[string]bool)
	for i := 0; i < 10; i++ {
		plaintext, _, _, err := generateAPIKeySecret(false)
		require.NoError(t, err)
		assert.False(t, secrets[plaintext], "each generated key must be unique")
		secrets[plaintext] = true
	}
}

func TestGenerateAPIKeySecret_EntropyLength(t *testing.T) {
	// The key should be long enough to be cryptographically secure.
	plaintext, _, _, err := generateAPIKeySecret(false)
	require.NoError(t, err)

	// sk_live_ prefix (8 chars) + base64 of 32 bytes (43 chars) = ~51 chars
	assert.Greater(t, len(plaintext), 40,
		"key must be long enough for cryptographic security")
}
