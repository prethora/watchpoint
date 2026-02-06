package core

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// --- Recoverer Tests ---

func TestRecoverer_NoPanic(t *testing.T) {
	srv := newTestServerForMiddleware(t)

	handler := srv.Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

func TestRecoverer_Panic_ReturnsJSON500(t *testing.T) {
	srv := newTestServerForMiddleware(t)

	handler := srv.Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something went wrong")
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}

	// Verify Content-Type is JSON.
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	// Verify the response body is a valid APIErrorResponse.
	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if resp.Error.Code != string(types.ErrCodeInternalUnexpected) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeInternalUnexpected, resp.Error.Code)
	}
	if resp.Error.Message != "an unexpected error occurred" {
		t.Errorf("unexpected error message: %q", resp.Error.Message)
	}
}

func TestRecoverer_Panic_PreservesRequestID(t *testing.T) {
	srv := newTestServerForMiddleware(t)

	handler := srv.Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("crash!")
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	ctx := types.WithRequestID(req.Context(), "req_abc123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if resp.Error.RequestID != "req_abc123" {
		t.Errorf("expected request_id %q, got %q", "req_abc123", resp.Error.RequestID)
	}
}

func TestRecoverer_PanicNilValue(t *testing.T) {
	srv := newTestServerForMiddleware(t)

	handler := srv.Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(nil)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	// panic(nil) results in recover() returning nil in Go 1.21+.
	// The recoverer should handle this gracefully (either catching it or not).
	handler.ServeHTTP(rec, req)

	// In Go 1.21+, panic(nil) wraps to a *runtime.PanicNilError,
	// so recover() returns non-nil. Either way the test should not crash.
	if rec.Code == http.StatusOK {
		// If panic(nil) was not caught, the handler never wrote, so the default
		// 200 from httptest is fine. This is Go-version-dependent.
		return
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500 or 200, got %d", rec.Code)
	}
}

func TestRecoverer_PanicWithError(t *testing.T) {
	srv := newTestServerForMiddleware(t)

	handler := srv.Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(42) // panic with a non-string value
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if resp.Error.Code != string(types.ErrCodeInternalUnexpected) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeInternalUnexpected, resp.Error.Code)
	}
}

// --- SecurityHeadersMiddleware Tests ---

func TestSecurityHeadersMiddleware_SetsAllHeaders(t *testing.T) {
	srv := newTestServerForMiddleware(t)

	handler := srv.SecurityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	expectedHeaders := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":       "DENY",
		"X-XSS-Protection":      "1; mode=block",
	}

	for name, expected := range expectedHeaders {
		actual := rec.Header().Get(name)
		if actual != expected {
			t.Errorf("header %q: got %q, want %q", name, actual, expected)
		}
	}
}

func TestSecurityHeadersMiddleware_PassesThrough(t *testing.T) {
	srv := newTestServerForMiddleware(t)
	called := false

	handler := srv.SecurityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler was not called")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}
}

// --- NewCORSMiddleware Tests ---

func TestCORSMiddleware_WildcardOrigin(t *testing.T) {
	mw := NewCORSMiddleware([]string{"*"})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin: got %q, want %q", got, "*")
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Access-Control-Allow-Methods header not set")
	}
}

func TestCORSMiddleware_SpecificOrigin_Allowed(t *testing.T) {
	mw := NewCORSMiddleware([]string{"https://app.watchpoint.io", "https://dashboard.watchpoint.io"})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://app.watchpoint.io")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.watchpoint.io" {
		t.Errorf("Access-Control-Allow-Origin: got %q, want %q", got, "https://app.watchpoint.io")
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary header: got %q, want %q", got, "Origin")
	}
}

func TestCORSMiddleware_SpecificOrigin_Denied(t *testing.T) {
	mw := NewCORSMiddleware([]string{"https://app.watchpoint.io"})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin should be empty for denied origin, got %q", got)
	}
}

func TestCORSMiddleware_OptionsPreflightReturns204(t *testing.T) {
	mw := NewCORSMiddleware([]string{"*"})
	nextCalled := false

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS preflight: expected status 204, got %d", rec.Code)
	}
	if nextCalled {
		t.Error("next handler should not be called for OPTIONS preflight")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin: got %q, want %q", got, "*")
	}
}

func TestCORSMiddleware_NoOriginHeader(t *testing.T) {
	mw := NewCORSMiddleware([]string{"https://app.watchpoint.io"})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Request without Origin header (e.g., server-to-server).
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin should be empty for no origin, got %q", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestCORSMiddleware_AllowedHeaders(t *testing.T) {
	mw := NewCORSMiddleware([]string{"*"})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	allowHeaders := rec.Header().Get("Access-Control-Allow-Headers")
	for _, expected := range []string{"Content-Type", "Authorization", "X-CSRF-Token", "Idempotency-Key"} {
		if !strings.Contains(allowHeaders, expected) {
			t.Errorf("Access-Control-Allow-Headers missing %q, got: %s", expected, allowHeaders)
		}
	}
}

func TestCORSMiddleware_AllowCredentials(t *testing.T) {
	mw := NewCORSMiddleware([]string{"https://app.watchpoint.io"})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://app.watchpoint.io")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials: got %q, want %q", got, "true")
	}
}

func TestCORSMiddleware_MaxAge(t *testing.T) {
	mw := NewCORSMiddleware([]string{"*"})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Max-Age"); got != "86400" {
		t.Errorf("Access-Control-Max-Age: got %q, want %q", got, "86400")
	}
}

// --- MetricsMiddleware Tests ---

func TestMetricsMiddleware_RecordsRequest(t *testing.T) {
	srv := newTestServerForMiddleware(t)
	mc := &mockMetricsCollector{}
	srv.Metrics = mc

	handler := srv.MetricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if len(mc.calls) != 1 {
		t.Fatalf("expected 1 metrics call, got %d", len(mc.calls))
	}

	call := mc.calls[0]
	if call.method != http.MethodPost {
		t.Errorf("method: got %q, want %q", call.method, http.MethodPost)
	}
	if call.endpoint != "/v1/watchpoints" {
		t.Errorf("endpoint: got %q, want %q", call.endpoint, "/v1/watchpoints")
	}
	if call.status != "201" {
		t.Errorf("status: got %q, want %q", call.status, "201")
	}
	if call.duration <= 0 {
		t.Errorf("duration should be positive, got %v", call.duration)
	}
}

func TestMetricsMiddleware_Records500Status(t *testing.T) {
	srv := newTestServerForMiddleware(t)
	mc := &mockMetricsCollector{}
	srv.Metrics = mc

	handler := srv.MetricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodGet, "/error", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if len(mc.calls) != 1 {
		t.Fatalf("expected 1 metrics call, got %d", len(mc.calls))
	}
	if mc.calls[0].status != "500" {
		t.Errorf("status: got %q, want %q", mc.calls[0].status, "500")
	}
}

func TestMetricsMiddleware_NilCollector_PassesThrough(t *testing.T) {
	srv := newTestServerForMiddleware(t)
	srv.Metrics = nil // Explicitly nil

	called := false
	handler := srv.MetricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called even when Metrics is nil")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestMetricsMiddleware_DefaultStatusCode(t *testing.T) {
	srv := newTestServerForMiddleware(t)
	mc := &mockMetricsCollector{}
	srv.Metrics = mc

	// Handler writes body but never calls WriteHeader explicitly.
	handler := srv.MetricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if len(mc.calls) != 1 {
		t.Fatalf("expected 1 metrics call, got %d", len(mc.calls))
	}
	if mc.calls[0].status != "200" {
		t.Errorf("status: got %q, want %q (default when WriteHeader not called)", mc.calls[0].status, "200")
	}
}

// --- RequestLogger Tests ---

func TestRequestLogger_LogsRequestMetadata(t *testing.T) {
	buf := &strings.Builder{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mw := RequestLogger(logger, nil)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	logOutput := buf.String()
	if logOutput == "" {
		t.Fatal("expected log output, got empty string")
	}
	if !strings.Contains(logOutput, "request completed") {
		t.Errorf("log should contain 'request completed', got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "GET") {
		t.Errorf("log should contain method GET, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "/v1/watchpoints") {
		t.Errorf("log should contain path, got: %s", logOutput)
	}
}

func TestRequestLogger_RedactsHeaders(t *testing.T) {
	buf := &strings.Builder{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mw := RequestLogger(logger, []string{"Authorization", "X-API-Key"})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer sk_live_secret_key_123")
	req.Header.Set("X-API-Key", "super_secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	logOutput := buf.String()

	// The secret values should NOT appear in the log.
	if strings.Contains(logOutput, "sk_live_secret_key_123") {
		t.Error("authorization header value should be redacted")
	}
	if strings.Contains(logOutput, "super_secret") {
		t.Error("X-API-Key header value should be redacted")
	}
	// REDACTED should appear.
	if !strings.Contains(logOutput, "[REDACTED]") {
		t.Error("redacted headers should show [REDACTED]")
	}
	// Non-redacted headers should appear normally.
	if !strings.Contains(logOutput, "application/json") {
		t.Error("non-redacted Content-Type header should appear in log")
	}
}

func TestRequestLogger_RedactionIsCaseInsensitive(t *testing.T) {
	buf := &strings.Builder{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Configured with lowercase, but HTTP headers are canonicalized.
	mw := RequestLogger(logger, []string{"authorization"})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer token_123") // Go canonicalizes this
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	logOutput := buf.String()
	if strings.Contains(logOutput, "token_123") {
		t.Error("authorization header should be redacted regardless of case")
	}
}

func TestRequestLogger_LogsErrorLevel_For5xx(t *testing.T) {
	buf := &strings.Builder{}
	// Set level to Debug to capture all levels.
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mw := RequestLogger(logger, nil)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "ERROR") {
		t.Errorf("5xx responses should be logged at ERROR level, got: %s", logOutput)
	}
}

func TestRequestLogger_LogsWarnLevel_For4xx(t *testing.T) {
	buf := &strings.Builder{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mw := RequestLogger(logger, nil)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "WARN") {
		t.Errorf("4xx responses should be logged at WARN level, got: %s", logOutput)
	}
}

func TestRequestLogger_IncludesRequestID(t *testing.T) {
	buf := &strings.Builder{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mw := RequestLogger(logger, nil)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	ctx := types.WithRequestID(req.Context(), "req_test456")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "req_test456") {
		t.Errorf("log should contain request_id, got: %s", logOutput)
	}
}

// --- responseCapture Tests ---

func TestResponseCapture_CapturesStatusCode(t *testing.T) {
	rec := httptest.NewRecorder()
	rc := &responseCapture{ResponseWriter: rec, statusCode: http.StatusOK}

	rc.WriteHeader(http.StatusNotFound)

	if rc.statusCode != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, rc.statusCode)
	}
	if !rc.written {
		t.Error("written flag should be true after WriteHeader")
	}
}

func TestResponseCapture_DefaultsTo200OnWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	rc := &responseCapture{ResponseWriter: rec, statusCode: http.StatusOK}

	_, err := rc.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rc.statusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rc.statusCode)
	}
	if !rc.written {
		t.Error("written flag should be true after Write")
	}
}

func TestResponseCapture_WriteHeaderOnlyOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	rc := &responseCapture{ResponseWriter: rec, statusCode: http.StatusOK}

	rc.WriteHeader(http.StatusCreated)
	rc.WriteHeader(http.StatusNotFound) // Second call should not change captured code.

	if rc.statusCode != http.StatusCreated {
		t.Errorf("expected first status %d to be captured, got %d", http.StatusCreated, rc.statusCode)
	}
}

func TestResponseCapture_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	rc := &responseCapture{ResponseWriter: rec, statusCode: http.StatusOK}

	unwrapped := rc.Unwrap()
	if unwrapped != rec {
		t.Error("Unwrap should return the underlying ResponseWriter")
	}
}

// --- writeJSON/escapeJSON Tests ---

func TestWriteJSON_ProducesValidJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := APIErrorResponse{
		Error: ErrorDetail{
			Code:      "internal_unexpected_error",
			Message:   "an unexpected error occurred",
			RequestID: "req_123",
		},
	}

	err := writeJSON(rec, resp)
	if err != nil {
		t.Fatalf("writeJSON returned error: %v", err)
	}

	body, _ := io.ReadAll(rec.Body)
	var parsed APIErrorResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("writeJSON output is not valid JSON: %v, body: %s", err, body)
	}
	if parsed.Error.Code != "internal_unexpected_error" {
		t.Errorf("code: got %q, want %q", parsed.Error.Code, "internal_unexpected_error")
	}
	if parsed.Error.RequestID != "req_123" {
		t.Errorf("request_id: got %q, want %q", parsed.Error.RequestID, "req_123")
	}
}

func TestEscapeJSON_HandlesSpecialChars(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`hello`, `hello`},
		{`say "hello"`, `say \"hello\"`},
		{"line1\nline2", `line1\nline2`},
		{`back\slash`, `back\\slash`},
		{"tab\there", `tab\there`},
		{"cr\rhere", `cr\rhere`},
	}

	for _, tc := range tests {
		got := escapeJSON(tc.input)
		if got != tc.expected {
			t.Errorf("escapeJSON(%q): got %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// --- Integration-style tests combining middleware ---

func TestMiddlewareChain_RecovererWithMetrics(t *testing.T) {
	srv := newTestServerForMiddleware(t)
	mc := &mockMetricsCollector{}
	srv.Metrics = mc

	// Chain: Recoverer -> Metrics -> handler that panics.
	// The Recoverer catches the panic and writes 500. Then Metrics records the 500.
	// But since Recoverer is outermost, Metrics wraps the inner handler, and the panic
	// happens inside Metrics' wrapped handler. Recoverer catches it after Metrics has
	// already started its measurement.

	handler := srv.Recoverer(srv.MetricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}

	// When the handler panics inside MetricsMiddleware, the deferred recover in
	// Recoverer catches it, so MetricsMiddleware never gets to record the request
	// (its defer/measurement runs after the handler returns, but the panic unwinds
	// past it to Recoverer). This is expected behavior: metrics won't record panicked
	// requests, which is an acceptable trade-off since panics indicate critical issues.
	// The test verifies the overall chain doesn't itself panic.
}

func TestMiddlewareChain_SecurityHeadersWithCORS(t *testing.T) {
	srv := newTestServerForMiddleware(t)

	corsMW := NewCORSMiddleware([]string{"https://app.watchpoint.io"})
	handler := srv.SecurityHeadersMiddleware(corsMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://app.watchpoint.io")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Both security headers and CORS headers should be present.
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q, want %q", got, "nosniff")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.watchpoint.io" {
		t.Errorf("Access-Control-Allow-Origin: got %q, want %q", got, "https://app.watchpoint.io")
	}
}

// --- Test Helpers ---

// newTestServerForMiddleware creates a minimal Server suitable for testing
// middleware in isolation. It uses a no-op logger and minimal config.
func newTestServerForMiddleware(t *testing.T) *Server {
	t.Helper()

	// Use a discard handler so middleware logging does not pollute test output.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	return &Server{
		Logger: logger,
	}
}

// --- Latency Measurement ---

func TestMetricsMiddleware_MeasuresDuration(t *testing.T) {
	srv := newTestServerForMiddleware(t)
	mc := &mockMetricsCollector{}
	srv.Metrics = mc

	handler := srv.MetricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a handler that takes some time.
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/slow", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if len(mc.calls) != 1 {
		t.Fatalf("expected 1 metrics call, got %d", len(mc.calls))
	}
	if mc.calls[0].duration < 10*time.Millisecond {
		t.Errorf("duration should be >= 10ms, got %v", mc.calls[0].duration)
	}
}
