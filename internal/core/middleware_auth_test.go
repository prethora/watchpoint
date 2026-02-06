package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"watchpoint/internal/types"
)

// --- AuthMiddleware Tests ---

func TestAuthMiddleware_ValidToken_InjectsActor(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	expectedActor := &types.Actor{
		ID:             "user_abc123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_xyz789",
		Role:           types.RoleAdmin,
		Scopes:         types.RoleScopeMap[types.RoleAdmin],
		Source:         "dashboard",
	}
	srv.Authenticator = &MockAuthenticator{Actor: expectedActor}

	var capturedActor types.Actor
	var actorFound bool
	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedActor, actorFound = types.GetActor(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer sk_live_test123")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if !actorFound {
		t.Fatal("expected actor in context")
	}
	if capturedActor.ID != expectedActor.ID {
		t.Errorf("actor ID: got %q, want %q", capturedActor.ID, expectedActor.ID)
	}
	if capturedActor.Type != expectedActor.Type {
		t.Errorf("actor Type: got %q, want %q", capturedActor.Type, expectedActor.Type)
	}
	if capturedActor.OrganizationID != expectedActor.OrganizationID {
		t.Errorf("actor OrgID: got %q, want %q", capturedActor.OrganizationID, expectedActor.OrganizationID)
	}
	if capturedActor.Role != expectedActor.Role {
		t.Errorf("actor Role: got %q, want %q", capturedActor.Role, expectedActor.Role)
	}
	if capturedActor.Source != expectedActor.Source {
		t.Errorf("actor Source: got %q, want %q", capturedActor.Source, expectedActor.Source)
	}
}

func TestAuthMiddleware_ValidAPIKey_ScopesPresent(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	expectedActor := &types.Actor{
		ID:             "key_abc123",
		Type:           types.ActorTypeAPIKey,
		OrganizationID: "org_xyz789",
		Scopes:         []string{"watchpoints:read", "watchpoints:write"},
		IsTestMode:     false,
		Source:         "wedding_app",
	}
	srv.Authenticator = &MockAuthenticator{Actor: expectedActor}

	var capturedActor types.Actor
	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedActor, _ = types.GetActor(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer sk_live_keyabc123")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if len(capturedActor.Scopes) != 2 {
		t.Errorf("expected 2 scopes, got %d", len(capturedActor.Scopes))
	}
	if !capturedActor.HasScope("watchpoints:read") {
		t.Error("expected actor to have watchpoints:read scope")
	}
	if !capturedActor.HasScope("watchpoints:write") {
		t.Error("expected actor to have watchpoints:write scope")
	}
}

func TestAuthMiddleware_MissingAuthHeader_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Actor: &types.Actor{ID: "should_not_reach"},
	}

	nextCalled := false
	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	// No Authorization header.
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next handler should NOT be called when Authorization header is missing")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != string(types.ErrCodeAuthTokenMissing) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeAuthTokenMissing, resp.Error.Code)
	}
}

func TestAuthMiddleware_EmptyBearerToken_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{Actor: &types.Actor{ID: "should_not_reach"}}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != string(types.ErrCodeAuthTokenMissing) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeAuthTokenMissing, resp.Error.Code)
	}
}

func TestAuthMiddleware_NonBearerScheme_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{Actor: &types.Actor{ID: "should_not_reach"}}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_InvalidToken_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthTokenInvalid, "token not found", nil),
	}

	nextCalled := false
	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer sk_live_invalid_key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next handler should NOT be called for invalid token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != string(types.ErrCodeAuthTokenInvalid) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeAuthTokenInvalid, resp.Error.Code)
	}
}

func TestAuthMiddleware_ExpiredToken_Returns401WithExpiredCode(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthTokenExpired, "token expired", nil),
	}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer sk_live_expired_key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	// The error code must be auth_token_expired (not auth_token_invalid)
	// so clients can distinguish expired vs invalid for UX purposes.
	if resp.Error.Code != string(types.ErrCodeAuthTokenExpired) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeAuthTokenExpired, resp.Error.Code)
	}
}

func TestAuthMiddleware_RevokedToken_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthTokenRevoked, "token revoked", nil),
	}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer sk_live_revoked_key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	// Revoked tokens should be mapped to auth_token_invalid to not
	// leak information about token revocation status.
	if resp.Error.Code != string(types.ErrCodeAuthTokenInvalid) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeAuthTokenInvalid, resp.Error.Code)
	}
}

func TestAuthMiddleware_SessionExpired_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthSessionExpired, "session expired", nil),
	}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer sess_expired_session")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != string(types.ErrCodeAuthSessionExpired) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeAuthSessionExpired, resp.Error.Code)
	}
}

func TestAuthMiddleware_NilAuthenticator_PassesThrough(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = nil

	nextCalled := false
	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	// No auth header -- should still pass through when authenticator is nil.
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called when Authenticator is nil")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_PublicPath_Health_SkipsAuth(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthTokenInvalid, "should not be called", nil),
	}

	nextCalled := false
	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	// No auth header on public path.
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called for public /health path")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_PublicPath_OpenAPI_SkipsAuth(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthTokenInvalid, "should not be called", nil),
	}

	nextCalled := false
	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called for public /openapi.json path")
	}
}

func TestAuthMiddleware_PreservesRequestID(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid", nil),
	}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer bad_token")
	ctx := types.WithRequestID(req.Context(), "req_auth_test_999")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.RequestID != "req_auth_test_999" {
		t.Errorf("expected request_id %q, got %q", "req_auth_test_999", resp.Error.RequestID)
	}
}

func TestAuthMiddleware_RecordsTokenCall(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	mock := &MockAuthenticator{
		Actor: &types.Actor{
			ID:   "user_1",
			Type: types.ActorTypeUser,
			Role: types.RoleMember,
		},
	}
	srv.Authenticator = mock

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer sk_live_mytoken123")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if len(mock.Calls) != 1 {
		t.Fatalf("expected 1 call to ResolveToken, got %d", len(mock.Calls))
	}
	if mock.Calls[0] != "sk_live_mytoken123" {
		t.Errorf("expected token %q, got %q", "sk_live_mytoken123", mock.Calls[0])
	}
}

func TestAuthMiddleware_GenericError_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		ResolveTokenFunc: func(ctx context.Context, token string) (*types.Actor, error) {
			return nil, context.DeadlineExceeded
		},
	}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer some_token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	// Generic errors should be mapped to auth_token_invalid to not leak details.
	if resp.Error.Code != string(types.ErrCodeAuthTokenInvalid) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeAuthTokenInvalid, resp.Error.Code)
	}
}

func TestAuthMiddleware_NilActor_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	// MockAuthenticator returns nil actor and nil error.
	srv.Authenticator = &MockAuthenticator{}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer some_token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_ResponseIsJSON(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid", nil),
	}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer bad_token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

func TestAuthMiddleware_BearerCaseInsensitive(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Actor: &types.Actor{
			ID:   "user_1",
			Type: types.ActorTypeAPIKey,
		},
	}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// "bearer" lowercase should be accepted per RFC 7235 (case-insensitive scheme).
	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "bearer sk_live_test")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200 for lowercase bearer (RFC 7235), got %d", rec.Code)
	}
}

func TestAuthMiddleware_BearerMixedCase(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Actor: &types.Actor{
			ID:   "user_1",
			Type: types.ActorTypeAPIKey,
		},
	}

	handler := srv.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// "BEARER" uppercase should also be accepted.
	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "BEARER sk_live_test")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200 for uppercase BEARER (RFC 7235), got %d", rec.Code)
	}
}

// --- RequireRole Tests ---

func TestRequireRole_MemberCanAccessMemberRoute(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	nextCalled := false
	handler := srv.RequireRole(types.RoleMember)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "user_1",
		Type: types.ActorTypeUser,
		Role: types.RoleMember,
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("Member should be able to access Member-required route")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestRequireRole_AdminCanAccessMemberRoute(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	nextCalled := false
	handler := srv.RequireRole(types.RoleMember)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "user_1",
		Type: types.ActorTypeUser,
		Role: types.RoleAdmin,
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("Admin should be able to access Member-required route")
	}
}

func TestRequireRole_OwnerCanAccessAdminRoute(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	nextCalled := false
	handler := srv.RequireRole(types.RoleAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/settings", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "user_owner",
		Type: types.ActorTypeUser,
		Role: types.RoleOwner,
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("Owner should be able to access Admin-required route")
	}
}

func TestRequireRole_MemberCannotAccessAdminRoute(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	nextCalled := false
	handler := srv.RequireRole(types.RoleAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/settings", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "user_member",
		Type: types.ActorTypeUser,
		Role: types.RoleMember,
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("Member should NOT be able to access Admin-required route")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != string(types.ErrCodePermissionRole) {
		t.Errorf("expected error code %q, got %q", types.ErrCodePermissionRole, resp.Error.Code)
	}
}

func TestRequireRole_MemberCannotAccessOwnerRoute(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	handler := srv.RequireRole(types.RoleOwner)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodDelete, "/v1/org", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "user_member",
		Type: types.ActorTypeUser,
		Role: types.RoleMember,
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
}

func TestRequireRole_AdminCannotAccessOwnerRoute(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	handler := srv.RequireRole(types.RoleOwner)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodDelete, "/v1/org", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "user_admin",
		Type: types.ActorTypeUser,
		Role: types.RoleAdmin,
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
}

func TestRequireRole_NoActor_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	handler := srv.RequireRole(types.RoleMember)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	// No actor in context.
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

func TestRequireRole_SystemActor_BypassesCheck(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	nextCalled := false
	handler := srv.RequireRole(types.RoleOwner)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/internal/trigger", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "system",
		Type: types.ActorTypeSystem,
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("System actor should bypass role checks")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestRequireRole_PreservesRequestID(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	handler := srv.RequireRole(types.RoleOwner)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/settings", nil)
	ctx := types.WithRequestID(req.Context(), "req_role_test_123")
	ctx = types.WithActor(ctx, types.Actor{
		ID:   "user_member",
		Type: types.ActorTypeUser,
		Role: types.RoleMember,
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.RequestID != "req_role_test_123" {
		t.Errorf("expected request_id %q, got %q", "req_role_test_123", resp.Error.RequestID)
	}
}

// --- RequireScope Tests ---

func TestRequireScope_APIKey_HasScope_Passes(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	nextCalled := false
	handler := srv.RequireScope("watchpoints:write")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:     "key_abc123",
		Type:   types.ActorTypeAPIKey,
		Scopes: []string{"watchpoints:read", "watchpoints:write"},
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("API key with required scope should pass")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}
}

func TestRequireScope_APIKey_MissingScope_Returns403(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	nextCalled := false
	handler := srv.RequireScope("watchpoints:write")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:     "key_readonly",
		Type:   types.ActorTypeAPIKey,
		Scopes: []string{"watchpoints:read"}, // Missing watchpoints:write
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("API key without required scope should NOT pass")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != string(types.ErrCodePermissionScope) {
		t.Errorf("expected error code %q, got %q", types.ErrCodePermissionScope, resp.Error.Code)
	}
}

func TestRequireScope_User_ImplicitScopes_Passes(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	nextCalled := false
	handler := srv.RequireScope("watchpoints:write")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:     "user_1",
		Type:   types.ActorTypeUser,
		Role:   types.RoleMember,
		Scopes: types.RoleScopeMap[types.RoleMember], // Implicit scopes from role
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("User with implicit scope from role should pass")
	}
}

func TestRequireScope_User_MissingImplicitScope_Returns403(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	handler := srv.RequireScope("account:write")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPut, "/v1/org/settings", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:     "user_member",
		Type:   types.ActorTypeUser,
		Role:   types.RoleMember,
		Scopes: types.RoleScopeMap[types.RoleMember], // Member lacks account:write
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
}

func TestRequireScope_NoActor_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	handler := srv.RequireScope("watchpoints:read")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	// No actor in context.
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

func TestRequireScope_SystemActor_BypassesCheck(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	nextCalled := false
	handler := srv.RequireScope("account:write")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/internal/trigger", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "system",
		Type: types.ActorTypeSystem,
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("System actor should bypass scope checks")
	}
}

func TestRequireScope_APIKey_EmptyScopes_Returns403(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)

	handler := srv.RequireScope("watchpoints:read")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:     "key_empty",
		Type:   types.ActorTypeAPIKey,
		Scopes: []string{}, // No scopes at all
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
}

// --- extractBearerToken Tests ---

func TestExtractBearerToken_ValidBearer(t *testing.T) {
	token := extractBearerToken("Bearer sk_live_abc123")
	if token != "sk_live_abc123" {
		t.Errorf("got %q, want %q", token, "sk_live_abc123")
	}
}

func TestExtractBearerToken_EmptyAfterBearer(t *testing.T) {
	token := extractBearerToken("Bearer ")
	if token != "" {
		t.Errorf("got %q, want empty string", token)
	}
}

func TestExtractBearerToken_NoBearer(t *testing.T) {
	token := extractBearerToken("Basic dXNlcjpwYXNz")
	if token != "" {
		t.Errorf("got %q, want empty string", token)
	}
}

func TestExtractBearerToken_CaseInsensitive(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"lowercase", "bearer sk_live_abc", "sk_live_abc"},
		{"uppercase", "BEARER sk_live_abc", "sk_live_abc"},
		{"mixed case", "BeArEr sk_live_abc", "sk_live_abc"},
		{"standard", "Bearer sk_live_abc", "sk_live_abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractBearerToken(tc.input)
			if got != tc.want {
				t.Errorf("extractBearerToken(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestExtractBearerToken_BearerOnly(t *testing.T) {
	token := extractBearerToken("Bearer")
	if token != "" {
		t.Errorf("got %q, want empty string", token)
	}
}

func TestExtractBearerToken_WithExtraSpaces(t *testing.T) {
	token := extractBearerToken("Bearer   sk_live_abc123  ")
	if token != "sk_live_abc123" {
		t.Errorf("got %q, want %q", token, "sk_live_abc123")
	}
}

func TestExtractBearerToken_EmptyString(t *testing.T) {
	token := extractBearerToken("")
	if token != "" {
		t.Errorf("got %q, want empty string", token)
	}
}

func TestExtractBearerToken_SessionToken(t *testing.T) {
	token := extractBearerToken("Bearer sess_abc123xyz")
	if token != "sess_abc123xyz" {
		t.Errorf("got %q, want %q", token, "sess_abc123xyz")
	}
}

// --- Integration: AuthMiddleware + RequireRole ---

func TestAuthThenRequireRole_ValidAuth_InsufficientRole(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Actor: &types.Actor{
			ID:             "user_member",
			Type:           types.ActorTypeUser,
			OrganizationID: "org_1",
			Role:           types.RoleMember,
			Scopes:         types.RoleScopeMap[types.RoleMember],
		},
	}

	nextCalled := false
	handler := srv.AuthMiddleware(
		srv.RequireRole(types.RoleAdmin)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusOK)
			}),
		),
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/settings", nil)
	req.Header.Set("Authorization", "Bearer sk_live_member_key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("handler should not be reached; member cannot access admin route")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
}

func TestAuthThenRequireRole_ValidAuth_SufficientRole(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Actor: &types.Actor{
			ID:             "user_admin",
			Type:           types.ActorTypeUser,
			OrganizationID: "org_1",
			Role:           types.RoleAdmin,
			Scopes:         types.RoleScopeMap[types.RoleAdmin],
		},
	}

	nextCalled := false
	handler := srv.AuthMiddleware(
		srv.RequireRole(types.RoleAdmin)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusOK)
			}),
		),
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/settings", nil)
	req.Header.Set("Authorization", "Bearer sk_live_admin_key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("handler should be reached; admin can access admin route")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

// --- Integration: AuthMiddleware + RequireScope ---

func TestAuthThenRequireScope_ValidAuth_HasScope(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Actor: &types.Actor{
			ID:             "key_rw",
			Type:           types.ActorTypeAPIKey,
			OrganizationID: "org_1",
			Scopes:         []string{"watchpoints:read", "watchpoints:write"},
		},
	}

	nextCalled := false
	handler := srv.AuthMiddleware(
		srv.RequireScope("watchpoints:write")(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusCreated)
			}),
		),
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer sk_live_rw_key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("handler should be reached; API key has required scope")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}
}

func TestAuthThenRequireScope_ValidAuth_MissingScope(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Actor: &types.Actor{
			ID:             "key_ro",
			Type:           types.ActorTypeAPIKey,
			OrganizationID: "org_1",
			Scopes:         []string{"watchpoints:read"}, // Missing write
		},
	}

	nextCalled := false
	handler := srv.AuthMiddleware(
		srv.RequireScope("watchpoints:write")(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusCreated)
			}),
		),
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer sk_live_ro_key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("handler should NOT be reached; API key lacks required scope")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
}

func TestAuthThenRequireScope_NoAuth_Returns401(t *testing.T) {
	srv := newTestServerForAuthMiddleware(t)
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthTokenInvalid, "bad", nil),
	}

	handler := srv.AuthMiddleware(
		srv.RequireScope("watchpoints:read")(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		),
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.Header.Set("Authorization", "Bearer bad_token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// AuthMiddleware should reject before RequireScope runs.
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

// --- Test Helpers ---

// newTestServerForAuthMiddleware creates a minimal Server suitable for testing
// the auth middleware in isolation.
func newTestServerForAuthMiddleware(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return &Server{
		Logger: logger,
	}
}
