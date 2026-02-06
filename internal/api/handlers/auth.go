// Package handlers contains the HTTP handler implementations for the WatchPoint API.
//
// Each handler is responsible for:
//   - Decoding and validating HTTP requests
//   - Delegating to service-layer logic
//   - Encoding responses and managing HTTP-specific concerns (headers, cookies)
package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"watchpoint/internal/auth"
	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// --- DTOs ---

// LoginRequest is the request body for POST /auth/login.
// Per 05f-api-auth.md Section 3.1.
type LoginRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

// AuthResponse is the unified response for Login, OAuth Callback, and Accept Invite.
// Per 05f-api-auth.md Section 3.1: Session ID is returned ONLY via HttpOnly/Secure
// Set-Cookie header. It is NOT included in the JSON response body.
type AuthResponse struct {
	CSRFToken string      `json:"csrf_token"`
	User      *types.User `json:"user"`
	ExpiresAt time.Time   `json:"expires_at"`
}

// AcceptInviteRequest is the request body for POST /auth/accept-invite.
// Per 05f-api-auth.md Section 3.2.
type AcceptInviteRequest struct {
	Token    string `json:"token" validate:"required"`
	Name     string `json:"name" validate:"required,max=100"`
	Password string `json:"password" validate:"required,min=8,max=72"`
}

// ForgotPasswordRequest is the request body for POST /auth/forgot-password.
// Per 05f-api-auth.md Section 3.2.
type ForgotPasswordRequest struct {
	Email string `json:"email" validate:"required,email"`
}

// ResetPasswordRequest is the request body for POST /auth/reset-password.
// Per 05f-api-auth.md Section 3.2.
type ResetPasswordRequest struct {
	Token       string `json:"token" validate:"required"`
	NewPassword string `json:"new_password" validate:"required,min=8,max=72"`
}

// --- Service Interfaces ---
//
// These interfaces allow the handler to depend on abstractions rather than
// concrete service implementations, enabling testability via mocks.

// AuthService orchestrates credential validation and lifecycle flows.
// Mirrors the interface defined in 05f-api-auth.md Section 4.2.
type AuthService interface {
	// Login verifies credentials and returns the User and Session.
	Login(ctx context.Context, email, password, ip string) (*types.User, *types.Session, error)

	// AcceptInvite validates the invite token, sets user password, and activates the account.
	AcceptInvite(ctx context.Context, token, name, password, ip string) (*types.User, *types.Session, error)

	// RequestPasswordReset initiates a password reset flow.
	RequestPasswordReset(ctx context.Context, email string) error

	// CompletePasswordReset validates the reset token and sets the new password.
	CompletePasswordReset(ctx context.Context, token, newPassword string) error
}

// SessionService manages session tokens, cookies, and CSRF.
// Mirrors the interface defined in 05f-api-auth.md Section 4.2.
type SessionService interface {
	InvalidateSession(ctx context.Context, sessionID string) error
}

// OAuthProvider abstracts a single OAuth provider.
// Per 05f-api-auth.md Section 8.1.
type OAuthProvider interface {
	Name() string
	GetLoginURL(state string) string
	Exchange(ctx context.Context, code string) (*types.OAuthProfile, error)
}

// OAuthManager provides access to configured OAuth providers.
// Per 05f-api-auth.md Section 8.1.
type OAuthManager interface {
	GetProvider(name string) (OAuthProvider, error)
}

// --- Cookie Configuration ---

// CookieConfig defines security attributes for session cookies.
// Per 05f-api-auth.md Section 5.
type CookieConfig struct {
	Name     string
	Domain   string
	Secure   bool
	HttpOnly bool
	SameSite http.SameSite
	MaxAge   int // seconds
	Path     string
}

// DefaultCookieConfig returns the default cookie configuration.
// Per 05f-api-auth.md Section 5: HttpOnly=true, Secure=true, SameSite=Lax, MaxAge=604800 (7 days).
func DefaultCookieConfig() CookieConfig {
	return CookieConfig{
		Name:     "session_id",
		Domain:   "",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   604800, // 7 days
		Path:     "/",
	}
}

// --- Handler ---

// AuthHandler maps HTTP requests to the auth Service layer and handles
// cookie/header management.
// Per 05f-api-auth.md Section 5.
type AuthHandler struct {
	authService    AuthService
	sessionService SessionService
	oauthManager   OAuthManager
	cookieConfig   CookieConfig
	logger         *slog.Logger
	validator      *core.Validator
}

// NewAuthHandler creates a new AuthHandler with the provided dependencies.
// Per 05f-api-auth.md Section 5.
func NewAuthHandler(
	authSvc AuthService,
	sessSvc SessionService,
	oauth OAuthManager,
	cfg CookieConfig,
	l *slog.Logger,
	v *core.Validator,
) *AuthHandler {
	if l == nil {
		l = slog.Default()
	}
	return &AuthHandler{
		authService:    authSvc,
		sessionService: sessSvc,
		oauthManager:   oauth,
		cookieConfig:   cfg,
		logger:         l,
		validator:      v,
	}
}

// RegisterRoutes mounts all auth routes onto the provided router.
// Per 05f-api-auth.md Section 6.
//
// Public Routes (no session required):
//   - POST /login          - Credential login
//   - POST /forgot-password - Initiate password reset
//   - POST /reset-password  - Complete password reset
//   - POST /accept-invite   - Accept organization invite
//   - GET  /oauth/{provider}/login    - OAuth redirect
//   - GET  /oauth/{provider}/callback - OAuth callback
//
// Protected Routes (requires valid session):
//   - POST /logout - Session logout
func (h *AuthHandler) RegisterRoutes(r chi.Router) {
	// Public Routes
	r.Group(func(r chi.Router) {
		r.Post("/login", h.HandleLogin)
		r.Post("/forgot-password", h.HandleForgotPassword)
		r.Post("/reset-password", h.HandleResetPassword)
		r.Post("/accept-invite", h.HandleAcceptInvite)
		r.Get("/oauth/{provider}/login", h.HandleOAuthLogin)
		r.Get("/oauth/{provider}/callback", h.HandleOAuthCallback)
	})

	// Protected Routes
	r.Group(func(r chi.Router) {
		r.Post("/logout", h.HandleLogout)
	})
}

// --- Handler Methods ---

// HandleLogin processes POST /auth/login requests.
//
// Per USER-006 flow simulation and 05f-api-auth.md:
//  1. Decode and validate the LoginRequest.
//  2. Canonicalize the email (Section 7.3).
//  3. Extract client IP for security tracking.
//  4. Call AuthService.Login.
//  5. On success: set HttpOnly cookie with session ID and return AuthResponse.
//  6. On failure: map error to appropriate HTTP status (Section 7.4).
//
// Session ID is returned ONLY via Set-Cookie header (Section 7.2).
func (h *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	// Canonicalize email per Section 7.3
	email := auth.CanonicalizeEmail(req.Email)

	// Extract client IP for security event tracking
	ip := extractClientIP(r)

	user, session, err := h.authService.Login(r.Context(), email, req.Password, ip)
	if err != nil {
		h.handleAuthError(w, r, err)
		return
	}

	// Set the session cookie (Session ID ONLY via cookie, per Section 7.2)
	h.setSessionCookie(w, session.ID)

	// Return the AuthResponse with CSRF token and user info
	resp := AuthResponse{
		CSRFToken: session.CSRFToken,
		User:      user,
		ExpiresAt: session.ExpiresAt,
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: resp})
}

// HandleLogout processes POST /auth/logout requests.
//
// Per DASH-004 flow simulation:
//  1. Extract session ID from cookie.
//  2. Call SessionService.InvalidateSession (hard delete).
//  3. Clear the client cookie (Max-Age=0, Expires=epoch).
//  4. Return 200 OK.
func (h *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	// Extract session ID from cookie
	sessionID := h.extractSessionIDFromCookie(r)
	if sessionID == "" {
		// No session cookie present; nothing to invalidate.
		// Still clear any residual cookie and return success.
		h.clearSessionCookie(w)
		core.JSON(w, r, http.StatusOK, core.APIResponse{
			Data: map[string]string{"message": "logged out"},
		})
		return
	}

	// Invalidate the session in the database (hard delete)
	if err := h.sessionService.InvalidateSession(r.Context(), sessionID); err != nil {
		h.logger.Warn("failed to invalidate session during logout",
			"session_id", sessionID,
			"error", err,
		)
		// Continue with cookie clearing even if DB invalidation fails.
		// The session will expire naturally or be cleaned up by the scheduled job.
	}

	// Clear the client cookie per Section 7.2:
	// Set-Cookie: session_id=; Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:00 GMT
	h.clearSessionCookie(w)

	core.JSON(w, r, http.StatusOK, core.APIResponse{
		Data: map[string]string{"message": "logged out"},
	})
}

// HandleOAuthCallback processes GET /auth/oauth/{provider}/callback requests.
//
// Per USER-007 flow simulation and 05f-api-auth.md Section 8:
//  1. Extract provider from URL path.
//  2. Validate state parameter against oauth_state cookie (CSRF protection).
//  3. Exchange authorization code for OAuthProfile.
//  4. Call AuthService.LoginWithOAuth.
//  5. Set session cookie and return AuthResponse.
//
// NOTE: This is a scaffold implementation. Full OAuth wiring (state validation,
// code exchange) will be completed when the OAuthManager is implemented.
func (h *AuthHandler) HandleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	providerName := chi.URLParam(r, "provider")
	if providerName == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"provider is required",
			nil,
		))
		return
	}

	if h.oauthManager == nil {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"OAuth is not configured",
			nil,
		))
		return
	}

	provider, err := h.oauthManager.GetProvider(providerName)
	if err != nil {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"unsupported OAuth provider: "+providerName,
			err,
		))
		return
	}

	// Step 2: Validate state parameter against oauth_state cookie (CSRF protection)
	stateParam := r.URL.Query().Get("state")
	stateCookie, cookieErr := r.Cookie("oauth_state")
	if cookieErr != nil || stateCookie.Value == "" || stateParam == "" || stateParam != stateCookie.Value {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthTokenInvalid,
			"invalid OAuth state parameter",
			nil,
		))
		return
	}

	// Clear the oauth_state cookie after validation
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    "",
		MaxAge:   -1,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cookieConfig.Secure,
		SameSite: h.cookieConfig.SameSite,
	})

	// Step 3: Exchange authorization code for OAuthProfile
	code := r.URL.Query().Get("code")
	if code == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"authorization code is required",
			nil,
		))
		return
	}

	profile, err := provider.Exchange(r.Context(), code)
	if err != nil {
		h.logger.Error("OAuth code exchange failed",
			"provider", providerName,
			"error", err,
		)
		core.Error(w, r, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"OAuth authentication failed",
			err,
		))
		return
	}

	// Step 4: Delegate to AuthService.LoginWithOAuth
	// LoginWithOAuth is not yet implemented in the AuthService; this handler
	// scaffolds the complete flow. When the service method is available, this
	// will call it and handle the session/cookie response identically to HandleLogin.
	_ = profile
	core.Error(w, r, types.NewAppError(
		types.ErrCodeInternalUnexpected,
		"OAuth login flow not yet fully implemented",
		nil,
	))
}

// HandleOAuthLogin processes GET /auth/oauth/{provider}/login requests.
//
// Per 05f-api-auth.md Section 8.2:
//  1. Generate a random state token.
//  2. Set a short-lived oauth_state cookie with the state value.
//  3. Redirect the user to the provider's login URL.
//
// NOTE: This is a scaffold implementation. Full OAuth state generation
// will be completed when the OAuthManager and TokenGenerator are fully wired.
func (h *AuthHandler) HandleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	providerName := chi.URLParam(r, "provider")
	if providerName == "" {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"provider is required",
			nil,
		))
		return
	}

	if h.oauthManager == nil {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"OAuth is not configured",
			nil,
		))
		return
	}

	provider, err := h.oauthManager.GetProvider(providerName)
	if err != nil {
		core.Error(w, r, types.NewAppError(
			types.ErrCodeValidationMissingField,
			"unsupported OAuth provider: "+providerName,
			err,
		))
		return
	}

	// Generate state (for CSRF protection). A simple random approach using
	// the request ID as a fallback. The proper TokenGenerator-based approach
	// will be wired when the full OAuth infrastructure is complete.
	state := types.GetRequestID(r.Context())
	if state == "" {
		state = "fallback_state"
	}

	// Set short-lived oauth_state cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		MaxAge:   600, // 10 minutes
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cookieConfig.Secure,
		SameSite: h.cookieConfig.SameSite,
	})

	// Redirect to provider's login URL
	loginURL := provider.GetLoginURL(state)
	http.Redirect(w, r, loginURL, http.StatusTemporaryRedirect)
}

// HandleAcceptInvite processes POST /auth/accept-invite requests.
//
// Per USER-005 flow simulation and 05f-api-auth.md Section 3.2:
//  1. Decode and validate the AcceptInviteRequest.
//  2. Call AuthService.AcceptInvite.
//  3. On success: set session cookie and return AuthResponse.
func (h *AuthHandler) HandleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	var req AcceptInviteRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	ip := extractClientIP(r)

	user, session, err := h.authService.AcceptInvite(r.Context(), req.Token, req.Name, req.Password, ip)
	if err != nil {
		h.handleAuthError(w, r, err)
		return
	}

	// Set the session cookie
	h.setSessionCookie(w, session.ID)

	resp := AuthResponse{
		CSRFToken: session.CSRFToken,
		User:      user,
		ExpiresAt: session.ExpiresAt,
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{Data: resp})
}

// HandleForgotPassword processes POST /auth/forgot-password requests.
//
// Per 05f-api-auth.md Section 7.1 (Enumeration Protection):
// Always returns a generic success message regardless of whether the
// email exists. This prevents email enumeration attacks.
func (h *AuthHandler) HandleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req ForgotPasswordRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	email := auth.CanonicalizeEmail(req.Email)

	// Call the service -- errors are intentionally swallowed to prevent enumeration.
	// The service handles rate limiting on the email key internally.
	if err := h.authService.RequestPasswordReset(r.Context(), email); err != nil {
		h.logger.Warn("password reset request failed",
			"email", email,
			"error", err,
		)
		// Fall through to return success regardless
	}

	// Always return success to prevent enumeration (Section 7.1)
	core.JSON(w, r, http.StatusOK, core.APIResponse{
		Data: map[string]string{
			"message": "If an account exists with that email, a password reset link has been sent.",
		},
	})
}

// HandleResetPassword processes POST /auth/reset-password requests.
func (h *AuthHandler) HandleResetPassword(w http.ResponseWriter, r *http.Request) {
	var req ResetPasswordRequest
	if err := core.DecodeJSON(w, r, &req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.validator.ValidateStruct(req); err != nil {
		core.Error(w, r, err)
		return
	}

	if err := h.authService.CompletePasswordReset(r.Context(), req.Token, req.NewPassword); err != nil {
		h.handleAuthError(w, r, err)
		return
	}

	core.JSON(w, r, http.StatusOK, core.APIResponse{
		Data: map[string]string{
			"message": "Password has been reset successfully.",
		},
	})
}

// --- Cookie Helpers ---

// setSessionCookie writes the session cookie to the response.
// Per Section 7.2: Session ID is returned ONLY via HttpOnly/Secure Set-Cookie header.
func (h *AuthHandler) setSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookieConfig.Name,
		Value:    sessionID,
		MaxAge:   h.cookieConfig.MaxAge,
		Path:     h.cookieConfig.Path,
		Domain:   h.cookieConfig.Domain,
		Secure:   h.cookieConfig.Secure,
		HttpOnly: h.cookieConfig.HttpOnly,
		SameSite: h.cookieConfig.SameSite,
	})
}

// clearSessionCookie writes a cookie with Max-Age=0 and Expires=epoch to force
// immediate browser deletion of the session cookie.
// Per Section 7.2 and DASH-004 flow simulation.
func (h *AuthHandler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookieConfig.Name,
		Value:    "",
		MaxAge:   -1, // Forces deletion (equivalent to Max-Age=0 in the spec)
		Expires:  time.Unix(0, 0).UTC(), // Thu, 01 Jan 1970 00:00:00 GMT
		Path:     h.cookieConfig.Path,
		Domain:   h.cookieConfig.Domain,
		Secure:   h.cookieConfig.Secure,
		HttpOnly: h.cookieConfig.HttpOnly,
		SameSite: h.cookieConfig.SameSite,
	})
}

// extractSessionIDFromCookie reads the session ID from the request cookie.
func (h *AuthHandler) extractSessionIDFromCookie(r *http.Request) string {
	cookie, err := r.Cookie(h.cookieConfig.Name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// --- Error Handling ---

// handleAuthError maps internal auth errors to appropriate HTTP responses.
// Per 05f-api-auth.md Section 7.4:
//
//	ErrAuthLocked          -> 429 "Too many failed attempts. Try again later."
//	ErrAuthInvalidCreds    -> 401 "Invalid email or password."
//	ErrAuthUserNotFound    -> 401 "Invalid email or password." (Masked)
//	ErrAuthAccountNotActive -> 403 "Account not active. Please check your invite."
//	ErrAuthEmailNotVerified -> 403 "Social login email not verified."
//
// The error mapping for HTTP status codes is already defined in types.ErrorCode.HTTPStatus(),
// so we use core.Error which delegates to that mapping. The custom messages below
// override the internal error messages with user-facing messages per Section 7.4.
func (h *AuthHandler) handleAuthError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		core.Error(w, r, err)
		return
	}

	// Map internal error codes to user-facing messages per Section 7.4
	switch appErr.Code {
	case types.ErrCodeAuthLocked:
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthLocked,
			"Too many failed attempts. Try again later.",
			nil,
		))
	case types.ErrCodeAuthInvalidCreds, types.ErrCodeAuthUserNotFound:
		// Both map to 401 with a generic message for enumeration protection
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthInvalidCreds,
			"Invalid email or password.",
			nil,
		))
	case types.ErrCodeAuthAccountNotActive:
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthAccountNotActive,
			"Account not active. Please check your invite.",
			nil,
		))
	case types.ErrCodeAuthEmailNotVerified:
		core.Error(w, r, types.NewAppError(
			types.ErrCodeAuthEmailNotVerified,
			"Social login email not verified.",
			nil,
		))
	default:
		core.Error(w, r, err)
	}
}

// --- Utility ---

// extractClientIP extracts the client IP from the request.
// It checks X-Forwarded-For first (for requests behind a proxy/ALB),
// then falls back to RemoteAddr.
func extractClientIP(r *http.Request) string {
	// X-Forwarded-For may contain multiple IPs: client, proxy1, proxy2
	// The first entry is the original client IP.
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		ip := strings.TrimSpace(parts[0])
		if ip != "" {
			return ip
		}
	}

	// Fall back to RemoteAddr (may include port: "ip:port")
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}
