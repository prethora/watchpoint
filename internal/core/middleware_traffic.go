package core

import (
	"bytes"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"watchpoint/internal/types"
)

// defaultRateLimitWindow is the default rate limit window used by the middleware.
// The actual per-organization limit and window are resolved by the RateLimitStore
// implementation against the organization's plan.
const defaultRateLimitWindow = 24 * time.Hour

// defaultRateLimitMax is the default maximum number of requests per window.
// This is used as a fallback; the production RateLimitStore implementation
// reads the actual limit from the organization's plan_limits.
const defaultRateLimitMax = 1000

// RateLimit uses a backing store to enforce plan limits.
//
// The middleware extracts the OrganizationID from the request context (set by
// AuthMiddleware) and calls RateLimitStore.IncrementAndCheck to atomically
// increment the counter and check against the limit.
//
// If no RateLimitStore is configured (e.g., during tests), the middleware
// passes through without rate limiting.
//
// If no Actor is in the context (unauthenticated request), the middleware
// passes through -- AuthMiddleware will handle the 401.
//
// On every request (allowed or not), the middleware sets standard rate limit
// response headers:
//   - X-RateLimit-Limit: The maximum number of requests in the window.
//   - X-RateLimit-Remaining: The number of requests remaining.
//   - X-RateLimit-Reset: Unix timestamp when the window resets.
//
// When rate limited, the middleware also sets:
//   - Retry-After: Seconds until the rate limit window resets.
func (s *Server) RateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If no rate limit store is configured, pass through.
		if s.RateLimitStore == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Extract the Actor from context. If not present, the request is
		// unauthenticated and AuthMiddleware will handle the 401.
		actor, ok := types.GetActor(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		// Use the organization ID as the rate limit key.
		orgID := actor.OrganizationID
		if orgID == "" {
			next.ServeHTTP(w, r)
			return
		}

		result, err := s.RateLimitStore.IncrementAndCheck(
			r.Context(),
			orgID,
			defaultRateLimitMax,
			defaultRateLimitWindow,
		)
		if err != nil {
			// On store errors, fail open: allow the request through but log
			// the error. This prevents a rate limit store outage from blocking
			// all API traffic.
			s.Logger.Error("rate limit store error",
				slog.String("org_id", orgID),
				slog.String("error", err.Error()),
			)
			next.ServeHTTP(w, r)
			return
		}

		// Set rate limit headers on every response (allowed or denied).
		setRateLimitHeaders(w, defaultRateLimitMax, result)

		if !result.Allowed {
			s.Logger.Warn("rate limit exceeded",
				slog.String("org_id", orgID),
				slog.String("actor_id", actor.ID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)

			// Set Retry-After header for 429 responses.
			retryAfter := int(time.Until(result.ResetAt).Seconds())
			if retryAfter < 1 {
				retryAfter = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))

			requestID := types.GetRequestID(r.Context())
			resp := APIErrorResponse{
				Error: ErrorDetail{
					Code:      string(types.ErrCodeRateLimit),
					Message:   "Rate limit exceeded. Please retry after the reset time.",
					RequestID: requestID,
				},
			}
			JSON(w, r, http.StatusTooManyRequests, resp)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// setRateLimitHeaders writes the standard X-RateLimit-* headers to the response.
func setRateLimitHeaders(w http.ResponseWriter, limit int, result RateLimitResult) {
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(result.Remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(result.ResetAt.Unix(), 10))
}

// ResponseCapturer wraps an http.ResponseWriter to buffer the response status
// code, headers, and body during handler execution. This is used by the
// IdempotencyMiddleware to capture the complete response for storage and replay.
//
// The captured data is NOT written to the underlying ResponseWriter until
// Flush is called, allowing the middleware to inspect and potentially store
// the response before sending it to the client.
type ResponseCapturer struct {
	underlying http.ResponseWriter
	statusCode int
	body       bytes.Buffer
	headers    http.Header
	written    bool
}

// newResponseCapturer creates a new ResponseCapturer wrapping the given writer.
// It copies existing response headers to the capture buffer.
func newResponseCapturer(w http.ResponseWriter) *ResponseCapturer {
	return &ResponseCapturer{
		underlying: w,
		statusCode: http.StatusOK,
		headers:    make(http.Header),
	}
}

// Header returns the captured headers map. Handlers writing headers will
// write to this map, which is later flushed to the underlying writer.
func (rc *ResponseCapturer) Header() http.Header {
	return rc.headers
}

// WriteHeader captures the status code without writing to the underlying writer.
func (rc *ResponseCapturer) WriteHeader(code int) {
	if !rc.written {
		rc.statusCode = code
		rc.written = true
	}
}

// Write captures the response body without writing to the underlying writer.
func (rc *ResponseCapturer) Write(b []byte) (int, error) {
	if !rc.written {
		rc.statusCode = http.StatusOK
		rc.written = true
	}
	return rc.body.Write(b)
}

// Flush writes the captured response (status code, headers, and body) to the
// underlying ResponseWriter. This should be called exactly once after the
// handler chain completes.
func (rc *ResponseCapturer) Flush() {
	// Copy captured headers to the underlying writer.
	for key, values := range rc.headers {
		for _, v := range values {
			rc.underlying.Header().Add(key, v)
		}
	}
	rc.underlying.WriteHeader(rc.statusCode)
	_, _ = rc.underlying.Write(rc.body.Bytes())
}

// Unwrap returns the underlying ResponseWriter, enabling http.ResponseController
// and other standard library helpers to access it for features like Flush and Hijack.
func (rc *ResponseCapturer) Unwrap() http.ResponseWriter {
	return rc.underlying
}

// StatusCode returns the captured HTTP status code.
func (rc *ResponseCapturer) StatusCode() int {
	return rc.statusCode
}

// Body returns the captured response body as bytes.
func (rc *ResponseCapturer) Body() []byte {
	return rc.body.Bytes()
}

// errCodeIdempotencyConflict is the error code returned when a request with the
// same idempotency key is already being processed.
const errCodeIdempotencyConflict = "conflict_idempotency_in_progress"

// IdempotencyMiddleware ensures POST requests with an "Idempotency-Key" header
// are processed exactly once. It uses the IdempotencyStore interface to manage
// key state.
//
// Flow:
//  1. Extract the Idempotency-Key header and OrgID from context.
//  2. Look up the key in the store.
//  3. Case A (Found & Completed): Replay the stored response immediately.
//  4. Case B (Found & Processing): Return 409 Conflict.
//  5. Case C (New): Create record, wrap ResponseWriter with ResponseCapturer,
//     call the next handler, save the response, and flush to client.
//
// Non-POST requests and requests without the Idempotency-Key header pass
// through without idempotency handling.
//
// If no IdempotencyStore is configured (e.g., during tests), the middleware
// passes through without idempotency handling.
func (s *Server) IdempotencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If no idempotency store is configured, pass through.
		if s.IdempotencyStore == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Only apply idempotency to POST requests.
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}

		// Extract the Idempotency-Key header.
		idempotencyKey := r.Header.Get("Idempotency-Key")
		if idempotencyKey == "" {
			// No idempotency key provided; proceed without idempotency handling.
			next.ServeHTTP(w, r)
			return
		}

		// Extract OrgID from context. If not present, the request is
		// unauthenticated and will be rejected by auth middleware.
		orgID, ok := types.GetOrgID(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		ctx := r.Context()

		// Check if the key already exists.
		record, err := s.IdempotencyStore.Get(ctx, idempotencyKey, orgID)
		if err != nil {
			s.Logger.Error("idempotency store get error",
				slog.String("key", idempotencyKey),
				slog.String("org_id", orgID),
				slog.String("error", err.Error()),
			)
			// Fail open: allow the request through on store errors.
			next.ServeHTTP(w, r)
			return
		}

		if record != nil {
			switch record.Status {
			case IdempotencyStatusCompleted:
				// Case A: Return cached response.
				s.Logger.Info("idempotency key hit, returning cached response",
					slog.String("key", idempotencyKey),
					slog.String("org_id", orgID),
					slog.Int("cached_status", record.ResponseCode),
				)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Idempotent-Replayed", "true")
				w.WriteHeader(record.ResponseCode)
				_, _ = w.Write(record.ResponseBody)
				return

			case IdempotencyStatusProcessing:
				// Case B: Request is currently being processed.
				s.Logger.Warn("idempotency key conflict, request in progress",
					slog.String("key", idempotencyKey),
					slog.String("org_id", orgID),
				)
				requestID := types.GetRequestID(ctx)
				resp := APIErrorResponse{
					Error: ErrorDetail{
						Code:      errCodeIdempotencyConflict,
						Message:   "A request with this idempotency key is currently being processed",
						RequestID: requestID,
					},
				}
				JSON(w, r, http.StatusConflict, resp)
				return

			case IdempotencyStatusFailed:
				// Failed requests can be retried. Fall through to process
				// the request as new.
				s.Logger.Info("idempotency key previously failed, retrying",
					slog.String("key", idempotencyKey),
					slog.String("org_id", orgID),
				)
			}
		}

		// Case C: New key (or retrying after failure). Create the record.
		if err := s.IdempotencyStore.Create(ctx, idempotencyKey, orgID, r.URL.Path); err != nil {
			// If creation fails (e.g., race condition), log and fail open.
			s.Logger.Error("idempotency store create error",
				slog.String("key", idempotencyKey),
				slog.String("org_id", orgID),
				slog.String("error", err.Error()),
			)
			next.ServeHTTP(w, r)
			return
		}

		// Wrap the ResponseWriter with a capturer to buffer the response.
		capturer := newResponseCapturer(w)

		// Execute the handler chain.
		next.ServeHTTP(capturer, r)

		// Persist the captured response.
		statusCode := capturer.StatusCode()
		body := capturer.Body()

		if statusCode >= 200 && statusCode < 500 {
			// Save successful responses (2xx, 3xx, 4xx client errors).
			// We save client errors too because the same request with the same
			// idempotency key should return the same validation error.
			if err := s.IdempotencyStore.Complete(ctx, idempotencyKey, orgID, statusCode, body); err != nil {
				s.Logger.Error("idempotency store complete error",
					slog.String("key", idempotencyKey),
					slog.String("org_id", orgID),
					slog.String("error", err.Error()),
				)
			}
		} else {
			// Mark 5xx responses as failed so they can be retried.
			if err := s.IdempotencyStore.Fail(ctx, idempotencyKey, orgID); err != nil {
				s.Logger.Error("idempotency store fail error",
					slog.String("key", idempotencyKey),
					slog.String("org_id", orgID),
					slog.String("error", err.Error()),
				)
			}
		}

		// Flush the captured response to the actual client.
		capturer.Flush()
	})
}

