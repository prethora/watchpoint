package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"watchpoint/internal/config"
	"watchpoint/internal/types"
)

// newTestServerForRoutes creates a fully-wired test Server with MountRoutes called.
// All middleware dependencies are set to safe mocks so the middleware chain
// executes without nil-pointer panics.
func newTestServerForRoutes(t *testing.T) *Server {
	t.Helper()

	cfg := &config.Config{
		Environment: "local",
		Security: config.SecurityConfig{
			CorsAllowedOrigins: []string{"*"},
		},
	}
	repos := &mockRepositoryRegistry{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	// Set optional dependencies to exercise all middleware without panics.
	srv.SecurityService = &MockSecurityService{}
	srv.Authenticator = &MockAuthenticator{
		Actor: &types.Actor{
			ID:             "user_test",
			Type:           types.ActorTypeAPIKey,
			OrganizationID: "org_test",
			Source:         "test",
		},
	}
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{Allowed: true, Remaining: 999, ResetAt: time.Now().Add(time.Hour)},
	}
	srv.IdempotencyStore = NewMockIdempotencyStore()
	srv.Metrics = &mockMetricsCollector{}

	srv.MountRoutes()
	return srv
}

// TestMountRoutes_MiddlewareCount verifies that registerGlobalMiddleware
// registers exactly 13 middleware in the chain. This acts as a safeguard
// against accidentally adding or removing middleware from the chain.
func TestMountRoutes_MiddlewareCount(t *testing.T) {
	srv := newTestServerForRoutes(t)

	middlewares := srv.Router().Middlewares()
	expected := 13

	if len(middlewares) != expected {
		t.Errorf("expected %d middleware registered, got %d", expected, len(middlewares))
	}
}

// TestMountRoutes_HealthEndpoint verifies the /health endpoint is mounted
// and returns a 200 response.
func TestMountRoutes_HealthEndpoint(t *testing.T) {
	srv := newTestServerForRoutes(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	// Add an Authorization header so AuthMiddleware doesn't reject us.
	// The MockAuthenticator will resolve this to a default Actor.
	req.Header.Set("Authorization", "Bearer test_token")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /health: expected status 200, got %d", w.Code)
	}

	// Verify response is JSON.
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("GET /health: expected Content-Type application/json, got %q", ct)
	}
}

// TestMountRoutes_HealthEndpoint_NoAuth verifies that /health is accessible
// without authentication (it's in the public paths list).
func TestMountRoutes_HealthEndpoint_NoAuth(t *testing.T) {
	srv := newTestServerForRoutes(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	// No Authorization header.
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Health should be accessible without auth.
	if w.Code != http.StatusOK {
		t.Errorf("GET /health without auth: expected 200, got %d", w.Code)
	}
}

// TestMountRoutes_OpenAPIEndpoint verifies the /openapi.json endpoint is
// mounted and returns a JSON response.
func TestMountRoutes_OpenAPIEndpoint(t *testing.T) {
	srv := newTestServerForRoutes(t)

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	// OpenAPI spec is a public path.
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /openapi.json: expected status 200, got %d", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("GET /openapi.json: expected Content-Type application/json, got %q", ct)
	}
}

// TestMountRoutes_V1Group verifies the /v1 route group exists. Since mountV1
// is currently a stub, requesting a path under /v1 should return 404/405.
func TestMountRoutes_V1Group(t *testing.T) {
	srv := newTestServerForRoutes(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer test_token")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Since mountV1 is a stub, /v1/nonexistent should yield 404.
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /v1/nonexistent: expected 404, got %d", w.Code)
	}
}

// TestMountRoutes_SecurityHeaders verifies that all responses include the
// security headers set by SecurityHeadersMiddleware, regardless of the endpoint.
func TestMountRoutes_SecurityHeaders(t *testing.T) {
	srv := newTestServerForRoutes(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	headers := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":       "DENY",
		"X-XSS-Protection":      "1; mode=block",
	}

	for name, expected := range headers {
		got := w.Header().Get(name)
		if got != expected {
			t.Errorf("header %s: got %q, want %q", name, got, expected)
		}
	}
}

// TestMountRoutes_RequestIDGenerated verifies that a request without an
// X-Request-Id header gets one generated and set on the response.
func TestMountRoutes_RequestIDGenerated(t *testing.T) {
	srv := newTestServerForRoutes(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	requestID := w.Header().Get("X-Request-Id")
	if requestID == "" {
		t.Error("expected X-Request-Id header to be set on response")
	}
	// Generated IDs are 32 hex chars (16 bytes).
	if len(requestID) != 32 {
		t.Errorf("expected X-Request-Id to be 32 hex chars, got %d chars: %q", len(requestID), requestID)
	}
}

// TestMountRoutes_RequestIDPropagated verifies that a request with an existing
// X-Request-Id header has that value propagated to the response.
func TestMountRoutes_RequestIDPropagated(t *testing.T) {
	srv := newTestServerForRoutes(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("X-Request-Id", "client-correlation-id")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	got := w.Header().Get("X-Request-Id")
	if got != "client-correlation-id" {
		t.Errorf("X-Request-Id not propagated: got %q, want %q", got, "client-correlation-id")
	}
}

// TestMiddlewareOrder_IPSecurityBeforeAuth verifies that IPSecurityMiddleware
// executes before AuthMiddleware. When an IP is blocked, the auth middleware
// should never be called (no ResolveToken call).
func TestMiddlewareOrder_IPSecurityBeforeAuth(t *testing.T) {
	cfg := &config.Config{
		Environment: "local",
		Security: config.SecurityConfig{
			CorsAllowedOrigins: []string{"*"},
		},
	}
	repos := &mockRepositoryRegistry{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	// Configure IP security to block the test IP.
	srv.SecurityService = &MockSecurityService{
		BlockedIPs: map[string]bool{"192.168.1.100": true},
	}

	// Configure auth mock to track calls.
	authMock := &MockAuthenticator{
		Actor: &types.Actor{ID: "user_1", Type: types.ActorTypeUser, OrganizationID: "org_1"},
	}
	srv.Authenticator = authMock
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{Allowed: true, Remaining: 999, ResetAt: time.Now().Add(time.Hour)},
	}
	srv.IdempotencyStore = NewMockIdempotencyStore()

	srv.MountRoutes()

	// Make a request from the blocked IP.
	req := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	req.Header.Set("X-Forwarded-For", "192.168.1.100")
	req.Header.Set("Authorization", "Bearer test_token")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should be blocked with 403.
	if w.Code != http.StatusForbidden {
		t.Errorf("blocked IP should get 403, got %d", w.Code)
	}

	// Verify the body contains ip_blocked error code.
	var errResp APIErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if errResp.Error.Code != errCodeIPBlocked {
		t.Errorf("expected error code %q, got %q", errCodeIPBlocked, errResp.Error.Code)
	}

	// Auth should never have been called because IP was blocked first.
	if len(authMock.Calls) != 0 {
		t.Errorf("expected 0 auth calls (IP blocked before auth), got %d", len(authMock.Calls))
	}
}

// TestMiddlewareOrder_AuthBeforeRateLimit verifies that AuthMiddleware executes
// before RateLimit. An unauthenticated request should trigger a 401 from auth
// rather than consuming a rate limit token.
func TestMiddlewareOrder_AuthBeforeRateLimit(t *testing.T) {
	cfg := &config.Config{
		Environment: "local",
		Security: config.SecurityConfig{
			CorsAllowedOrigins: []string{"*"},
		},
	}
	repos := &mockRepositoryRegistry{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	srv.SecurityService = &MockSecurityService{}

	// Auth will reject (no valid token).
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid token", nil),
	}

	// Rate limit mock tracks calls.
	rateMock := &MockRateLimitStore{
		Result: RateLimitResult{Allowed: true, Remaining: 999, ResetAt: time.Now().Add(time.Hour)},
	}
	srv.RateLimitStore = rateMock
	srv.IdempotencyStore = NewMockIdempotencyStore()

	srv.MountRoutes()

	// Make a request with an auth token that will fail.
	req := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	req.Header.Set("Authorization", "Bearer invalid_token")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should get 401 from auth.
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 from auth, got %d", w.Code)
	}

	// Rate limit should not have been called because auth failed first and
	// no Actor was injected into context.
	if len(rateMock.Calls) != 0 {
		t.Errorf("expected 0 rate limit calls (auth failed before rate limit), got %d", len(rateMock.Calls))
	}
}

// TestMiddlewareOrder_AuthBeforeCSRF verifies that CSRF validation only applies
// to authenticated session-based requests. An unauthenticated request should get
// a 401 from auth, not a 403 from CSRF.
func TestMiddlewareOrder_AuthBeforeCSRF(t *testing.T) {
	cfg := &config.Config{
		Environment: "local",
		Security: config.SecurityConfig{
			CorsAllowedOrigins: []string{"*"},
		},
	}
	repos := &mockRepositoryRegistry{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	srv.SecurityService = &MockSecurityService{}

	// Auth will reject.
	srv.Authenticator = &MockAuthenticator{
		Err: types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid", nil),
	}
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{Allowed: true, Remaining: 999, ResetAt: time.Now().Add(time.Hour)},
	}
	srv.IdempotencyStore = NewMockIdempotencyStore()

	srv.MountRoutes()

	// POST request without CSRF token - should get 401 (auth fails) not 403 (CSRF).
	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	req.Header.Set("Authorization", "Bearer bad_token")
	// Deliberately no X-CSRF-Token header.
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 (auth before CSRF), got %d", w.Code)
	}
}

// TestMiddlewareOrder_ContextTimeout verifies that ContextTimeoutMiddleware
// sets a deadline on the request context.
func TestMiddlewareOrder_ContextTimeout(t *testing.T) {
	// Use a short timeout for testing.
	mw := ContextTimeoutMiddleware(50 * time.Millisecond)

	var deadlineSet bool
	var deadline time.Time
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, deadlineSet = r.Context().Deadline()
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !deadlineSet {
		t.Error("ContextTimeoutMiddleware should set a deadline on the context")
	}
	// The deadline should be roughly 50ms from now (within a generous margin).
	if time.Until(deadline) > 100*time.Millisecond {
		t.Error("ContextTimeoutMiddleware: deadline is too far in the future")
	}
}

// TestRequestIDMiddleware_Generation verifies that RequestIDMiddleware generates
// a new ID when none is provided.
func TestRequestIDMiddleware_Generation(t *testing.T) {
	var capturedID string
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = types.GetRequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if capturedID == "" {
		t.Error("RequestIDMiddleware should generate an ID when none is provided")
	}
	if len(capturedID) != 32 {
		t.Errorf("generated ID should be 32 hex chars, got %d: %q", len(capturedID), capturedID)
	}

	// Verify response header matches.
	responseID := w.Header().Get("X-Request-Id")
	if responseID != capturedID {
		t.Errorf("response header X-Request-Id=%q doesn't match context ID=%q", responseID, capturedID)
	}
}

// TestRequestIDMiddleware_Propagation verifies that RequestIDMiddleware reuses
// an existing X-Request-Id header.
func TestRequestIDMiddleware_Propagation(t *testing.T) {
	var capturedID string
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = types.GetRequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "incoming-id-12345")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if capturedID != "incoming-id-12345" {
		t.Errorf("expected propagated ID %q, got %q", "incoming-id-12345", capturedID)
	}

	responseID := w.Header().Get("X-Request-Id")
	if responseID != "incoming-id-12345" {
		t.Errorf("response header should echo incoming ID: got %q", responseID)
	}
}

// TestXRayMiddleware_PassThrough verifies that the XRay stub middleware
// passes requests through without modification.
func TestXRayMiddleware_PassThrough(t *testing.T) {
	srv := newTestServerForRoutes(t)

	var called bool
	handler := srv.XRayMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("XRayMiddleware should pass through to the next handler")
	}
}

// TestServeOpenAPISpec verifies the OpenAPI spec endpoint returns valid JSON.
func TestServeOpenAPISpec(t *testing.T) {
	srv := newTestServerForRoutes(t)

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()

	srv.ServeOpenAPISpec(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("ServeOpenAPISpec: expected 200, got %d", w.Code)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("ServeOpenAPISpec returned invalid JSON: %v", err)
	}

	if _, ok := body["openapi"]; !ok {
		t.Error("ServeOpenAPISpec response should contain 'openapi' key")
	}
}

// TestMiddlewareOrder_RecovererCatchesPanics verifies that Recoverer is the
// outermost middleware and catches panics from any downstream handler, returning
// a 500 JSON response.
func TestMiddlewareOrder_RecovererCatchesPanics(t *testing.T) {
	cfg := &config.Config{
		Environment: "local",
		Security: config.SecurityConfig{
			CorsAllowedOrigins: []string{"*"},
		},
	}
	repos := &mockRepositoryRegistry{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	srv.SecurityService = &MockSecurityService{}
	// Auth passes and provides an actor so the request reaches the handler.
	srv.Authenticator = &MockAuthenticator{
		Actor: &types.Actor{
			ID:             "user_panic",
			Type:           types.ActorTypeAPIKey,
			OrganizationID: "org_1",
			Source:         "test",
		},
	}
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{Allowed: true, Remaining: 999, ResetAt: time.Now().Add(time.Hour)},
	}
	srv.IdempotencyStore = NewMockIdempotencyStore()

	srv.MountRoutes()

	// Register a panicking handler under /v1/panic.
	srv.Router().Get("/v1/panic", func(w http.ResponseWriter, r *http.Request) {
		panic("test panic from handler")
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/panic", nil)
	req.Header.Set("Authorization", "Bearer test_token")
	w := httptest.NewRecorder()

	// This should not panic -- Recoverer should catch it.
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 after panic, got %d", w.Code)
	}

	// Verify the body is a valid JSON error response.
	var errResp APIErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode panic response: %v", err)
	}
	if errResp.Error.Code != string(types.ErrCodeInternalUnexpected) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeInternalUnexpected, errResp.Error.Code)
	}
}

// TestMountRoutes_CORSHeaders verifies that CORS headers are set on responses.
func TestMountRoutes_CORSHeaders(t *testing.T) {
	srv := newTestServerForRoutes(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "https://app.watchpoint.io")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	acao := w.Header().Get("Access-Control-Allow-Origin")
	if acao == "" {
		t.Error("expected Access-Control-Allow-Origin header to be set")
	}
}

// TestMountRoutes_FullChainIntegration performs an end-to-end test through the
// full middleware chain with all dependencies wired. This validates that
// middleware compose correctly and don't interfere with each other.
func TestMountRoutes_FullChainIntegration(t *testing.T) {
	cfg := &config.Config{
		Environment: "local",
		Security: config.SecurityConfig{
			CorsAllowedOrigins: []string{"*"},
		},
	}
	repos := &mockRepositoryRegistry{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := NewServer(cfg, repos, logger)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	actor := &types.Actor{
		ID:             "user_integration",
		Type:           types.ActorTypeAPIKey,
		OrganizationID: "org_int",
		Source:         "test",
	}
	srv.SecurityService = &MockSecurityService{}
	srv.Authenticator = &MockAuthenticator{Actor: actor}
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{Allowed: true, Remaining: 50, ResetAt: time.Now().Add(time.Hour)},
	}
	srv.IdempotencyStore = NewMockIdempotencyStore()
	srv.Metrics = &mockMetricsCollector{}

	srv.MountRoutes()

	// Register a handler that checks context values set by middleware.
	var (
		gotRequestID string
		gotActor     types.Actor
		gotActorOK   bool
		gotDeadline  bool
	)
	srv.Router().Get("/v1/integration-test", func(w http.ResponseWriter, r *http.Request) {
		gotRequestID = types.GetRequestID(r.Context())
		gotActor, gotActorOK = types.GetActor(r.Context())
		_, gotDeadline = r.Context().Deadline()
		JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/integration-test", nil)
	req.Header.Set("Authorization", "Bearer test_token")
	req.Header.Set("Origin", "https://app.watchpoint.io")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("integration test: expected 200, got %d; body: %s", w.Code, w.Body.String())
	}

	// Verify middleware effects.
	if gotRequestID == "" {
		t.Error("RequestID middleware should inject request ID into context")
	}
	if !gotActorOK {
		t.Error("Auth middleware should inject Actor into context")
	}
	if gotActor.ID != actor.ID {
		t.Errorf("Actor ID: got %q, want %q", gotActor.ID, actor.ID)
	}
	if !gotDeadline {
		t.Error("ContextTimeout middleware should set a deadline on the context")
	}

	// Verify security headers on the response.
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("SecurityHeaders middleware should set X-Content-Type-Options")
	}
	if w.Header().Get("X-Request-Id") == "" {
		t.Error("RequestID middleware should set X-Request-Id response header")
	}

	// Verify rate limit headers.
	if w.Header().Get("X-RateLimit-Limit") == "" {
		t.Error("RateLimit middleware should set X-RateLimit-Limit header")
	}
}

// TestContextTimeoutMiddleware_Cancellation verifies that the context is
// cancelled after the timeout expires.
func TestContextTimeoutMiddleware_Cancellation(t *testing.T) {
	mw := ContextTimeoutMiddleware(10 * time.Millisecond)

	var ctxErr error
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wait for the context to be cancelled.
		select {
		case <-r.Context().Done():
			ctxErr = r.Context().Err()
		case <-time.After(1 * time.Second):
			t.Error("context was not cancelled within expected time")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if ctxErr != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", ctxErr)
	}
}

// TestMountV1_IsStub verifies that mountV1 is currently a stub that does not
// register any routes.
func TestMountV1_IsStub(t *testing.T) {
	srv := newTestServerForRoutes(t)

	// Try several potential v1 paths -- all should 404 since mountV1 is a stub.
	paths := []string{
		"/v1/watchpoints",
		"/v1/auth/login",
		"/v1/organizations",
		"/v1/forecasts",
	}

	for _, path := range paths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test_token")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: expected 404 (stub), got %d", path, w.Code)
		}
	}
}
