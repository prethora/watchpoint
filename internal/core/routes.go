package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"watchpoint/internal/types"
)

// defaultRequestTimeout is the soft timeout applied to request contexts when
// no explicit RequestTimeout is configured. This should be set to Lambda
// timeout minus 1 second in production.
const defaultRequestTimeout = 29 * time.Second

// defaultRedactedHeaders lists header names whose values are masked in request
// logs to prevent accidental leakage of credentials or session tokens.
var defaultRedactedHeaders = []string{
	"Authorization",
	"Cookie",
	"X-CSRF-Token",
}

// MountRoutes defines the top-level routing hierarchy.
// It registers the global middleware chain, API version groups, and top-level
// routes (health check, OpenAPI spec).
func (s *Server) MountRoutes() {
	// Global Middleware Registration (strict order matters).
	s.registerGlobalMiddleware()

	// API Version Groups
	s.router.Route("/v1", s.mountV1)

	// Top-Level Routes (outside /v1 namespace)
	s.router.Get("/health", s.HandleHealth)
	s.router.Get("/openapi.json", s.ServeOpenAPISpec)
}

// registerGlobalMiddleware applies middleware in strict order.
//
// Ordering Rationale:
//  1. Recoverer          - Catches panics; outermost to catch all failures.
//  2. ContextTimeout     - Sets soft deadline before Lambda hard timeout.
//  3. RequestID          - Generates/propagates correlation ID for tracing.
//  4. SecurityHeaders    - Ensures all responses include security headers.
//  5. XRay               - AWS X-Ray segment generation and trace_id injection.
//  6. RequestLogger      - Structured logging (redacted headers).
//  7. CORS               - Browser security headers.
//  8. Metrics            - Request latency and count recording.
//  9. IPSecurity         - Blocks IPs with excessive failures (before auth).
// 10. Auth               - Resolves Actor, injects OrganizationID into context.
// 11. CSRF               - Validates CSRF tokens (depends on Actor type from Auth).
// 12. RateLimit          - Per-organization token buckets (requires OrgID from Auth).
// 13. Idempotency        - Locks for POST requests (requires OrgID from Auth).
func (s *Server) registerGlobalMiddleware() {
	s.router.Use(s.Recoverer)
	s.router.Use(ContextTimeoutMiddleware(s.requestTimeout()))
	s.router.Use(RequestIDMiddleware)
	s.router.Use(s.SecurityHeadersMiddleware)
	s.router.Use(s.XRayMiddleware)
	s.router.Use(RequestLogger(s.Logger, s.redactedHeaders()))
	s.router.Use(NewCORSMiddleware(s.corsAllowedOrigins()))
	s.router.Use(s.MetricsMiddleware)
	s.router.Use(s.IPSecurityMiddleware)
	s.router.Use(s.AuthMiddleware)
	s.router.Use(s.CSRFMiddleware)
	s.router.Use(s.RateLimit)
	s.router.Use(s.IdempotencyMiddleware)
}

// mountV1 registers all v1 endpoints. Domain handler routes are registered via
// V1RouteRegistrars, which are populated by the application entry point (main.go).
// This indirection avoids import cycles between core and handler packages.
//
// TODO: Mount remaining domain handlers (watchpoints, forecasts, organizations,
// billing) when their handler packages are implemented in Phase 6+.
func (s *Server) mountV1(r chi.Router) {
	for _, registrar := range s.V1RouteRegistrars {
		registrar(r)
	}
}

// requestTimeout returns the configured request timeout, falling back to the
// default if the config does not specify one. The timeout should be Lambda
// timeout minus 1 second for production use.
//
// TODO: When Config gains a RequestTimeout field, read it here instead of
// using the hardcoded default.
func (s *Server) requestTimeout() time.Duration {
	return defaultRequestTimeout
}

// redactedHeaders returns the list of header names to redact in request logs.
// If the configuration does not specify explicit redacted headers, a safe
// default set is used.
func (s *Server) redactedHeaders() []string {
	// No explicit RedactedHeaders field in Config yet; use defaults.
	// When Config gains a RedactedHeaders field, reference it here.
	return defaultRedactedHeaders
}

// corsAllowedOrigins returns the CORS allowed origins from configuration.
func (s *Server) corsAllowedOrigins() []string {
	if s.Config != nil && len(s.Config.Security.CorsAllowedOrigins) > 0 {
		return s.Config.Security.CorsAllowedOrigins
	}
	return []string{"*"}
}

// ContextTimeoutMiddleware sets a deadline on the request context.
// The duration should be set to (Lambda Timeout - 1 second) to allow
// graceful cleanup before the Lambda hard timeout kills the process.
// If the context deadline is exceeded, downstream handlers receive a
// cancelled context; the response is controlled by the handler's behavior
// on context cancellation.
func ContextTimeoutMiddleware(duration time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), duration)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestIDMiddleware generates or propagates a unique request ID for
// correlation across logs and traces. If the incoming request contains an
// X-Request-Id header, that value is reused; otherwise, a new random ID is
// generated.
//
// The request ID is stored in the context via types.WithRequestID and set as
// the X-Request-Id response header for client correlation.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = generateRequestID()
		}

		// Store in context for downstream access.
		ctx := types.WithRequestID(r.Context(), requestID)

		// Set the response header so clients can correlate responses.
		w.Header().Set("X-Request-Id", requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// generateRequestID produces a cryptographically random hex string suitable
// for use as a request correlation ID. It generates 16 random bytes encoded
// as 32 hex characters.
func generateRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: this should never happen in practice. If crypto/rand
		// fails, we still need a non-empty ID for correlation.
		return "fallback-" + hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b)
}

// XRayMiddleware creates AWS X-Ray segments and injects trace_id into the
// context and logger for distributed tracing.
//
// In non-production environments or when tracing is disabled, this middleware
// passes through without creating segments.
//
// TODO: Implement X-Ray integration when the AWS X-Ray SDK dependency is added.
// The current implementation is a pass-through stub.
func (s *Server) XRayMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// ServeOpenAPISpec serves the OpenAPI 3.0 specification file as JSON.
//
// TODO: Embed and serve the actual OpenAPI spec when it is authored.
// The current implementation returns a minimal placeholder.
func (s *Server) ServeOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	JSON(w, r, http.StatusOK, map[string]string{
		"openapi": "3.0.0",
		"info":    "WatchPoint API - Specification pending",
	})
}
