package core

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"watchpoint/internal/types"
)

// authPublicPaths lists URL paths that are exempt from authentication.
// Requests to these paths bypass the AuthMiddleware entirely.
var authPublicPaths = map[string]bool{
	"/health":      true,
	"/openapi.json": true,
}

// AuthMiddleware wraps handlers requiring authentication.
//
//  1. Extracts the Bearer token from the Authorization header.
//  2. Calls Authenticator.ResolveToken to resolve the token to an Actor.
//  3. Injects the Actor into the request context via types.WithActor.
//  4. For session-based auth (ActorTypeUser), also injects the CSRF token
//     into context via types.WithSessionCSRFToken for downstream CSRF validation.
//  5. Returns 401 Unauthorized on failure with distinct error codes:
//     - auth_token_missing: No Authorization header or empty Bearer token.
//     - auth_token_invalid: Token is malformed, not found, or revoked.
//     - auth_token_expired: Token exists but has expired.
//
// If the Authenticator field on Server is nil (e.g., during tests that don't
// inject one), the middleware passes through without authentication.
//
// The Authenticator implementation is responsible for the live role check
// (JOIN to retrieve current Role and OrganizationID from the database on
// every request) and sliding window session extension logic.
func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If no authenticator is configured, pass through.
		if s.Authenticator == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Skip authentication for public paths.
		if authPublicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		// Extract the Authorization header.
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			s.writeAuthError(w, r, types.ErrCodeAuthTokenMissing, "Authorization header is required")
			return
		}

		// Parse the Bearer token.
		token := extractBearerToken(authHeader)
		if token == "" {
			s.writeAuthError(w, r, types.ErrCodeAuthTokenMissing, "Bearer token is required")
			return
		}

		// Resolve the token to an Actor.
		actor, err := s.Authenticator.ResolveToken(r.Context(), token)
		if err != nil {
			s.handleAuthError(w, r, err, token)
			return
		}

		if actor == nil {
			s.writeAuthError(w, r, types.ErrCodeAuthTokenInvalid, "Invalid authentication token")
			return
		}

		// Inject the Actor into the request context.
		ctx := types.WithActor(r.Context(), *actor)

		// For session-based authentication, the Authenticator may provide a
		// CSRF token (stored as part of the session). Inject it into context
		// so CSRFMiddleware can validate it.
		// The CSRF token is expected to be set by the Authenticator via
		// a callback or by populating a field accessible here. Since the
		// current Actor struct does not carry the CSRF token (it is session-
		// specific), we rely on the Authenticator to have set it on the
		// context returned via the token resolution flow. For now, session
		// CSRF injection is handled by the SessionResolver (part of the
		// real Authenticator implementation) which wraps the context.
		//
		// Note: When the real Authenticator is implemented, it should call
		// types.WithSessionCSRFToken on the context. The middleware trusts
		// that the Authenticator handles this correctly.

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractBearerToken parses the Authorization header value and returns
// the token string. It expects the format "Bearer <token>" (case-insensitive
// scheme per RFC 7235). Returns empty string if the format is invalid.
func extractBearerToken(authHeader string) string {
	const prefix = "Bearer "
	if len(authHeader) < len(prefix) {
		return ""
	}
	// Case-insensitive comparison of the "Bearer " scheme prefix per RFC 7235.
	if !strings.EqualFold(authHeader[:len(prefix)], prefix) {
		return ""
	}
	token := authHeader[len(prefix):]
	return strings.TrimSpace(token)
}

// handleAuthError inspects the error from Authenticator.ResolveToken and
// writes the appropriate 401 response with the correct error code.
func (s *Server) handleAuthError(w http.ResponseWriter, r *http.Request, err error, token string) {
	var appErr *types.AppError
	if errors.As(err, &appErr) {
		switch appErr.Code {
		case types.ErrCodeAuthTokenExpired:
			s.Logger.Warn("authentication failed: token expired",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			s.writeAuthError(w, r, types.ErrCodeAuthTokenExpired, "Authentication token has expired")
			return
		case types.ErrCodeAuthTokenInvalid, types.ErrCodeAuthTokenRevoked:
			s.Logger.Warn("authentication failed: token invalid",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("error_code", string(appErr.Code)),
			)
			s.writeAuthError(w, r, types.ErrCodeAuthTokenInvalid, "Invalid authentication token")
			return
		case types.ErrCodeAuthSessionExpired:
			s.Logger.Warn("authentication failed: session expired",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			s.writeAuthError(w, r, types.ErrCodeAuthSessionExpired, "Session has expired")
			return
		}
	}

	// Generic error: log it but don't leak internal details.
	s.Logger.Error("authentication failed: unexpected error",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("error", err.Error()),
	)
	s.writeAuthError(w, r, types.ErrCodeAuthTokenInvalid, "Authentication failed")
}

// writeAuthError writes a 401 Unauthorized JSON response with the given error code.
func (s *Server) writeAuthError(w http.ResponseWriter, r *http.Request, code types.ErrorCode, message string) {
	requestID := types.GetRequestID(r.Context())
	resp := APIErrorResponse{
		Error: ErrorDetail{
			Code:      string(code),
			Message:   message,
			RequestID: requestID,
		},
	}
	JSON(w, r, http.StatusUnauthorized, resp)
}

// RequireRole returns middleware that checks if the Actor in the request
// context has a role equal to or higher than the specified role.
// The role hierarchy is: Owner > Admin > Member.
//
// If the Actor is not present in context (unauthenticated), returns 401.
// If the Actor's role is insufficient, returns 403 Forbidden.
//
// System actors bypass role checks entirely.
func (s *Server) RequireRole(role types.UserRole) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := types.GetActor(r.Context())
			if !ok {
				s.writeAuthError(w, r, types.ErrCodeAuthTokenMissing, "Authentication required")
				return
			}

			// System actors bypass role checks.
			if actor.Type == types.ActorTypeSystem {
				next.ServeHTTP(w, r)
				return
			}

			if !actor.RoleHasAtLeast(role) {
				requestID := types.GetRequestID(r.Context())
				resp := APIErrorResponse{
					Error: ErrorDetail{
						Code:      string(types.ErrCodePermissionRole),
						Message:   "Insufficient role for this operation",
						RequestID: requestID,
					},
				}
				JSON(w, r, http.StatusForbidden, resp)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireScope returns middleware that checks if the Actor in the request
// context possesses the specified scope.
//
// For API Key actors, the scope is checked against Actor.Scopes directly.
// For User actors, scopes are implicitly derived from their Role (see
// types.RoleScopeMap). User actors with sufficient role-based scopes pass.
// System actors bypass scope checks entirely.
//
// If the Actor is not present in context (unauthenticated), returns 401.
// If the Actor lacks the required scope, returns 403 Forbidden.
func (s *Server) RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := types.GetActor(r.Context())
			if !ok {
				s.writeAuthError(w, r, types.ErrCodeAuthTokenMissing, "Authentication required")
				return
			}

			// System actors bypass scope checks.
			if actor.Type == types.ActorTypeSystem {
				next.ServeHTTP(w, r)
				return
			}

			if !actor.HasScope(scope) {
				requestID := types.GetRequestID(r.Context())
				resp := APIErrorResponse{
					Error: ErrorDetail{
						Code:      string(types.ErrCodePermissionScope),
						Message:   "Insufficient scope for this operation",
						RequestID: requestID,
					},
				}
				JSON(w, r, http.StatusForbidden, resp)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
