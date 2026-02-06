package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"watchpoint/internal/config"
	"watchpoint/internal/notifications/core"
	"watchpoint/internal/security"
	"watchpoint/internal/types"
)

// --- Test Helpers ---

// mockLogger is a no-op logger for testing.
type mockLogger struct{}

func (m *mockLogger) Info(msg string, args ...any)  {}
func (m *mockLogger) Error(msg string, args ...any) {}
func (m *mockLogger) Warn(msg string, args ...any)  {}
func (m *mockLogger) With(args ...any) types.Logger { return m }

// mockClock provides a controllable clock for testing.
type mockClock struct {
	now time.Time
}

func (m *mockClock) Now() time.Time { return m.now }

// mockSigner is a test double for core.SignatureManager.
type mockSigner struct {
	signResult  string
	signErr     error
	verifyCalls int
}

func (m *mockSigner) SignPayload(payload []byte, secretConfig map[string]any, now time.Time) (string, error) {
	return m.signResult, m.signErr
}

func (m *mockSigner) VerifySignature(payload []byte, header string, secrets map[string]string) bool {
	m.verifyCalls++
	return true
}

var _ core.SignatureManager = (*mockSigner)(nil)

// testWebhookConfig returns a default config for testing.
func testWebhookConfig() *config.WebhookConfig {
	return &config.WebhookConfig{
		UserAgent:      "WatchPoint-Test/1.0",
		DefaultTimeout: 10 * time.Second,
		MaxRedirects:   3,
	}
}

// newTestChannel creates a WebhookChannel backed by the given httptest.Server.
func newTestChannel(server *httptest.Server) *WebhookChannel {
	ch := NewWebhookChannelWithClient(
		testWebhookConfig(),
		&mockSigner{signResult: "t=12345,v1=abc"},
		server.Client(),
		&mockLogger{},
	)
	return ch
}

// --- Constructor Tests ---

func TestNewWebhookChannel_NilConfig(t *testing.T) {
	_, err := NewWebhookChannel(nil, &mockSigner{}, &mockLogger{})
	if err == nil {
		t.Fatal("expected error for nil config")
	}
	if !strings.Contains(err.Error(), "config is nil") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewWebhookChannel_NilSigner(t *testing.T) {
	_, err := NewWebhookChannel(testWebhookConfig(), nil, &mockLogger{})
	if err == nil {
		t.Fatal("expected error for nil signer")
	}
	if !strings.Contains(err.Error(), "signer is nil") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewWebhookChannel_NilLogger(t *testing.T) {
	_, err := NewWebhookChannel(testWebhookConfig(), &mockSigner{}, nil)
	if err == nil {
		t.Fatal("expected error for nil logger")
	}
	if !strings.Contains(err.Error(), "logger is nil") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewWebhookChannel_Success(t *testing.T) {
	ch, err := NewWebhookChannel(testWebhookConfig(), &mockSigner{}, &mockLogger{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}
	if ch.Type() != types.ChannelWebhook {
		t.Errorf("expected channel type %q, got %q", types.ChannelWebhook, ch.Type())
	}
}

// --- Type() ---

func TestWebhookChannel_Type(t *testing.T) {
	ch := NewWebhookChannelWithClient(
		testWebhookConfig(),
		&mockSigner{},
		http.DefaultClient,
		&mockLogger{},
	)
	if ch.Type() != types.ChannelWebhook {
		t.Errorf("expected %q, got %q", types.ChannelWebhook, ch.Type())
	}
}

// --- ValidateConfig ---

func TestValidateConfig_MissingURL(t *testing.T) {
	ch := NewWebhookChannelWithClient(testWebhookConfig(), &mockSigner{}, http.DefaultClient, &mockLogger{})
	err := ch.ValidateConfig(map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing url")
	}
	if !strings.Contains(err.Error(), "missing required 'url'") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateConfig_EmptyURL(t *testing.T) {
	ch := NewWebhookChannelWithClient(testWebhookConfig(), &mockSigner{}, http.DefaultClient, &mockLogger{})
	err := ch.ValidateConfig(map[string]any{"url": ""})
	if err == nil {
		t.Fatal("expected error for empty url")
	}
}

func TestValidateConfig_NonHTTPS(t *testing.T) {
	ch := NewWebhookChannelWithClient(testWebhookConfig(), &mockSigner{}, http.DefaultClient, &mockLogger{})
	err := ch.ValidateConfig(map[string]any{"url": "http://example.com/hook"})
	if err == nil {
		t.Fatal("expected error for non-HTTPS url")
	}
	if !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateConfig_ValidHTTPS(t *testing.T) {
	ch := NewWebhookChannelWithClient(testWebhookConfig(), &mockSigner{}, http.DefaultClient, &mockLogger{})
	err := ch.ValidateConfig(map[string]any{"url": "https://hooks.slack.com/services/abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Format ---

func TestFormat_NilNotification(t *testing.T) {
	ch := NewWebhookChannelWithClient(testWebhookConfig(), &mockSigner{}, http.DefaultClient, &mockLogger{})
	_, err := ch.Format(context.Background(), nil, map[string]any{})
	if err == nil {
		t.Fatal("expected error for nil notification")
	}
}

func TestFormat_GenericPlatform(t *testing.T) {
	ch := NewWebhookChannelWithClient(testWebhookConfig(), &mockSigner{}, http.DefaultClient, &mockLogger{})
	n := &types.Notification{
		ID:             "notif-001",
		WatchPointID:   "wp-001",
		OrganizationID: "org-001",
		EventType:      types.EventThresholdCrossed,
		Urgency:        types.UrgencyWarning,
		Payload:        map[string]interface{}{"key": "value"},
	}

	payload, err := ch.Format(context.Background(), n, map[string]any{"url": "https://example.com/hook"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("expected non-empty payload")
	}
	// Check that the generic payload structure is present.
	payloadStr := string(payload)
	if !strings.Contains(payloadStr, "threshold_crossed") {
		t.Errorf("payload should contain event_type, got: %s", payloadStr)
	}
}

// --- Deliver: Success ---

func TestDeliver_Success_2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify method and content type.
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %s", ct)
		}

		w.Header().Set("X-Request-Id", "req-12345")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	ch := newTestChannel(server)
	result, err := ch.Deliver(context.Background(), []byte(`{"test":true}`), server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status != types.DeliveryStatusSent {
		t.Errorf("expected status %q, got %q", types.DeliveryStatusSent, result.Status)
	}
	if result.ProviderMessageID != "req-12345" {
		t.Errorf("expected provider_msg_id 'req-12345', got %q", result.ProviderMessageID)
	}
	if result.Retryable {
		t.Error("expected non-retryable for success")
	}
}

func TestDeliver_Success_SyntheticID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No X-Request-Id header.
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ch := newTestChannel(server)
	result, err := ch.Deliver(context.Background(), []byte(`{}`), server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status != types.DeliveryStatusSent {
		t.Errorf("expected status %q, got %q", types.DeliveryStatusSent, result.Status)
	}
	// Check synthetic ID format: generic-{status}-{timestamp}-{uuid_short}
	if !strings.HasPrefix(result.ProviderMessageID, "generic-200-") {
		t.Errorf("expected synthetic ID starting with 'generic-200-', got %q", result.ProviderMessageID)
	}
}

// --- Deliver: 429 Rate Limiting ---

func TestDeliver_429_ShortDelay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited"))
	}))
	defer server.Close()

	ch := newTestChannel(server)
	result, err := ch.Deliver(context.Background(), []byte(`{}`), server.URL)

	// Short delay: no error, result indicates retryable with delay.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Retryable {
		t.Error("expected retryable for 429")
	}
	if result.RetryAfter == nil {
		t.Fatal("expected RetryAfter to be set")
	}
	if *result.RetryAfter != 30*time.Second {
		t.Errorf("expected RetryAfter 30s, got %v", *result.RetryAfter)
	}
}

func TestDeliver_429_LongDelay_ExceedsSQSMax(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600") // 1 hour
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ch := newTestChannel(server)
	result, err := ch.Deliver(context.Background(), []byte(`{}`), server.URL)

	// Long delay: must return ErrWebhookLongDelay to trigger parking.
	if !errors.Is(err, ErrWebhookLongDelay) {
		t.Fatalf("expected ErrWebhookLongDelay, got: %v", err)
	}

	if result == nil {
		t.Fatal("expected non-nil result even with parking error")
	}
	if !result.Retryable {
		t.Error("expected retryable for 429 (parking)")
	}
	if result.RetryAfter == nil {
		t.Fatal("expected RetryAfter to be set")
	}
	if *result.RetryAfter != 3600*time.Second {
		t.Errorf("expected RetryAfter 3600s, got %v", *result.RetryAfter)
	}
}

func TestDeliver_429_NoRetryAfterHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Retry-After header.
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ch := newTestChannel(server)
	result, err := ch.Deliver(context.Background(), []byte(`{}`), server.URL)

	// Missing header defaults to 60s which is under SQS limit.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Retryable {
		t.Error("expected retryable for 429")
	}
	if result.RetryAfter == nil {
		t.Fatal("expected RetryAfter to be set")
	}
	if *result.RetryAfter != 60*time.Second {
		t.Errorf("expected default RetryAfter 60s, got %v", *result.RetryAfter)
	}
}

// --- Deliver: 410 Gone (Terminal) ---

func TestDeliver_410_Gone_Terminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer server.Close()

	ch := newTestChannel(server)
	result, err := ch.Deliver(context.Background(), []byte(`{}`), server.URL)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status != types.DeliveryStatusFailed {
		t.Errorf("expected status %q, got %q", types.DeliveryStatusFailed, result.Status)
	}
	if result.Retryable {
		t.Error("expected non-retryable for 410")
	}
	if !result.Terminal {
		t.Error("expected Terminal=true for 410 Gone")
	}
	if result.FailureReason != "endpoint_gone_410" {
		t.Errorf("expected failure reason 'endpoint_gone_410', got %q", result.FailureReason)
	}
}

// --- Deliver: 4xx Client Errors ---

func TestDeliver_4xx_PermanentFailure(t *testing.T) {
	tests := []int{400, 401, 403, 404, 405, 422}
	for _, statusCode := range tests {
		t.Run(fmt.Sprintf("status_%d", statusCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(statusCode)
				w.Write([]byte("bad request"))
			}))
			defer server.Close()

			ch := newTestChannel(server)
			result, err := ch.Deliver(context.Background(), []byte(`{}`), server.URL)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.Retryable {
				t.Errorf("expected non-retryable for %d", statusCode)
			}
			if result.Status != types.DeliveryStatusFailed {
				t.Errorf("expected status %q, got %q", types.DeliveryStatusFailed, result.Status)
			}
		})
	}
}

// --- Deliver: 5xx Server Errors ---

func TestDeliver_5xx_RetryableFailure(t *testing.T) {
	tests := []int{500, 502, 503, 504}
	for _, statusCode := range tests {
		t.Run(fmt.Sprintf("status_%d", statusCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(statusCode)
				w.Write([]byte("server error"))
			}))
			defer server.Close()

			ch := newTestChannel(server)
			result, err := ch.Deliver(context.Background(), []byte(`{}`), server.URL)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !result.Retryable {
				t.Errorf("expected retryable for %d", statusCode)
			}
			if result.Status != types.DeliveryStatusFailed {
				t.Errorf("expected status %q, got %q", types.DeliveryStatusFailed, result.Status)
			}
		})
	}
}

// --- Deliver: Network Errors ---

func TestDeliver_NetworkError_Retryable(t *testing.T) {
	// Use a server that is immediately closed to simulate connection refused.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close() // Close immediately.

	ch := NewWebhookChannelWithClient(
		testWebhookConfig(),
		&mockSigner{},
		&http.Client{Timeout: 1 * time.Second},
		&mockLogger{},
	)
	result, err := ch.Deliver(context.Background(), []byte(`{}`), serverURL)

	// Network errors should return a result (not a raw error) with Retryable=true.
	if err != nil {
		t.Fatalf("expected nil error (result-based), got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.Retryable {
		t.Error("expected retryable for network error")
	}
}

// --- ShouldRetry ---

func TestShouldRetry(t *testing.T) {
	ch := NewWebhookChannelWithClient(testWebhookConfig(), &mockSigner{}, http.DefaultClient, &mockLogger{})

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil error", nil, false},
		{"generic error", errors.New("something went wrong"), true},
		{"SSRF blocked", security.ErrSSRFBlocked, false},
		{"SSRF DNS timeout", security.ErrSSRFDNSTimeout, false},
		{"SSRF too many redirects", security.ErrSSRFTooManyRedirects, false},
		{"SSRF DNS failed", security.ErrSSRFDNSFailed, false},
		{"long delay", ErrWebhookLongDelay, true},
		{"wrapped SSRF", fmt.Errorf("outer: %w", security.ErrSSRFBlocked), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ch.ShouldRetry(tt.err)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// --- parseRetryAfter ---

func TestParseRetryAfter(t *testing.T) {
	fixedTime := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	clock := &mockClock{now: fixedTime}

	tests := []struct {
		name     string
		header   string
		expected time.Duration
	}{
		{"empty header defaults to 60s", "", 60 * time.Second},
		{"integer seconds", "30", 30 * time.Second},
		{"large integer seconds", "3600", 3600 * time.Second},
		{"zero seconds defaults to 1s", "0", 1 * time.Second},
		{"negative seconds defaults to 1s", "-5", 1 * time.Second},
		{"unparseable defaults to 60s", "bad-value", 60 * time.Second},
		{"RFC1123 date 30min ahead", fixedTime.Add(30 * time.Minute).Format(time.RFC1123), 30 * time.Minute},
		{"RFC1123 date in past defaults to 1s", fixedTime.Add(-1 * time.Hour).Format(time.RFC1123), 1 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseRetryAfter(tt.header, clock)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// --- extractProviderMessageID ---

func TestExtractProviderMessageID_Slack(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{
			"X-Slack-Req-Id": []string{"slack-req-123"},
		},
	}
	id := extractProviderMessageID(resp, PlatformSlack)
	if id != "slack-req-123" {
		t.Errorf("expected 'slack-req-123', got %q", id)
	}
}

func TestExtractProviderMessageID_GenericHeader(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{
			"X-Request-Id": []string{"req-456"},
		},
	}
	id := extractProviderMessageID(resp, PlatformGeneric)
	if id != "req-456" {
		t.Errorf("expected 'req-456', got %q", id)
	}
}

func TestExtractProviderMessageID_Synthetic(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
	}
	id := extractProviderMessageID(resp, PlatformGeneric)
	if !strings.HasPrefix(id, "generic-200-") {
		t.Errorf("expected synthetic ID starting with 'generic-200-', got %q", id)
	}
}

// --- isSSRFError ---

func TestIsSSRFError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil", nil, false},
		{"generic error", errors.New("foo"), false},
		{"SSRF blocked", security.ErrSSRFBlocked, true},
		{"SSRF DNS timeout", security.ErrSSRFDNSTimeout, true},
		{"SSRF redirects", security.ErrSSRFTooManyRedirects, true},
		{"SSRF DNS failed", security.ErrSSRFDNSFailed, true},
		{"wrapped SSRF", fmt.Errorf("wrapped: %w", security.ErrSSRFBlocked), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSSRFError(tt.err); got != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

// --- Integration Test: 429 with Retry-After ---

// TestIntegration_429RetryDelay verifies the complete interaction between
// WebhookChannel and a real HTTP server returning 429. This test validates
// that the Retry-After header is correctly parsed and the right delay is
// calculated for the SQS re-queue operation.
//
// Architecture reference: NOTIF-005 flow simulation
func TestIntegration_429RetryDelay(t *testing.T) {
	requestCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++

		// Verify the webhook channel sends correct headers.
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("request %d: expected Content-Type application/json, got %s", requestCount, ct)
		}
		if ua := r.Header.Get("User-Agent"); ua != "WatchPoint-Test/1.0" {
			t.Errorf("request %d: expected User-Agent WatchPoint-Test/1.0, got %s", requestCount, ua)
		}

		// Read and verify payload was transmitted.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("request %d: failed to read body: %v", requestCount, err)
		}
		if len(body) == 0 {
			t.Errorf("request %d: expected non-empty body", requestCount)
		}

		// Return 429 with various Retry-After values based on request count.
		switch requestCount {
		case 1:
			// Short delay (within SQS limits).
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
		case 2:
			// Long delay (exceeds SQS max of 900s, triggers parking).
			w.Header().Set("Retry-After", "1800")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
		case 3:
			// Success after "retry".
			w.Header().Set("X-Request-Id", "success-after-retry")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer server.Close()

	ch := newTestChannel(server)
	payload := []byte(`{"event_type":"threshold_crossed","watchpoint_id":"wp-test"}`)

	// --- Request 1: 429 with short Retry-After (120s) ---
	result1, err1 := ch.Deliver(context.Background(), payload, server.URL)
	if err1 != nil {
		t.Fatalf("request 1: unexpected error: %v", err1)
	}
	if !result1.Retryable {
		t.Error("request 1: expected retryable")
	}
	if result1.RetryAfter == nil {
		t.Fatal("request 1: expected RetryAfter to be set")
	}
	expected1 := 120 * time.Second
	if *result1.RetryAfter != expected1 {
		t.Errorf("request 1: expected RetryAfter %v, got %v", expected1, *result1.RetryAfter)
	}
	// Verify 120s is within SQS limit: should NOT trigger parking.
	if result1.RetryAfter.Seconds() > float64(SQSMaxDelaySeconds) {
		t.Error("request 1: 120s should be within SQS limit, should not trigger parking")
	}

	// --- Request 2: 429 with long Retry-After (1800s > 900s SQS max) ---
	result2, err2 := ch.Deliver(context.Background(), payload, server.URL)
	if !errors.Is(err2, ErrWebhookLongDelay) {
		t.Fatalf("request 2: expected ErrWebhookLongDelay, got: %v", err2)
	}
	if result2 == nil {
		t.Fatal("request 2: expected non-nil result")
	}
	if !result2.Retryable {
		t.Error("request 2: expected retryable")
	}
	if result2.RetryAfter == nil {
		t.Fatal("request 2: expected RetryAfter to be set")
	}
	expected2 := 1800 * time.Second
	if *result2.RetryAfter != expected2 {
		t.Errorf("request 2: expected RetryAfter %v, got %v", expected2, *result2.RetryAfter)
	}
	// Verify the Handler would detect this needs parking.
	if result2.RetryAfter.Seconds() <= float64(SQSMaxDelaySeconds) {
		t.Error("request 2: 1800s should exceed SQS limit, must trigger parking")
	}

	// --- Request 3: Successful delivery after simulated retry ---
	result3, err3 := ch.Deliver(context.Background(), payload, server.URL)
	if err3 != nil {
		t.Fatalf("request 3: unexpected error: %v", err3)
	}
	if result3.Status != types.DeliveryStatusSent {
		t.Errorf("request 3: expected status %q, got %q", types.DeliveryStatusSent, result3.Status)
	}
	if result3.ProviderMessageID != "success-after-retry" {
		t.Errorf("request 3: expected provider_msg_id 'success-after-retry', got %q", result3.ProviderMessageID)
	}

	// Verify all 3 requests were made.
	if requestCount != 3 {
		t.Errorf("expected 3 requests, got %d", requestCount)
	}
}

// TestIntegration_429RetryAfterHTTPDate verifies parsing of Retry-After as
// an HTTP-date string (RFC 1123 format).
func TestIntegration_429RetryAfterHTTPDate(t *testing.T) {
	fixedNow := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	retryAt := fixedNow.Add(5 * time.Minute) // 5 minutes in the future

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", retryAt.Format(time.RFC1123))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ch := newTestChannel(server)
	ch.SetClock(&mockClock{now: fixedNow})

	result, err := ch.Deliver(context.Background(), []byte(`{}`), server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Retryable {
		t.Error("expected retryable")
	}
	if result.RetryAfter == nil {
		t.Fatal("expected RetryAfter to be set")
	}
	// The delay should be approximately 5 minutes.
	if *result.RetryAfter != 5*time.Minute {
		t.Errorf("expected RetryAfter ~5m, got %v", *result.RetryAfter)
	}
}

// TestIntegration_FullDeliveryFlow exercises a complete webhook delivery
// flow including headers, payload, and response handling.
func TestIntegration_FullDeliveryFlow(t *testing.T) {
	var capturedHeaders http.Header
	var capturedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}

		w.Header().Set("X-Request-Id", "full-flow-id")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"received"}`))
	}))
	defer server.Close()

	ch := newTestChannel(server)

	payload := []byte(`{"event_type":"threshold_crossed","watchpoint_id":"wp-flow-test"}`)
	result, err := ch.Deliver(context.Background(), payload, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify headers.
	if ct := capturedHeaders.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", ct)
	}
	if ua := capturedHeaders.Get("User-Agent"); ua != "WatchPoint-Test/1.0" {
		t.Errorf("expected User-Agent WatchPoint-Test/1.0, got %s", ua)
	}

	// Verify payload was transmitted intact.
	if string(capturedBody) != string(payload) {
		t.Errorf("payload mismatch: expected %s, got %s", string(payload), string(capturedBody))
	}

	// Verify result.
	if result.Status != types.DeliveryStatusSent {
		t.Errorf("expected status %q, got %q", types.DeliveryStatusSent, result.Status)
	}
	if result.ProviderMessageID != "full-flow-id" {
		t.Errorf("expected provider_msg_id 'full-flow-id', got %q", result.ProviderMessageID)
	}
}

// --- Deliver: 429 Boundary at Exactly SQS Limit ---

func TestDeliver_429_ExactlySQSLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(SQSMaxDelaySeconds))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ch := newTestChannel(server)
	result, err := ch.Deliver(context.Background(), []byte(`{}`), server.URL)

	// Exactly at the SQS limit should NOT trigger parking.
	if err != nil {
		t.Fatalf("expected no error at exact SQS limit, got: %v", err)
	}
	if result.RetryAfter == nil {
		t.Fatal("expected RetryAfter")
	}
	if *result.RetryAfter != time.Duration(SQSMaxDelaySeconds)*time.Second {
		t.Errorf("expected RetryAfter %ds, got %v", SQSMaxDelaySeconds, *result.RetryAfter)
	}
}

func TestDeliver_429_OneSecondOverSQSLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(SQSMaxDelaySeconds+1))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ch := newTestChannel(server)
	_, err := ch.Deliver(context.Background(), []byte(`{}`), server.URL)

	// One second over the SQS limit MUST trigger parking.
	if !errors.Is(err, ErrWebhookLongDelay) {
		t.Fatalf("expected ErrWebhookLongDelay at %ds, got: %v", SQSMaxDelaySeconds+1, err)
	}
}
