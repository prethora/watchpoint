package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock dependencies
// ---------------------------------------------------------------------------

// mockHTTPClient implements HTTPClient for testing. It returns a configurable
// response or error without making real HTTP calls.
type mockHTTPClient struct {
	doFunc func(req *http.Request) (*http.Response, error)
	// calls records all requests for assertion.
	calls []*http.Request
}

func (m *mockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	m.calls = append(m.calls, req)
	if m.doFunc != nil {
		return m.doFunc(req)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
	}, nil
}

// mockDBConnector implements DatabaseConnector for testing.
type mockDBConnector struct {
	connectFn func(ctx context.Context, dsn string) error
	// calls records all DSNs passed to Connect.
	calls []string
}

func (m *mockDBConnector) Connect(ctx context.Context, dsn string) error {
	m.calls = append(m.calls, dsn)
	if m.connectFn != nil {
		return m.connectFn(ctx, dsn)
	}
	return nil
}

// newTestValidator creates a Validator with mock dependencies.
func newTestValidator(httpClient *mockHTTPClient, dbConn *mockDBConnector) *Validator {
	return NewValidatorWithDeps(httpClient, dbConn)
}

// mockHTTPResponse creates a simple HTTP response with the given status and body.
func mockHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
}

// ---------------------------------------------------------------------------
// ValidateDatabaseURL tests
// ---------------------------------------------------------------------------

func TestValidateDatabaseURL_Success(t *testing.T) {
	dbConn := &mockDBConnector{}
	v := newTestValidator(&mockHTTPClient{}, dbConn)

	result := v.ValidateDatabaseURL(context.Background(), "postgres://user:pass@db.example.com:6543/mydb")
	if !result.Valid {
		t.Fatalf("expected valid, got: %s", result.Message)
	}
	if !strings.Contains(result.Message, "database connection verified") {
		t.Errorf("unexpected message: %s", result.Message)
	}
	if !strings.Contains(result.Message, "port=6543") {
		t.Errorf("message should mention port: %s", result.Message)
	}

	// Verify the connector was called with the correct DSN.
	if len(dbConn.calls) != 1 {
		t.Fatalf("expected 1 Connect call, got %d", len(dbConn.calls))
	}
	if dbConn.calls[0] != "postgres://user:pass@db.example.com:6543/mydb" {
		t.Errorf("Connect DSN = %q", dbConn.calls[0])
	}
}

func TestValidateDatabaseURL_PostgreSQLScheme(t *testing.T) {
	dbConn := &mockDBConnector{}
	v := newTestValidator(&mockHTTPClient{}, dbConn)

	result := v.ValidateDatabaseURL(context.Background(), "postgresql://user:pass@db.example.com:6543/mydb")
	if !result.Valid {
		t.Fatalf("expected valid for postgresql:// scheme, got: %s", result.Message)
	}
}

func TestValidateDatabaseURL_Empty(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateDatabaseURL(context.Background(), "")
	if result.Valid {
		t.Fatal("expected invalid for empty URL")
	}
	if !strings.Contains(result.Message, "must not be empty") {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestValidateDatabaseURL_WhitespaceOnly(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateDatabaseURL(context.Background(), "   ")
	if result.Valid {
		t.Fatal("expected invalid for whitespace-only URL")
	}
}

func TestValidateDatabaseURL_WrongScheme(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateDatabaseURL(context.Background(), "mysql://user:pass@host:6543/db")
	if result.Valid {
		t.Fatal("expected invalid for mysql scheme")
	}
	if !strings.Contains(result.Message, "postgres://") {
		t.Errorf("message should mention expected scheme: %s", result.Message)
	}
}

func TestValidateDatabaseURL_WrongPort(t *testing.T) {
	tests := []struct {
		name string
		url  string
		port string
	}{
		{"standard postgres port", "postgres://user:pass@host:5432/db", "5432"},
		{"random port", "postgres://user:pass@host:3306/db", "3306"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

			result := v.ValidateDatabaseURL(context.Background(), tt.url)
			if result.Valid {
				t.Fatal("expected invalid for wrong port")
			}
			if !strings.Contains(result.Message, "6543") {
				t.Errorf("message should mention required port 6543: %s", result.Message)
			}
			if !strings.Contains(result.Message, tt.port) {
				t.Errorf("message should mention actual port %s: %s", tt.port, result.Message)
			}
		})
	}
}

func TestValidateDatabaseURL_NoPort(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateDatabaseURL(context.Background(), "postgres://user:pass@host/db")
	if result.Valid {
		t.Fatal("expected invalid when no port specified")
	}
	if !strings.Contains(result.Message, "6543") {
		t.Errorf("message should mention required port: %s", result.Message)
	}
}

func TestValidateDatabaseURL_ConnectionFails(t *testing.T) {
	dbConn := &mockDBConnector{
		connectFn: func(_ context.Context, _ string) error {
			return fmt.Errorf("connection refused")
		},
	}
	v := newTestValidator(&mockHTTPClient{}, dbConn)

	result := v.ValidateDatabaseURL(context.Background(), "postgres://user:pass@host:6543/db")
	if result.Valid {
		t.Fatal("expected invalid when connection fails")
	}
	if !strings.Contains(result.Message, "connection failed") {
		t.Errorf("message should indicate connection failure: %s", result.Message)
	}
	if !strings.Contains(result.Message, "connection refused") {
		t.Errorf("message should include underlying error: %s", result.Message)
	}
}

func TestValidateDatabaseURL_TrimsWhitespace(t *testing.T) {
	dbConn := &mockDBConnector{}
	v := newTestValidator(&mockHTTPClient{}, dbConn)

	result := v.ValidateDatabaseURL(context.Background(), "  postgres://user:pass@host:6543/db  ")
	if !result.Valid {
		t.Fatalf("expected valid after trimming whitespace, got: %s", result.Message)
	}
}

func TestValidateDatabaseURL_ContextCancelled(t *testing.T) {
	dbConn := &mockDBConnector{
		connectFn: func(ctx context.Context, _ string) error {
			return ctx.Err()
		},
	}
	v := newTestValidator(&mockHTTPClient{}, dbConn)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := v.ValidateDatabaseURL(ctx, "postgres://user:pass@host:6543/db")
	if result.Valid {
		t.Fatal("expected invalid when context is cancelled")
	}
}

// ---------------------------------------------------------------------------
// ValidateStripeKey tests
// ---------------------------------------------------------------------------

func TestValidateStripeKey_Success_TestMode(t *testing.T) {
	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return mockHTTPResponse(http.StatusOK, `{"id":"acct_123","business_profile":{"name":"Test Corp"}}`), nil
		},
	}
	v := newTestValidator(httpClient, &mockDBConnector{})

	result := v.ValidateStripeKey(context.Background(), "sk_test_abcdefghijklmnopqrstuvwx")
	if !result.Valid {
		t.Fatalf("expected valid, got: %s", result.Message)
	}
	if !strings.Contains(result.Message, "test mode") {
		t.Errorf("message should mention test mode: %s", result.Message)
	}

	// Verify the request was sent with correct auth.
	if len(httpClient.calls) != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", len(httpClient.calls))
	}
	req := httpClient.calls[0]
	if req.URL.String() != "https://api.stripe.com/v1/account" {
		t.Errorf("URL = %q", req.URL.String())
	}
	authHeader := req.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer sk_test_") {
		t.Errorf("Authorization header = %q", authHeader)
	}
}

func TestValidateStripeKey_Success_LiveMode(t *testing.T) {
	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return mockHTTPResponse(http.StatusOK, `{"id":"acct_456"}`), nil
		},
	}
	v := newTestValidator(httpClient, &mockDBConnector{})

	result := v.ValidateStripeKey(context.Background(), "sk_live_abcdefghijklmnopqrstuvwx")
	if !result.Valid {
		t.Fatalf("expected valid, got: %s", result.Message)
	}
	if !strings.Contains(result.Message, "live mode") {
		t.Errorf("message should mention live mode: %s", result.Message)
	}
}

func TestValidateStripeKey_Empty(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateStripeKey(context.Background(), "")
	if result.Valid {
		t.Fatal("expected invalid for empty key")
	}
	if !strings.Contains(result.Message, "must not be empty") {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestValidateStripeKey_InvalidFormat(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"no prefix", "abcdefghijklmnopqrstuvwxyz1234"},
		{"wrong prefix", "pk_test_abcdefghijklmnopqrstuvwx"},
		{"too short", "sk_test_abc"},
		{"missing mode", "sk_abcdefghijklmnopqrstuvwxyz1234"},
		{"invalid chars", "sk_test_abcdefghijklmnopq!@#$%"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

			result := v.ValidateStripeKey(context.Background(), tt.key)
			if result.Valid {
				t.Fatal("expected invalid for bad format")
			}
			if !strings.Contains(result.Message, "format") {
				t.Errorf("message should mention format: %s", result.Message)
			}
		})
	}
}

func TestValidateStripeKey_Unauthorized(t *testing.T) {
	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return mockHTTPResponse(http.StatusUnauthorized, `{"error":{"message":"Invalid API Key provided"}}`), nil
		},
	}
	v := newTestValidator(httpClient, &mockDBConnector{})

	result := v.ValidateStripeKey(context.Background(), "sk_test_abcdefghijklmnopqrstuvwx")
	if result.Valid {
		t.Fatal("expected invalid for 401 response")
	}
	if !strings.Contains(result.Message, "401") {
		t.Errorf("message should mention 401: %s", result.Message)
	}
	if !strings.Contains(result.Message, "invalid or revoked") {
		t.Errorf("message should explain failure: %s", result.Message)
	}
}

func TestValidateStripeKey_ServerError(t *testing.T) {
	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return mockHTTPResponse(http.StatusInternalServerError, `{"error":"internal"}`), nil
		},
	}
	v := newTestValidator(httpClient, &mockDBConnector{})

	result := v.ValidateStripeKey(context.Background(), "sk_test_abcdefghijklmnopqrstuvwx")
	if result.Valid {
		t.Fatal("expected invalid for 500 response")
	}
	if !strings.Contains(result.Message, "500") {
		t.Errorf("message should mention status code: %s", result.Message)
	}
}

func TestValidateStripeKey_NetworkError(t *testing.T) {
	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial tcp: connection refused")
		},
	}
	v := newTestValidator(httpClient, &mockDBConnector{})

	result := v.ValidateStripeKey(context.Background(), "sk_test_abcdefghijklmnopqrstuvwx")
	if result.Valid {
		t.Fatal("expected invalid for network error")
	}
	if !strings.Contains(result.Message, "probe failed") {
		t.Errorf("message should mention probe failure: %s", result.Message)
	}
}

func TestValidateStripeKey_TrimsWhitespace(t *testing.T) {
	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return mockHTTPResponse(http.StatusOK, `{"id":"acct_123"}`), nil
		},
	}
	v := newTestValidator(httpClient, &mockDBConnector{})

	result := v.ValidateStripeKey(context.Background(), "  sk_test_abcdefghijklmnopqrstuvwx  ")
	if !result.Valid {
		t.Fatalf("expected valid after trimming, got: %s", result.Message)
	}
}

// ---------------------------------------------------------------------------
// ValidateRunPodKey tests
// ---------------------------------------------------------------------------

func TestValidateRunPodKey_Success(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRunPodKey(context.Background(), "abcdefghijklmnopqrstu") // 21 chars
	if !result.Valid {
		t.Fatalf("expected valid for 21-char key, got: %s", result.Message)
	}
	if !strings.Contains(result.Message, "21") {
		t.Errorf("message should mention length: %s", result.Message)
	}
}

func TestValidateRunPodKey_LongKey(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	key := strings.Repeat("a", 100)
	result := v.ValidateRunPodKey(context.Background(), key)
	if !result.Valid {
		t.Fatalf("expected valid for 100-char key, got: %s", result.Message)
	}
}

func TestValidateRunPodKey_Empty(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRunPodKey(context.Background(), "")
	if result.Valid {
		t.Fatal("expected invalid for empty key")
	}
}

func TestValidateRunPodKey_TooShort(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"exactly 20 chars", "12345678901234567890"},
		{"1 char", "a"},
		{"19 chars", "1234567890123456789"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

			result := v.ValidateRunPodKey(context.Background(), tt.key)
			if result.Valid {
				t.Fatalf("expected invalid for key of length %d", len(tt.key))
			}
			if !strings.Contains(result.Message, "longer than 20") {
				t.Errorf("message should mention minimum length: %s", result.Message)
			}
		})
	}
}

func TestValidateRunPodKey_ExactlyBoundary(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	// 20 chars should fail (must be >20, not >=20)
	key20 := strings.Repeat("a", 20)
	result := v.ValidateRunPodKey(context.Background(), key20)
	if result.Valid {
		t.Fatal("expected invalid for exactly 20 chars (must be >20)")
	}

	// 21 chars should pass
	key21 := strings.Repeat("a", 21)
	result = v.ValidateRunPodKey(context.Background(), key21)
	if !result.Valid {
		t.Fatalf("expected valid for 21 chars, got: %s", result.Message)
	}
}

func TestValidateRunPodKey_TrimsWhitespace(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRunPodKey(context.Background(), "  "+strings.Repeat("a", 21)+"  ")
	if !result.Valid {
		t.Fatalf("expected valid after trimming, got: %s", result.Message)
	}
}

// ---------------------------------------------------------------------------
// ValidateRegex tests
// ---------------------------------------------------------------------------

func TestValidateRegex_Success(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	// Google Client ID pattern (numeric with .apps.googleusercontent.com suffix)
	result := v.ValidateRegex(context.Background(), "123456789-abc.apps.googleusercontent.com", `^[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com$`, "Google Client ID")
	if !result.Valid {
		t.Fatalf("expected valid, got: %s", result.Message)
	}
	if !strings.Contains(result.Message, "Google Client ID") {
		t.Errorf("message should mention field name: %s", result.Message)
	}
}

func TestValidateRegex_Empty(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRegex(context.Background(), "", `.*`, "test field")
	if result.Valid {
		t.Fatal("expected invalid for empty input")
	}
	if !strings.Contains(result.Message, "test field") {
		t.Errorf("message should mention field name: %s", result.Message)
	}
}

func TestValidateRegex_NoMatch(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRegex(context.Background(), "not-a-github-id", `^[0-9a-f]{20}$`, "GitHub Client ID")
	if result.Valid {
		t.Fatal("expected invalid when regex doesn't match")
	}
	if !strings.Contains(result.Message, "GitHub Client ID") {
		t.Errorf("message should mention field name: %s", result.Message)
	}
	if !strings.Contains(result.Message, "format") {
		t.Errorf("message should mention format: %s", result.Message)
	}
}

func TestValidateRegex_InvalidPattern(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRegex(context.Background(), "some-input", `[invalid`, "test field")
	if result.Valid {
		t.Fatal("expected invalid for bad regex pattern")
	}
	if !strings.Contains(result.Message, "invalid regex") {
		t.Errorf("message should mention invalid regex: %s", result.Message)
	}
}

func TestValidateRegex_SimplePatterns(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		pattern string
		valid   bool
	}{
		{"hex string match", "abcdef1234567890abcd", `^[0-9a-f]{20}$`, true},
		{"hex string too short", "abcdef", `^[0-9a-f]{20}$`, false},
		{"any non-empty", "hello", `.+`, true},
		{"numeric only", "12345", `^[0-9]+$`, true},
		{"numeric only fails", "abc", `^[0-9]+$`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

			result := v.ValidateRegex(context.Background(), tt.input, tt.pattern, "test field")
			if result.Valid != tt.valid {
				t.Errorf("expected valid=%v, got valid=%v: %s", tt.valid, result.Valid, result.Message)
			}
		})
	}
}

func TestValidateRegex_TrimsWhitespace(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRegex(context.Background(), "  12345  ", `^[0-9]+$`, "test")
	if !result.Valid {
		t.Fatalf("expected valid after trimming, got: %s", result.Message)
	}
}

// ---------------------------------------------------------------------------
// ValidateRunPodEndpointID tests
// ---------------------------------------------------------------------------

func TestValidateRunPodEndpointID_Success(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRunPodEndpointID(context.Background(), "vllm-abc123xyz")
	if !result.Valid {
		t.Fatalf("expected valid, got: %s", result.Message)
	}
	if !strings.Contains(result.Message, "vllm-abc123xyz") {
		t.Errorf("message should echo the ID: %s", result.Message)
	}
}

func TestValidateRunPodEndpointID_Empty(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRunPodEndpointID(context.Background(), "")
	if result.Valid {
		t.Fatal("expected invalid for empty ID")
	}
}

func TestValidateRunPodEndpointID_TooShort(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRunPodEndpointID(context.Background(), "abc123")
	if result.Valid {
		t.Fatal("expected invalid for ID shorter than 10 chars")
	}
}

func TestValidateRunPodEndpointID_InvalidChars(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRunPodEndpointID(context.Background(), "vllm_abc$123xyz")
	if result.Valid {
		t.Fatal("expected invalid for ID with special chars")
	}
}

func TestValidateRunPodEndpointID_ExactlyTenChars(t *testing.T) {
	v := newTestValidator(&mockHTTPClient{}, &mockDBConnector{})

	result := v.ValidateRunPodEndpointID(context.Background(), "abcdefghij") // exactly 10
	if !result.Valid {
		t.Fatalf("expected valid for exactly 10 chars, got: %s", result.Message)
	}
}

// ---------------------------------------------------------------------------
// NewValidator tests
// ---------------------------------------------------------------------------

func TestNewValidator(t *testing.T) {
	v := NewValidator()
	if v == nil {
		t.Fatal("NewValidator returned nil")
	}
	if v.httpClient == nil {
		t.Error("httpClient should not be nil")
	}
	if v.dbConn == nil {
		t.Error("dbConn should not be nil")
	}
}

func TestNewValidatorWithDeps(t *testing.T) {
	httpClient := &mockHTTPClient{}
	dbConn := &mockDBConnector{}
	v := NewValidatorWithDeps(httpClient, dbConn)
	if v == nil {
		t.Fatal("NewValidatorWithDeps returned nil")
	}
	if v.httpClient != httpClient {
		t.Error("httpClient not set correctly")
	}
	if v.dbConn != dbConn {
		t.Error("dbConn not set correctly")
	}
}

// ---------------------------------------------------------------------------
// truncateBody tests
// ---------------------------------------------------------------------------

func TestTruncateBody(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		limit    int
		expected string
	}{
		{"short body", "hello", 10, "hello"},
		{"exact limit", "hello", 5, "hello"},
		{"truncated", "hello world", 5, "hello..."},
		{"empty", "", 10, ""},
		{"zero limit", "hello", 0, "..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateBody([]byte(tt.body), tt.limit)
			if got != tt.expected {
				t.Errorf("truncateBody(%q, %d) = %q, want %q", tt.body, tt.limit, got, tt.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Stripe key regex tests
// ---------------------------------------------------------------------------

func TestStripeKeyRegex(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		match bool
	}{
		{"valid test key", "sk_test_abcdefghijklmnopqrstuvwx", true},
		{"valid live key", "sk_live_abcdefghijklmnopqrstuvwx", true},
		{"valid long key", "sk_test_abcdefghijklmnopqrstuvwxyz0123456789", true},
		{"exactly 24 after prefix", "sk_test_123456789012345678901234", true},
		{"too short after prefix", "sk_test_12345678901234567890123", false}, // 23 chars
		{"wrong prefix pk", "pk_test_abcdefghijklmnopqrstuvwx", false},
		{"no mode", "sk_abcdefghijklmnopqrstuvwxyz", false},
		{"wrong mode", "sk_staging_abcdefghijklmnopqrstuvwx", false},
		{"empty", "", false},
		{"special chars", "sk_test_abcdef!@#$%^&*()_+-=[]", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripeKeyRegex.MatchString(tt.key)
			if got != tt.match {
				t.Errorf("stripeKeyRegex.MatchString(%q) = %v, want %v", tt.key, got, tt.match)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RunPod endpoint regex tests
// ---------------------------------------------------------------------------

func TestRunPodEndpointRegex(t *testing.T) {
	tests := []struct {
		name  string
		id    string
		match bool
	}{
		{"valid alphanumeric", "abcdefghij", true},
		{"valid with hyphens", "vllm-abc123", true},
		{"valid long ID", "my-runpod-endpoint-id-12345", true},
		{"too short", "abc123789", false}, // 9 chars
		{"special chars", "abc!@#defgh", false},
		{"with underscore", "abc_defghij", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runPodEndpointRegex.MatchString(tt.id)
			if got != tt.match {
				t.Errorf("runPodEndpointRegex.MatchString(%q) = %v, want %v", tt.id, got, tt.match)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidationResult tests
// ---------------------------------------------------------------------------

func TestValidationResult_Fields(t *testing.T) {
	// Ensure the struct fields are accessible and correct.
	r := ValidationResult{
		Valid:   true,
		Message: "all good",
	}
	if !r.Valid {
		t.Error("Valid should be true")
	}
	if r.Message != "all good" {
		t.Errorf("Message = %q, want %q", r.Message, "all good")
	}
}

// ---------------------------------------------------------------------------
// Integration-style tests (verifying validator combinations)
// ---------------------------------------------------------------------------

func TestValidatorEndToEnd_AllValidatorsAccessible(t *testing.T) {
	// Verify all validator methods exist and can be called on a single
	// Validator instance. This test ensures the API surface is stable.
	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return mockHTTPResponse(http.StatusOK, `{"remain":100,"id":"acct_123"}`), nil
		},
	}
	dbConn := &mockDBConnector{}
	v := NewValidatorWithDeps(httpClient, dbConn)
	ctx := context.Background()

	// Each call should complete without panic.
	v.ValidateDatabaseURL(ctx, "postgres://u:p@h:6543/db")
	v.ValidateStripeKey(ctx, "sk_test_abcdefghijklmnopqrstuvwx")
	v.ValidateRunPodKey(ctx, strings.Repeat("a", 21))
	v.ValidateRegex(ctx, "input", `.+`, "field")
	v.ValidateRunPodEndpointID(ctx, "abcdefghij")
}

// ---------------------------------------------------------------------------
// Response body handling
// ---------------------------------------------------------------------------

func TestValidateStripeKey_LargeResponseBody(t *testing.T) {
	// Ensure we don't read unbounded response bodies.
	largeBody := strings.Repeat("x", 100000)
	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader([]byte(largeBody))),
				Header:     http.Header{},
			}, nil
		},
	}
	v := newTestValidator(httpClient, &mockDBConnector{})

	// Should still succeed — the body is limited to 4096 bytes internally.
	result := v.ValidateStripeKey(context.Background(), "sk_test_abcdefghijklmnopqrstuvwx")
	if !result.Valid {
		t.Fatalf("expected valid even with large response body, got: %s", result.Message)
	}
}
