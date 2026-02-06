package core

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"watchpoint/internal/types"
)

// --- IPSecurityMiddleware Tests ---

func TestIPSecurityMiddleware_AllowsUnblockedIP(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)
	srv.SecurityService = &MockSecurityService{
		BlockedIPs: map[string]bool{
			"10.0.0.99": true, // Some other blocked IP
		},
	}

	nextCalled := false
	handler := srv.IPSecurityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.RemoteAddr = "192.168.1.1:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called for unblocked IP")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestIPSecurityMiddleware_BlocksBlockedIP(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)
	srv.SecurityService = &MockSecurityService{
		BlockedIPs: map[string]bool{
			"192.168.1.100": true,
		},
	}

	nextCalled := false
	handler := srv.IPSecurityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.RemoteAddr = "192.168.1.100:54321"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next handler should NOT be called for blocked IP")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}

	// Verify JSON response body.
	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != errCodeIPBlocked {
		t.Errorf("expected error code %q, got %q", errCodeIPBlocked, resp.Error.Code)
	}
	if resp.Error.Message != "Access denied" {
		t.Errorf("expected message %q, got %q", "Access denied", resp.Error.Message)
	}
}

func TestIPSecurityMiddleware_BlockedIP_ReturnsJSON(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)
	srv.SecurityService = &MockSecurityService{
		BlockedIPs: map[string]bool{"1.2.3.4": true},
	}

	handler := srv.IPSecurityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.RemoteAddr = "1.2.3.4:8080"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

func TestIPSecurityMiddleware_UsesXForwardedFor(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)
	mockSecurity := &MockSecurityService{
		BlockedIPs: map[string]bool{
			"203.0.113.50": true,
		},
	}
	srv.SecurityService = mockSecurity

	nextCalled := false
	handler := srv.IPSecurityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	// X-Forwarded-For with the blocked IP as the original client.
	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	req.RemoteAddr = "10.0.0.1:8080" // Proxy IP (not blocked)
	req.Header.Set("X-Forwarded-For", "203.0.113.50, 10.0.0.1")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next handler should NOT be called; X-Forwarded-For IP is blocked")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
}

func TestIPSecurityMiddleware_XForwardedFor_SingleIP(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)
	mockSecurity := &MockSecurityService{
		BlockedIPs: map[string]bool{
			"198.51.100.23": true,
		},
	}
	srv.SecurityService = mockSecurity

	handler := srv.IPSecurityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.23")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
}

func TestIPSecurityMiddleware_NilSecurityService_PassesThrough(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)
	srv.SecurityService = nil

	nextCalled := false
	handler := srv.IPSecurityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "1.2.3.4:8080"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called when SecurityService is nil")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestIPSecurityMiddleware_PreservesRequestID(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)
	srv.SecurityService = &MockSecurityService{
		BlockedIPs: map[string]bool{"5.5.5.5": true},
	}

	handler := srv.IPSecurityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "5.5.5.5:1234"
	ctx := types.WithRequestID(req.Context(), "req_security_test")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.RequestID != "req_security_test" {
		t.Errorf("expected request_id %q, got %q", "req_security_test", resp.Error.RequestID)
	}
}

func TestIPSecurityMiddleware_FallbackToRemoteAddr_NoPort(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)
	srv.SecurityService = &MockSecurityService{
		BlockedIPs: map[string]bool{"1.2.3.4": true},
	}

	handler := srv.IPSecurityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	// RemoteAddr without port (can happen in some test scenarios).
	req.RemoteAddr = "1.2.3.4"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
}

// --- CSRFMiddleware Tests ---

func TestCSRFMiddleware_SafeMethods_SkipValidation(t *testing.T) {
	safeMethods := []string{http.MethodGet, http.MethodHead, http.MethodOptions}

	for _, method := range safeMethods {
		t.Run(method, func(t *testing.T) {
			srv := newTestServerForSecurityMiddleware(t)

			nextCalled := false
			handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(method, "/v1/watchpoints", nil)
			// Inject a user actor without CSRF token in context -- should still pass for safe methods.
			ctx := types.WithActor(req.Context(), types.Actor{
				ID:   "user_abc",
				Type: types.ActorTypeUser,
			})
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if !nextCalled {
				t.Errorf("%s: next handler should be called for safe methods", method)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("%s: expected status 200, got %d", method, rec.Code)
			}
		})
	}
}

func TestCSRFMiddleware_APIKey_SkipsValidation(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	nextCalled := false
	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "key_abc123",
		Type:           types.ActorTypeAPIKey,
		OrganizationID: "org_xyz789",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called for API key actors")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}
}

func TestCSRFMiddleware_SystemActor_SkipsValidation(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	nextCalled := false
	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		t.Error("next handler should be called for system actors")
	}
}

func TestCSRFMiddleware_NoActor_PassesThrough(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	nextCalled := false
	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	// No actor in context -- unauthenticated request.
	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called when no actor in context")
	}
}

func TestCSRFMiddleware_UserActor_ValidCSRF(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	nextCalled := false
	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusCreated)
	}))

	csrfToken := "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6"

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("X-CSRF-Token", csrfToken)

	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_abc123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_xyz789",
	})
	ctx = types.WithSessionCSRFToken(ctx, csrfToken)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called when CSRF token is valid")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}
}

func TestCSRFMiddleware_UserActor_MissingCSRFHeader(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	nextCalled := false
	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	// No X-CSRF-Token header set.

	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_abc123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_xyz789",
	})
	ctx = types.WithSessionCSRFToken(ctx, "valid_csrf_token")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next handler should NOT be called when CSRF header is missing")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != errCodeCSRFInvalid {
		t.Errorf("expected error code %q, got %q", errCodeCSRFInvalid, resp.Error.Code)
	}
}

func TestCSRFMiddleware_UserActor_WrongCSRFToken(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	nextCalled := false
	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPut, "/v1/watchpoints/wp_123", nil)
	req.Header.Set("X-CSRF-Token", "wrong_token_value")

	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_abc123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_xyz789",
	})
	ctx = types.WithSessionCSRFToken(ctx, "correct_csrf_token")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next handler should NOT be called when CSRF token is wrong")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != errCodeCSRFInvalid {
		t.Errorf("expected error code %q, got %q", errCodeCSRFInvalid, resp.Error.Code)
	}
	if resp.Error.Message != "CSRF token is invalid" {
		t.Errorf("expected message %q, got %q", "CSRF token is invalid", resp.Error.Message)
	}
}

func TestCSRFMiddleware_UserActor_NoSessionCSRFInContext(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	nextCalled := false
	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodDelete, "/v1/watchpoints/wp_123", nil)
	req.Header.Set("X-CSRF-Token", "some_token")

	// Actor is user but no session CSRF token in context.
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_abc123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_xyz789",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next handler should NOT be called when session CSRF token is missing from context")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
}

func TestCSRFMiddleware_UserActor_PATCH_ValidToken(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	nextCalled := false
	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	csrfToken := "patch_csrf_token_12345678901234"

	req := httptest.NewRequest(http.MethodPatch, "/v1/watchpoints/wp_123", nil)
	req.Header.Set("X-CSRF-Token", csrfToken)

	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "user_patch",
		Type: types.ActorTypeUser,
	})
	ctx = types.WithSessionCSRFToken(ctx, csrfToken)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called for PATCH with valid CSRF")
	}
}

func TestCSRFMiddleware_UserActor_DELETE_ValidToken(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	nextCalled := false
	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))

	csrfToken := "delete_csrf_token_1234567890123"

	req := httptest.NewRequest(http.MethodDelete, "/v1/watchpoints/wp_123", nil)
	req.Header.Set("X-CSRF-Token", csrfToken)

	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "user_delete",
		Type: types.ActorTypeUser,
	})
	ctx = types.WithSessionCSRFToken(ctx, csrfToken)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called for DELETE with valid CSRF")
	}
}

func TestCSRFMiddleware_ResponseIsJSON(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	// No CSRF header.

	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "user_json",
		Type: types.ActorTypeUser,
	})
	ctx = types.WithSessionCSRFToken(ctx, "some_token")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

func TestCSRFMiddleware_PreservesRequestID(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)

	handler := srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	ctx := types.WithRequestID(req.Context(), "req_csrf_test_123")
	ctx = types.WithActor(ctx, types.Actor{
		ID:   "user_reqid",
		Type: types.ActorTypeUser,
	})
	ctx = types.WithSessionCSRFToken(ctx, "token_abc")
	req = req.WithContext(ctx)
	// Set a wrong CSRF token to trigger failure.
	req.Header.Set("X-CSRF-Token", "wrong_token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.RequestID != "req_csrf_test_123" {
		t.Errorf("expected request_id %q, got %q", "req_csrf_test_123", resp.Error.RequestID)
	}
}

// --- extractClientIP Tests ---

func TestExtractClientIP_XForwardedFor_MultipleIPs(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.50, 70.41.3.18, 150.172.238.178")
	req.RemoteAddr = "10.0.0.1:8080"

	ip := extractClientIP(req)
	if ip != "203.0.113.50" {
		t.Errorf("expected first IP from X-Forwarded-For, got %q", ip)
	}
}

func TestExtractClientIP_XForwardedFor_SingleIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.23")
	req.RemoteAddr = "10.0.0.1:8080"

	ip := extractClientIP(req)
	if ip != "198.51.100.23" {
		t.Errorf("expected IP from X-Forwarded-For, got %q", ip)
	}
}

func TestExtractClientIP_NoXForwardedFor_WithPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "192.168.1.50:54321"

	ip := extractClientIP(req)
	if ip != "192.168.1.50" {
		t.Errorf("expected IP without port, got %q", ip)
	}
}

func TestExtractClientIP_NoXForwardedFor_NoPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "192.168.1.50"

	ip := extractClientIP(req)
	if ip != "192.168.1.50" {
		t.Errorf("expected raw RemoteAddr, got %q", ip)
	}
}

func TestExtractClientIP_XForwardedFor_WithSpaces(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Forwarded-For", "  203.0.113.50 , 70.41.3.18")
	req.RemoteAddr = "10.0.0.1:8080"

	ip := extractClientIP(req)
	if ip != "203.0.113.50" {
		t.Errorf("expected trimmed IP from X-Forwarded-For, got %q", ip)
	}
}

func TestExtractClientIP_IPv6_RemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "[::1]:8080"

	ip := extractClientIP(req)
	if ip != "::1" {
		t.Errorf("expected IPv6 address, got %q", ip)
	}
}

// --- isSafeMethod Tests ---

func TestIsSafeMethod(t *testing.T) {
	tests := []struct {
		method string
		safe   bool
	}{
		{http.MethodGet, true},
		{http.MethodHead, true},
		{http.MethodOptions, true},
		{http.MethodPost, false},
		{http.MethodPut, false},
		{http.MethodPatch, false},
		{http.MethodDelete, false},
	}

	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			got := isSafeMethod(tc.method)
			if got != tc.safe {
				t.Errorf("isSafeMethod(%q): got %v, want %v", tc.method, got, tc.safe)
			}
		})
	}
}

// --- Integration Tests ---

func TestIPSecurityThenCSRF_BlockedIP_NeverReachesCSRF(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)
	srv.SecurityService = &MockSecurityService{
		BlockedIPs: map[string]bool{"10.10.10.10": true},
	}

	nextCalled := false
	// Chain: IPSecurity -> CSRF -> handler.
	handler := srv.IPSecurityMiddleware(srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.RemoteAddr = "10.10.10.10:1234"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("handler should not be reached when IP is blocked")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	// Should be IP blocked, not CSRF error.
	if resp.Error.Code != errCodeIPBlocked {
		t.Errorf("expected error code %q, got %q", errCodeIPBlocked, resp.Error.Code)
	}
}

func TestIPSecurityThenCSRF_AllowedIP_ValidCSRF(t *testing.T) {
	srv := newTestServerForSecurityMiddleware(t)
	srv.SecurityService = &MockSecurityService{
		BlockedIPs: map[string]bool{}, // No blocked IPs.
	}

	csrfToken := "integration_csrf_token_1234567"
	nextCalled := false

	handler := srv.IPSecurityMiddleware(srv.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusCreated)
	})))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.RemoteAddr = "192.168.1.1:8080"
	req.Header.Set("X-CSRF-Token", csrfToken)

	ctx := types.WithActor(req.Context(), types.Actor{
		ID:   "user_integ",
		Type: types.ActorTypeUser,
	})
	ctx = types.WithSessionCSRFToken(ctx, csrfToken)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("handler should be reached with allowed IP and valid CSRF")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}
}

// --- Test Helpers ---

// newTestServerForSecurityMiddleware creates a minimal Server suitable for testing
// the security middleware in isolation.
func newTestServerForSecurityMiddleware(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return &Server{
		Logger: logger,
	}
}
