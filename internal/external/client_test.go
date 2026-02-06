package external

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"watchpoint/internal/types"

	"github.com/sony/gobreaker/v2"
)

// noopSleep is a sleep function that does nothing, for fast tests.
func noopSleep(time.Duration) {}

// newTestClient creates a BaseClient pointed at the given test server URL with
// sensible test defaults: fast retries, no real sleep.
func newTestClient(t *testing.T, serverURL string, policy RetryPolicy) *BaseClient {
	t.Helper()
	return NewBaseClient(
		&http.Client{Timeout: 5 * time.Second},
		"test-breaker",
		policy,
		"WatchPoint-Test/1.0",
		WithSleepFunc(noopSleep),
	)
}

// newTestClientWithBreaker creates a BaseClient with a custom circuit breaker for
// tests that need fine-grained control over the breaker configuration.
func newTestClientWithBreaker(t *testing.T, breaker *gobreaker.CircuitBreaker[*http.Response], policy RetryPolicy) *BaseClient {
	t.Helper()
	return NewBaseClientWithBreaker(
		&http.Client{Timeout: 5 * time.Second},
		breaker,
		policy,
		"WatchPoint-Test/1.0",
		WithSleepFunc(noopSleep),
	)
}

func TestDo_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, DefaultRetryPolicy())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"status":"ok"}` {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestDo_InjectsTraceID(t *testing.T) {
	var receivedTraceID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTraceID = r.Header.Get("X-B3-TraceId")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, DefaultRetryPolicy())

	ctx := types.WithRequestID(context.Background(), "trace-abc-123")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	resp.Body.Close()

	if receivedTraceID != "trace-abc-123" {
		t.Errorf("expected trace ID 'trace-abc-123', got '%s'", receivedTraceID)
	}
}

func TestDo_InjectsUserAgent(t *testing.T) {
	var receivedUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, DefaultRetryPolicy())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	resp.Body.Close()

	if receivedUA != "WatchPoint-Test/1.0" {
		t.Errorf("expected User-Agent 'WatchPoint-Test/1.0', got '%s'", receivedUA)
	}
}

func TestDo_RetriesOn500(t *testing.T) {
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := callCount.Add(1)
		if count <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	}))
	defer server.Close()

	policy := RetryPolicy{
		MaxRetries: 3,
		MinWait:    10 * time.Millisecond,
		MaxWait:    100 * time.Millisecond,
	}
	client := newTestClient(t, server.URL, policy)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected success after retries, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	if calls := callCount.Load(); calls != 3 {
		t.Errorf("expected 3 calls (2 failures + 1 success), got %d", calls)
	}
}

func TestDo_RetriesOn429(t *testing.T) {
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := callCount.Add(1)
		if count <= 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	policy := RetryPolicy{
		MaxRetries: 2,
		MinWait:    10 * time.Millisecond,
		MaxWait:    5 * time.Second,
	}
	client := newTestClient(t, server.URL, policy)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected success after 429 retry, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	if calls := callCount.Load(); calls != 2 {
		t.Errorf("expected 2 calls (1 rate-limited + 1 success), got %d", calls)
	}
}

func TestDo_ExhaustedRetriesReturnsError(t *testing.T) {
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	policy := RetryPolicy{
		MaxRetries: 2,
		MinWait:    10 * time.Millisecond,
		MaxWait:    50 * time.Millisecond,
	}
	client := newTestClient(t, server.URL, policy)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if resp != nil {
		t.Error("expected nil response on exhausted retries")
	}

	if err == nil {
		t.Fatal("expected error on exhausted retries, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}

	// After exhausted retries on 500s, should be upstream unavailable.
	if appErr.Code != types.ErrCodeUpstreamUnavailable {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamUnavailable, appErr.Code)
	}

	if calls := callCount.Load(); calls != 3 {
		t.Errorf("expected 3 total attempts (1 + 2 retries), got %d", calls)
	}
}

func TestDo_ExhaustedRetriesOn429ReturnsRateLimitedError(t *testing.T) {
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	policy := RetryPolicy{
		MaxRetries: 1,
		MinWait:    10 * time.Millisecond,
		MaxWait:    50 * time.Millisecond,
	}
	client := newTestClient(t, server.URL, policy)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if resp != nil {
		t.Error("expected nil response on exhausted 429 retries")
	}

	if err == nil {
		t.Fatal("expected error on exhausted 429 retries, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}

	if appErr.Code != types.ErrCodeUpstreamRateLimited {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamRateLimited, appErr.Code)
	}
}

func TestDo_CircuitBreakerOpensAfterThreshold(t *testing.T) {
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	// Create a circuit breaker that opens after 3 consecutive failures
	// for faster testing.
	breaker := gobreaker.NewCircuitBreaker[*http.Response](gobreaker.Settings{
		Name:        "test-open",
		MaxRequests: 1,
		Interval:    60 * time.Second,
		Timeout:     60 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures > 3
		},
	})

	// Use no retries so each Do() call = exactly 1 attempt.
	policy := RetryPolicy{
		MaxRetries: 0,
		MinWait:    1 * time.Millisecond,
		MaxWait:    1 * time.Millisecond,
	}
	client := newTestClientWithBreaker(t, breaker, policy)

	// Make requests through the server to accumulate failures.
	for i := 0; i < 4; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
		_, _ = client.Do(req)
	}

	// At this point we've had 4 consecutive failures. The breaker should now be open.
	// The next request should fail with a circuit breaker error without hitting the server.
	serverCallsBefore := callCount.Load()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
		t.Error("expected nil response when circuit breaker is open")
	}

	if err == nil {
		t.Fatal("expected error when circuit breaker is open, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}

	if appErr.Code != types.ErrCodeUpstreamRateLimited {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamRateLimited, appErr.Code)
	}

	// Verify the server was NOT called when breaker was open.
	serverCallsAfter := callCount.Load()
	if serverCallsAfter != serverCallsBefore {
		t.Errorf("expected no additional server calls when breaker is open, got %d more",
			serverCallsAfter-serverCallsBefore)
	}
}

func TestDo_RespectsRetryAfterHeader(t *testing.T) {
	var callCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := callCount.Add(1)
		if count == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var sleepDurations []time.Duration
	trackingSleep := func(d time.Duration) {
		sleepDurations = append(sleepDurations, d)
	}

	policy := RetryPolicy{
		MaxRetries: 1,
		MinWait:    100 * time.Millisecond,
		MaxWait:    10 * time.Second,
	}
	client := NewBaseClient(
		&http.Client{Timeout: 5 * time.Second},
		"test-retry-after",
		policy,
		"WatchPoint-Test/1.0",
		WithSleepFunc(trackingSleep),
	)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	resp.Body.Close()

	if len(sleepDurations) != 1 {
		t.Fatalf("expected 1 sleep call, got %d", len(sleepDurations))
	}

	// Retry-After: 2 should result in a 2-second wait.
	if sleepDurations[0] != 2*time.Second {
		t.Errorf("expected sleep of 2s (Retry-After), got %v", sleepDurations[0])
	}
}

func TestDo_4xxNotRetried(t *testing.T) {
	var callCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer server.Close()

	policy := RetryPolicy{
		MaxRetries: 3,
		MinWait:    10 * time.Millisecond,
		MaxWait:    50 * time.Millisecond,
	}
	client := newTestClient(t, server.URL, policy)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected no error for 400, got: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	// 4xx (except 429) should NOT be retried.
	if calls := callCount.Load(); calls != 1 {
		t.Errorf("expected exactly 1 call for 4xx, got %d", calls)
	}
}

func TestDo_NetworkErrorMapsToAppError(t *testing.T) {
	// Point at a server that immediately closes connections.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This handler won't be reached because we close the server.
	}))
	serverURL := server.URL
	server.Close() // Close immediately so connections fail.

	policy := RetryPolicy{
		MaxRetries: 1,
		MinWait:    1 * time.Millisecond,
		MaxWait:    1 * time.Millisecond,
	}
	client := newTestClient(t, serverURL, policy)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, serverURL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
		t.Error("expected nil response for network error")
	}

	if err == nil {
		t.Fatal("expected error for network error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}

	if appErr.Code != types.ErrCodeInternalUnexpected {
		t.Errorf("expected error code %s, got %s", types.ErrCodeInternalUnexpected, appErr.Code)
	}
}

func TestDo_NoTraceIDWhenNotInContext(t *testing.T) {
	var receivedTraceID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTraceID = r.Header.Get("X-B3-TraceId")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, DefaultRetryPolicy())

	// Use a plain context with no request ID.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	resp.Body.Close()

	if receivedTraceID != "" {
		t.Errorf("expected no X-B3-TraceId header, got '%s'", receivedTraceID)
	}
}

func TestDo_503RetriedAndEventuallySucceeds(t *testing.T) {
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := callCount.Add(1)
		if count <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("recovered"))
	}))
	defer server.Close()

	policy := RetryPolicy{
		MaxRetries: 2,
		MinWait:    1 * time.Millisecond,
		MaxWait:    10 * time.Millisecond,
	}
	client := newTestClient(t, server.URL, policy)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected success after 503 retry, got: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "recovered" {
		t.Errorf("expected body 'recovered', got '%s'", body)
	}
}

func TestDo_RetryAfterCappedByMaxWait(t *testing.T) {
	var callCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := callCount.Add(1)
		if count == 1 {
			w.Header().Set("Retry-After", "3600") // 1 hour
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var sleepDurations []time.Duration
	trackingSleep := func(d time.Duration) {
		sleepDurations = append(sleepDurations, d)
	}

	policy := RetryPolicy{
		MaxRetries: 1,
		MinWait:    100 * time.Millisecond,
		MaxWait:    5 * time.Second,
	}
	client := NewBaseClient(
		&http.Client{Timeout: 5 * time.Second},
		"test-capped",
		policy,
		"WatchPoint-Test/1.0",
		WithSleepFunc(trackingSleep),
	)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	resp.Body.Close()

	if len(sleepDurations) != 1 {
		t.Fatalf("expected 1 sleep, got %d", len(sleepDurations))
	}

	// Should be capped at MaxWait (5s), not the full 3600s.
	if sleepDurations[0] != 5*time.Second {
		t.Errorf("expected sleep capped at 5s, got %v", sleepDurations[0])
	}
}

func TestDefaultRetryPolicy(t *testing.T) {
	policy := DefaultRetryPolicy()

	if policy.MaxRetries != 3 {
		t.Errorf("expected MaxRetries 3, got %d", policy.MaxRetries)
	}
	if policy.MinWait != 500*time.Millisecond {
		t.Errorf("expected MinWait 500ms, got %v", policy.MinWait)
	}
	if policy.MaxWait != 10*time.Second {
		t.Errorf("expected MaxWait 10s, got %v", policy.MaxWait)
	}
}

func TestComputeBackoff_ExponentialGrowth(t *testing.T) {
	client := &BaseClient{
		retryPolicy: RetryPolicy{
			MaxRetries: 5,
			MinWait:    100 * time.Millisecond,
			MaxWait:    10 * time.Second,
		},
	}

	// Test that backoff generally grows with attempt number.
	// Due to jitter, we check bounds rather than exact values.
	for attempt := 0; attempt < 5; attempt++ {
		backoff := client.computeBackoff(attempt, nil)
		if backoff < client.retryPolicy.MinWait {
			t.Errorf("attempt %d: backoff %v < MinWait %v", attempt, backoff, client.retryPolicy.MinWait)
		}
		if backoff > client.retryPolicy.MaxWait {
			t.Errorf("attempt %d: backoff %v > MaxWait %v", attempt, backoff, client.retryPolicy.MaxWait)
		}
	}
}

func TestMapError_CircuitBreakerOpen(t *testing.T) {
	client := &BaseClient{}

	appErr := client.mapError(nil, gobreaker.ErrOpenState)
	if appErr.Code != types.ErrCodeUpstreamRateLimited {
		t.Errorf("expected %s, got %s", types.ErrCodeUpstreamRateLimited, appErr.Code)
	}

	if !strings.Contains(appErr.Message, "circuit breaker") {
		t.Errorf("expected message to mention circuit breaker, got: %s", appErr.Message)
	}
}

func TestMapError_TooManyRequests(t *testing.T) {
	client := &BaseClient{}

	appErr := client.mapError(nil, gobreaker.ErrTooManyRequests)
	if appErr.Code != types.ErrCodeUpstreamRateLimited {
		t.Errorf("expected %s, got %s", types.ErrCodeUpstreamRateLimited, appErr.Code)
	}
}

func TestDo_PostBodyPreservedAcrossRetries(t *testing.T) {
	var callCount atomic.Int32
	var receivedBodies []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBodies = append(receivedBodies, string(body))

		count := callCount.Add(1)
		if count <= 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	policy := RetryPolicy{
		MaxRetries: 2,
		MinWait:    1 * time.Millisecond,
		MaxWait:    10 * time.Millisecond,
	}
	client := newTestClient(t, server.URL, policy)

	body := `{"key":"value"}`
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		server.URL+"/test",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected success after retry, got error: %v", err)
	}
	resp.Body.Close()

	if len(receivedBodies) != 2 {
		t.Fatalf("expected 2 requests (1 failure + 1 success), got %d", len(receivedBodies))
	}

	// Both attempts should have received the full body.
	for i, rb := range receivedBodies {
		if rb != body {
			t.Errorf("attempt %d: expected body %q, got %q", i, body, rb)
		}
	}
}

func TestDo_MapError500ReturnsUpstreamUnavailable(t *testing.T) {
	client := &BaseClient{}

	resp := &http.Response{StatusCode: http.StatusInternalServerError}
	appErr := client.mapError(resp, fmt.Errorf("upstream returned 500"))
	if appErr.Code != types.ErrCodeUpstreamUnavailable {
		t.Errorf("expected %s, got %s", types.ErrCodeUpstreamUnavailable, appErr.Code)
	}
}
