package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// --- RateLimit Middleware Tests ---

func TestRateLimit_NilStore_PassesThrough(t *testing.T) {
	srv := newTestServerForTraffic(t)
	srv.RateLimitStore = nil

	called := false
	handler := srv.RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called when RateLimitStore is nil")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestRateLimit_NoActor_PassesThrough(t *testing.T) {
	srv := newTestServerForTraffic(t)
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{Allowed: false, Remaining: 0},
	}

	called := false
	handler := srv.RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// Request without Actor in context.
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called when no Actor is in context")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestRateLimit_EmptyOrgID_PassesThrough(t *testing.T) {
	srv := newTestServerForTraffic(t)
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{Allowed: false, Remaining: 0},
	}

	called := false
	handler := srv.RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "", // Empty org ID
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called when OrgID is empty")
	}
}

func TestRateLimit_Allowed_SetsHeaders(t *testing.T) {
	srv := newTestServerForTraffic(t)
	resetAt := time.Date(2026, 2, 7, 0, 0, 0, 0, time.UTC)
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{
			Allowed:   true,
			Remaining: 950,
			ResetAt:   resetAt,
		},
	}

	handler := srv.RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/watchpoints", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_456",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	// Check rate limit headers.
	if got := rec.Header().Get("X-RateLimit-Limit"); got != strconv.Itoa(defaultRateLimitMax) {
		t.Errorf("X-RateLimit-Limit: got %q, want %q", got, strconv.Itoa(defaultRateLimitMax))
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "950" {
		t.Errorf("X-RateLimit-Remaining: got %q, want %q", got, "950")
	}
	expectedReset := strconv.FormatInt(resetAt.Unix(), 10)
	if got := rec.Header().Get("X-RateLimit-Reset"); got != expectedReset {
		t.Errorf("X-RateLimit-Reset: got %q, want %q", got, expectedReset)
	}

	// Body should be from the next handler.
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

func TestRateLimit_Denied_Returns429(t *testing.T) {
	srv := newTestServerForTraffic(t)
	resetAt := time.Now().Add(30 * time.Minute)
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{
			Allowed:   false,
			Remaining: 0,
			ResetAt:   resetAt,
		},
	}

	nextCalled := false
	handler := srv.RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeAPIKey,
		OrganizationID: "org_456",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next handler should not be called when rate limited")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", rec.Code)
	}

	// Verify error response.
	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != string(types.ErrCodeRateLimit) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeRateLimit, resp.Error.Code)
	}

	// Verify Retry-After header.
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Error("Retry-After header should be set on 429 response")
	}
	retrySeconds, err := strconv.Atoi(retryAfter)
	if err != nil {
		t.Fatalf("Retry-After is not a valid integer: %q", retryAfter)
	}
	if retrySeconds < 1 {
		t.Errorf("Retry-After should be at least 1, got %d", retrySeconds)
	}

	// Verify rate limit headers are still set.
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining: got %q, want %q", got, "0")
	}
}

func TestRateLimit_StoreError_FailsOpen(t *testing.T) {
	srv := newTestServerForTraffic(t)
	srv.RateLimitStore = &MockRateLimitStore{
		Err: errors.New("database connection failed"),
	}

	called := false
	handler := srv.RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_456",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called on store error (fail open)")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestRateLimit_UsesOrgIDAsKey(t *testing.T) {
	srv := newTestServerForTraffic(t)
	mock := &MockRateLimitStore{
		Result: RateLimitResult{Allowed: true, Remaining: 99, ResetAt: time.Now().Add(time.Hour)},
	}
	srv.RateLimitStore = mock

	handler := srv.RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_abc",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_unique_789",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if len(mock.Calls) != 1 {
		t.Fatalf("expected 1 rate limit call, got %d", len(mock.Calls))
	}
	if mock.Calls[0].Key != "org_unique_789" {
		t.Errorf("rate limit key: got %q, want %q", mock.Calls[0].Key, "org_unique_789")
	}
}

func TestRateLimit_Denied_PreservesRequestID(t *testing.T) {
	srv := newTestServerForTraffic(t)
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{Allowed: false, Remaining: 0, ResetAt: time.Now().Add(time.Hour)},
	}

	handler := srv.RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_456",
	})
	ctx = types.WithRequestID(ctx, "req_test_xyz")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.RequestID != "req_test_xyz" {
		t.Errorf("expected request_id %q, got %q", "req_test_xyz", resp.Error.RequestID)
	}
}

// --- IdempotencyMiddleware Tests ---

func TestIdempotency_NilStore_PassesThrough(t *testing.T) {
	srv := newTestServerForTraffic(t)
	srv.IdempotencyStore = nil

	called := false
	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"wp_123"}`))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Idempotency-Key", "test-key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called when IdempotencyStore is nil")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}
}

func TestIdempotency_NonPOST_PassesThrough(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	srv.IdempotencyStore = store

	called := false
	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		called = false
		req := httptest.NewRequest(method, "/v1/watchpoints", nil)
		req.Header.Set("Idempotency-Key", "test-key-1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if !called {
			t.Errorf("%s: next handler should be called for non-POST requests", method)
		}
	}

	// No calls to the store should have been made.
	if len(store.GetCalls) != 0 {
		t.Errorf("expected 0 store Get calls for non-POST methods, got %d", len(store.GetCalls))
	}
}

func TestIdempotency_NoHeader_PassesThrough(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	srv.IdempotencyStore = store

	called := false
	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	// No Idempotency-Key header set.
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_456",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called without Idempotency-Key header")
	}
	if len(store.GetCalls) != 0 {
		t.Errorf("expected 0 store Get calls without header, got %d", len(store.GetCalls))
	}
}

func TestIdempotency_NoActor_PassesThrough(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	srv.IdempotencyStore = store

	called := false
	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Idempotency-Key", "test-key-1")
	// No Actor in context.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called without Actor in context")
	}
}

func TestIdempotency_NewKey_ProcessesAndStores(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	srv.IdempotencyStore = store

	responseBody := `{"data":{"id":"wp_new_123","name":"Test WatchPoint"}}`
	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(responseBody))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Idempotency-Key", "create-wp-001")
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_456",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Verify the handler response was passed through.
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}
	if rec.Body.String() != responseBody {
		t.Errorf("body: got %q, want %q", rec.Body.String(), responseBody)
	}

	// Verify store interactions.
	if len(store.GetCalls) != 1 {
		t.Fatalf("expected 1 Get call, got %d", len(store.GetCalls))
	}
	if store.GetCalls[0].Key != "create-wp-001" {
		t.Errorf("Get key: got %q, want %q", store.GetCalls[0].Key, "create-wp-001")
	}
	if store.GetCalls[0].OrgID != "org_456" {
		t.Errorf("Get orgID: got %q, want %q", store.GetCalls[0].OrgID, "org_456")
	}

	if len(store.CreateCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(store.CreateCalls))
	}
	if store.CreateCalls[0].RequestPath != "/v1/watchpoints" {
		t.Errorf("Create requestPath: got %q, want %q", store.CreateCalls[0].RequestPath, "/v1/watchpoints")
	}

	if len(store.CompleteCalls) != 1 {
		t.Fatalf("expected 1 Complete call, got %d", len(store.CompleteCalls))
	}
	if store.CompleteCalls[0].StatusCode != http.StatusCreated {
		t.Errorf("Complete statusCode: got %d, want %d", store.CompleteCalls[0].StatusCode, http.StatusCreated)
	}
	if string(store.CompleteCalls[0].Body) != responseBody {
		t.Errorf("Complete body: got %q, want %q", string(store.CompleteCalls[0].Body), responseBody)
	}
}

func TestIdempotency_ExistingCompleted_ReturnsCachedResponse(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	srv.IdempotencyStore = store

	// Pre-populate the store with a completed record.
	cachedBody := `{"data":{"id":"wp_cached_456"}}`
	_ = store.Create(context.Background(), "existing-key", "org_789", "/v1/watchpoints")
	_ = store.Complete(context.Background(), "existing-key", "org_789", http.StatusCreated, []byte(cachedBody))

	nextCalled := false
	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"id":"wp_new_should_not_appear"}}`))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Idempotency-Key", "existing-key")
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_789",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next handler should NOT be called for completed idempotency key")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected cached status 201, got %d", rec.Code)
	}
	if rec.Body.String() != cachedBody {
		t.Errorf("body: got %q, want cached %q", rec.Body.String(), cachedBody)
	}

	// Verify X-Idempotent-Replayed header.
	if got := rec.Header().Get("X-Idempotent-Replayed"); got != "true" {
		t.Errorf("X-Idempotent-Replayed: got %q, want %q", got, "true")
	}
}

func TestIdempotency_ExistingProcessing_Returns409(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	srv.IdempotencyStore = store

	// Pre-populate the store with a processing record.
	_ = store.Create(context.Background(), "in-progress-key", "org_789", "/v1/watchpoints")

	nextCalled := false
	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Idempotency-Key", "in-progress-key")
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_789",
	})
	ctx = types.WithRequestID(ctx, "req_conflict_test")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if nextCalled {
		t.Error("next handler should NOT be called for processing idempotency key")
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d", rec.Code)
	}

	var resp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Code != errCodeIdempotencyConflict {
		t.Errorf("expected error code %q, got %q", errCodeIdempotencyConflict, resp.Error.Code)
	}
	if resp.Error.RequestID != "req_conflict_test" {
		t.Errorf("expected request_id %q, got %q", "req_conflict_test", resp.Error.RequestID)
	}
}

func TestIdempotency_FailedKey_AllowsRetry(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	srv.IdempotencyStore = store

	// Pre-populate the store with a failed record.
	_ = store.Create(context.Background(), "failed-key", "org_789", "/v1/watchpoints")
	_ = store.Fail(context.Background(), "failed-key", "org_789")

	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"id":"wp_retry_success"}}`))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Idempotency-Key", "failed-key")
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_789",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}
	if rec.Body.String() != `{"data":{"id":"wp_retry_success"}}` {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

func TestIdempotency_Handler5xx_MarksAsFailed(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	srv.IdempotencyStore = store

	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error"}}`))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Idempotency-Key", "will-fail-key")
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_456",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}

	// Verify the record was marked as failed (not completed).
	if len(store.CompleteCalls) != 0 {
		t.Errorf("expected 0 Complete calls for 5xx, got %d", len(store.CompleteCalls))
	}

	// The store should have the record in failed state.
	record, err := store.Get(context.Background(), "will-fail-key", "org_456")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if record == nil {
		t.Fatal("expected record to exist")
	}
	if record.Status != IdempotencyStatusFailed {
		t.Errorf("expected status %q, got %q", IdempotencyStatusFailed, record.Status)
	}
}

func TestIdempotency_Handler4xx_MarksAsCompleted(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	srv.IdempotencyStore = store

	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_error"}}`))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Idempotency-Key", "client-error-key")
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_456",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	// Client errors should be stored as completed (same input = same error).
	if len(store.CompleteCalls) != 1 {
		t.Errorf("expected 1 Complete call for 4xx, got %d", len(store.CompleteCalls))
	}
	if store.CompleteCalls[0].StatusCode != http.StatusBadRequest {
		t.Errorf("Complete statusCode: got %d, want %d", store.CompleteCalls[0].StatusCode, http.StatusBadRequest)
	}
}

func TestIdempotency_GetError_FailsOpen(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	store.GetErr = errors.New("database unavailable")
	srv.IdempotencyStore = store

	called := false
	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Idempotency-Key", "error-key")
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_456",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called on store Get error (fail open)")
	}
}

func TestIdempotency_CreateError_FailsOpen(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	store.CreateErr = errors.New("insert conflict")
	srv.IdempotencyStore = store

	called := false
	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
	req.Header.Set("Idempotency-Key", "race-key")
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		Type:           types.ActorTypeUser,
		OrganizationID: "org_456",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called on store Create error (fail open)")
	}
}

// --- ResponseCapturer Tests ---

func TestResponseCapturer_CapturesStatusAndBody(t *testing.T) {
	underlying := httptest.NewRecorder()
	capturer := newResponseCapturer(underlying)

	capturer.WriteHeader(http.StatusCreated)
	_, err := capturer.Write([]byte(`{"id":"test"}`))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	// Verify captured values.
	if capturer.StatusCode() != http.StatusCreated {
		t.Errorf("status: got %d, want %d", capturer.StatusCode(), http.StatusCreated)
	}
	if string(capturer.Body()) != `{"id":"test"}` {
		t.Errorf("body: got %q, want %q", string(capturer.Body()), `{"id":"test"}`)
	}

	// Underlying writer should NOT have been written to yet.
	if underlying.Code != http.StatusOK { // httptest default
		t.Errorf("underlying should not have received WriteHeader yet, got %d", underlying.Code)
	}
	if underlying.Body.Len() != 0 {
		t.Errorf("underlying should not have received body yet, got %q", underlying.Body.String())
	}
}

func TestResponseCapturer_FlushWritesToUnderlying(t *testing.T) {
	underlying := httptest.NewRecorder()
	capturer := newResponseCapturer(underlying)

	capturer.Header().Set("Content-Type", "application/json")
	capturer.Header().Set("X-Custom-Header", "test-value")
	capturer.WriteHeader(http.StatusCreated)
	_, _ = capturer.Write([]byte(`{"id":"flushed"}`))

	capturer.Flush()

	// Verify underlying writer received everything.
	if underlying.Code != http.StatusCreated {
		t.Errorf("underlying status: got %d, want %d", underlying.Code, http.StatusCreated)
	}
	if underlying.Body.String() != `{"id":"flushed"}` {
		t.Errorf("underlying body: got %q, want %q", underlying.Body.String(), `{"id":"flushed"}`)
	}
	if got := underlying.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("underlying Content-Type: got %q, want %q", got, "application/json")
	}
	if got := underlying.Header().Get("X-Custom-Header"); got != "test-value" {
		t.Errorf("underlying X-Custom-Header: got %q, want %q", got, "test-value")
	}
}

func TestResponseCapturer_DefaultStatus200(t *testing.T) {
	underlying := httptest.NewRecorder()
	capturer := newResponseCapturer(underlying)

	// Write body without calling WriteHeader.
	_, _ = capturer.Write([]byte("hello"))

	if capturer.StatusCode() != http.StatusOK {
		t.Errorf("expected default status 200, got %d", capturer.StatusCode())
	}
}

func TestResponseCapturer_WriteHeaderOnlyOnce(t *testing.T) {
	underlying := httptest.NewRecorder()
	capturer := newResponseCapturer(underlying)

	capturer.WriteHeader(http.StatusCreated)
	capturer.WriteHeader(http.StatusNotFound) // Should be ignored.

	if capturer.StatusCode() != http.StatusCreated {
		t.Errorf("expected first status %d, got %d", http.StatusCreated, capturer.StatusCode())
	}
}

func TestResponseCapturer_MultipleWrites(t *testing.T) {
	underlying := httptest.NewRecorder()
	capturer := newResponseCapturer(underlying)

	capturer.WriteHeader(http.StatusOK)
	_, _ = capturer.Write([]byte("part1"))
	_, _ = capturer.Write([]byte("part2"))

	if string(capturer.Body()) != "part1part2" {
		t.Errorf("body: got %q, want %q", string(capturer.Body()), "part1part2")
	}
}

func TestResponseCapturer_HeaderIsolation(t *testing.T) {
	underlying := httptest.NewRecorder()
	capturer := newResponseCapturer(underlying)

	// Headers set on the capturer should not appear on underlying until Flush.
	capturer.Header().Set("X-Test", "captured")

	if got := underlying.Header().Get("X-Test"); got != "" {
		t.Errorf("underlying should not have header before Flush, got %q", got)
	}

	capturer.Flush()

	if got := underlying.Header().Get("X-Test"); got != "captured" {
		t.Errorf("underlying should have header after Flush, got %q", got)
	}
}

// --- MockIdempotencyStore Tests ---

func TestMockIdempotencyStore_CreateAndGet(t *testing.T) {
	store := NewMockIdempotencyStore()

	err := store.Create(context.Background(), "key1", "org1", "/v1/test")
	if err != nil {
		t.Fatalf("Create error: %v", err)
	}

	record, err := store.Get(context.Background(), "key1", "org1")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if record == nil {
		t.Fatal("expected record, got nil")
	}
	if record.Status != IdempotencyStatusProcessing {
		t.Errorf("status: got %q, want %q", record.Status, IdempotencyStatusProcessing)
	}
}

func TestMockIdempotencyStore_GetNonExistent(t *testing.T) {
	store := NewMockIdempotencyStore()

	record, err := store.Get(context.Background(), "nonexistent", "org1")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if record != nil {
		t.Errorf("expected nil for nonexistent key, got %+v", record)
	}
}

func TestMockIdempotencyStore_OrgIsolation(t *testing.T) {
	store := NewMockIdempotencyStore()

	_ = store.Create(context.Background(), "shared-key", "org1", "/test")

	// Same key, different org should return nil.
	record, _ := store.Get(context.Background(), "shared-key", "org2")
	if record != nil {
		t.Error("keys should be scoped to organization")
	}

	// Same key, same org should return the record.
	record, _ = store.Get(context.Background(), "shared-key", "org1")
	if record == nil {
		t.Error("key should exist for the creating organization")
	}
}

func TestMockIdempotencyStore_Complete(t *testing.T) {
	store := NewMockIdempotencyStore()
	_ = store.Create(context.Background(), "k", "o", "/test")
	_ = store.Complete(context.Background(), "k", "o", 201, []byte(`{"id":"123"}`))

	record, _ := store.Get(context.Background(), "k", "o")
	if record.Status != IdempotencyStatusCompleted {
		t.Errorf("status: got %q, want %q", record.Status, IdempotencyStatusCompleted)
	}
	if record.ResponseCode != 201 {
		t.Errorf("response code: got %d, want 201", record.ResponseCode)
	}
	if string(record.ResponseBody) != `{"id":"123"}` {
		t.Errorf("response body: got %q, want %q", string(record.ResponseBody), `{"id":"123"}`)
	}
}

func TestMockIdempotencyStore_Fail(t *testing.T) {
	store := NewMockIdempotencyStore()
	_ = store.Create(context.Background(), "k", "o", "/test")
	_ = store.Fail(context.Background(), "k", "o")

	record, _ := store.Get(context.Background(), "k", "o")
	if record.Status != IdempotencyStatusFailed {
		t.Errorf("status: got %q, want %q", record.Status, IdempotencyStatusFailed)
	}
}

func TestMockIdempotencyStore_DuplicateCreate(t *testing.T) {
	store := NewMockIdempotencyStore()
	_ = store.Create(context.Background(), "k", "o", "/test")
	err := store.Create(context.Background(), "k", "o", "/test")
	if err == nil {
		t.Fatal("expected error on duplicate Create, got nil")
	}
}

// --- Integration-style tests ---

func TestIdempotency_EndToEnd_CreateThenReplay(t *testing.T) {
	srv := newTestServerForTraffic(t)
	store := NewMockIdempotencyStore()
	srv.IdempotencyStore = store

	originalBody := `{"data":{"id":"wp_e2e_123","name":"End-to-End Test"}}`
	callCount := 0

	handler := srv.IdempotencyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(originalBody))
	}))

	makeRequest := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/watchpoints", nil)
		req.Header.Set("Idempotency-Key", "e2e-key")
		ctx := types.WithActor(req.Context(), types.Actor{
			ID:             "user_123",
			Type:           types.ActorTypeUser,
			OrganizationID: "org_e2e",
		})
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// First request: handler should be called.
	rec1 := makeRequest()
	if callCount != 1 {
		t.Errorf("handler should be called once on first request, got %d", callCount)
	}
	if rec1.Code != http.StatusCreated {
		t.Errorf("first request: expected 201, got %d", rec1.Code)
	}

	// Second request: handler should NOT be called; cached response returned.
	rec2 := makeRequest()
	if callCount != 1 {
		t.Errorf("handler should not be called on second request, count is %d", callCount)
	}
	if rec2.Code != http.StatusCreated {
		t.Errorf("second request: expected 201, got %d", rec2.Code)
	}
	if rec2.Body.String() != originalBody {
		t.Errorf("second request body: got %q, want %q", rec2.Body.String(), originalBody)
	}
	if got := rec2.Header().Get("X-Idempotent-Replayed"); got != "true" {
		t.Errorf("second request should have X-Idempotent-Replayed: got %q", got)
	}
}

func TestRateLimit_RetryAfter_MinimumOneSecond(t *testing.T) {
	srv := newTestServerForTraffic(t)
	// Reset time is in the past.
	srv.RateLimitStore = &MockRateLimitStore{
		Result: RateLimitResult{
			Allowed:   false,
			Remaining: 0,
			ResetAt:   time.Now().Add(-1 * time.Hour),
		},
	}

	handler := srv.RateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	ctx := types.WithActor(req.Context(), types.Actor{
		ID:             "user_123",
		OrganizationID: "org_456",
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	retryAfter := rec.Header().Get("Retry-After")
	val, _ := strconv.Atoi(retryAfter)
	if val < 1 {
		t.Errorf("Retry-After should be at least 1, got %d", val)
	}
}

// --- Test Helpers ---

// newTestServerForTraffic creates a minimal Server suitable for testing
// traffic middleware (rate limit and idempotency) in isolation.
func newTestServerForTraffic(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return &Server{
		Logger: logger,
	}
}
