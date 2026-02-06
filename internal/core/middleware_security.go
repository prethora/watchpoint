package core

import (
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"watchpoint/internal/types"
)

// errCodeIPBlocked is the error code returned when an IP address is blocked
// by the security service due to excessive failed authentication attempts.
const errCodeIPBlocked = "ip_blocked"

// errCodeCSRFInvalid is the error code returned when CSRF token validation
// fails for session-based authentication.
const errCodeCSRFInvalid = "csrf_token_invalid"

// IPSecurityMiddleware provides proactive IP-based blocking before authentication.
// It executes BEFORE AuthMiddleware to reject known-bad IPs without performing
// expensive authentication checks (DB queries, bcrypt).
//
// Logic:
//  1. Extract client IP from X-Forwarded-For header (first entry) or RemoteAddr.
//  2. Call SecurityService.IsIPBlocked(ctx, ip).
//  3. If blocked: Return 403 Forbidden with JSON error body.
//  4. If not blocked: Call next handler.
//
// If SecurityService is nil (e.g., during tests that don't inject it), the
// middleware passes through without blocking.
func (s *Server) IPSecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If no security service is configured, pass through.
		if s.SecurityService == nil {
			next.ServeHTTP(w, r)
			return
		}

		ip := extractClientIP(r)

		if s.SecurityService.IsIPBlocked(r.Context(), ip) {
			s.Logger.Warn("blocked request from IP",
				slog.String("ip", ip),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)

			requestID := types.GetRequestID(r.Context())
			resp := APIErrorResponse{
				Error: ErrorDetail{
					Code:      errCodeIPBlocked,
					Message:   "Access denied",
					RequestID: requestID,
				},
			}
			JSON(w, r, http.StatusForbidden, resp)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// CSRFMiddleware enforces CSRF protection for session-based authentication.
// It checks the Actor.Type from context (injected by AuthMiddleware).
//
// Validation Logic:
//   - If ActorType == ActorTypeAPIKey: Skip validation (API keys are bearer tokens,
//     not vulnerable to CSRF attacks).
//   - If ActorType == ActorTypeUser (Session): Enforce that the X-CSRF-Token header
//     matches the token stored in the session. Reject with 403 Forbidden if mismatch.
//   - If ActorType == ActorTypeSystem: Skip validation (internal system calls).
//   - If no Actor in context (unauthenticated): Skip validation (AuthMiddleware will
//     handle the 401 response).
//
// Safe Methods: GET, HEAD, OPTIONS requests are exempt from CSRF validation
// as they should not cause state changes.
func (s *Server) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Safe methods are exempt from CSRF validation.
		if isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		// Retrieve the actor from context. If not present, the request is
		// unauthenticated and AuthMiddleware will handle the 401.
		actor, ok := types.GetActor(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		// Only enforce CSRF for session-based (User) authentication.
		switch actor.Type {
		case types.ActorTypeAPIKey, types.ActorTypeSystem:
			// API keys and system actors are not vulnerable to CSRF.
			next.ServeHTTP(w, r)
			return
		case types.ActorTypeUser:
			// Validate CSRF token for session-based requests.
			headerToken := r.Header.Get("X-CSRF-Token")
			sessionToken, hasToken := types.GetSessionCSRFToken(r.Context())

			if !hasToken || headerToken == "" {
				s.Logger.Warn("CSRF token missing",
					slog.String("actor_id", actor.ID),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)

				requestID := types.GetRequestID(r.Context())
				resp := APIErrorResponse{
					Error: ErrorDetail{
						Code:      errCodeCSRFInvalid,
						Message:   "CSRF token is required for this request",
						RequestID: requestID,
					},
				}
				JSON(w, r, http.StatusForbidden, resp)
				return
			}

			// Use constant-time comparison to prevent timing attacks.
			if subtle.ConstantTimeCompare([]byte(headerToken), []byte(sessionToken)) != 1 {
				s.Logger.Warn("CSRF token mismatch",
					slog.String("actor_id", actor.ID),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)

				requestID := types.GetRequestID(r.Context())
				resp := APIErrorResponse{
					Error: ErrorDetail{
						Code:      errCodeCSRFInvalid,
						Message:   "CSRF token is invalid",
						RequestID: requestID,
					},
				}
				JSON(w, r, http.StatusForbidden, resp)
				return
			}

			next.ServeHTTP(w, r)
			return
		default:
			// Unknown actor type: skip CSRF validation and let downstream
			// middleware/handlers handle authorization.
			next.ServeHTTP(w, r)
			return
		}
	})
}

// extractClientIP extracts the client's IP address from the request.
// It first checks the X-Forwarded-For header (using the first entry, which
// is the original client IP when behind a proxy/load balancer). If that
// header is not present, it falls back to RemoteAddr.
//
// The returned IP is always stripped of the port number if present.
func extractClientIP(r *http.Request) string {
	// Check X-Forwarded-For first (standard for proxied requests).
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For can contain multiple IPs: "client, proxy1, proxy2".
		// The first entry is the original client IP.
		parts := strings.SplitN(xff, ",", 2)
		ip := strings.TrimSpace(parts[0])
		if ip != "" {
			return ip
		}
	}

	// Fall back to RemoteAddr, stripping the port if present.
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr may not have a port (e.g., in tests).
		return r.RemoteAddr
	}
	return ip
}

// isSafeMethod returns true for HTTP methods that should not cause state changes
// and are therefore exempt from CSRF validation.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}
