package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"watchpoint/internal/core"
	"watchpoint/internal/types"
)

// =============================================================================
// Mock Implementations
// =============================================================================

// mockAuthService implements the AuthService interface for testing.
type mockAuthService struct {
	loginFn                  func(ctx context.Context, email, password, ip string) (*types.User, *types.Session, error)
	acceptInviteFn           func(ctx context.Context, token, name, password, ip string) (*types.User, *types.Session, error)
	requestPasswordResetFn   func(ctx context.Context, email string) error
	completePasswordResetFn  func(ctx context.Context, token, newPassword string) error
}

func (m *mockAuthService) Login(ctx context.Context, email, password, ip string) (*types.User, *types.Session, error) {
	if m.loginFn != nil {
		return m.loginFn(ctx, email, password, ip)
	}
	return nil, nil, errors.New("Login not mocked")
}

func (m *mockAuthService) AcceptInvite(ctx context.Context, token, name, password, ip string) (*types.User, *types.Session, error) {
	if m.acceptInviteFn != nil {
		return m.acceptInviteFn(ctx, token, name, password, ip)
	}
	return nil, nil, errors.New("AcceptInvite not mocked")
}

func (m *mockAuthService) RequestPasswordReset(ctx context.Context, email string) error {
	if m.requestPasswordResetFn != nil {
		return m.requestPasswordResetFn(ctx, email)
	}
	return nil
}

func (m *mockAuthService) CompletePasswordReset(ctx context.Context, token, newPassword string) error {
	if m.completePasswordResetFn != nil {
		return m.completePasswordResetFn(ctx, token, newPassword)
	}
	return nil
}

// mockSessionService implements the SessionService interface for testing.
type mockSessionService struct {
	invalidateSessionFn func(ctx context.Context, sessionID string) error
}

func (m *mockSessionService) InvalidateSession(ctx context.Context, sessionID string) error {
	if m.invalidateSessionFn != nil {
		return m.invalidateSessionFn(ctx, sessionID)
	}
	return nil
}

// mockOAuthProvider implements OAuthProvider for testing.
type mockOAuthProvider struct {
	name     string
	loginURL string
	profile  *types.OAuthProfile
	err      error
}

func (m *mockOAuthProvider) Name() string                                          { return m.name }
func (m *mockOAuthProvider) GetLoginURL(state string) string                       { return m.loginURL + "?state=" + state }
func (m *mockOAuthProvider) Exchange(_ context.Context, _ string) (*types.OAuthProfile, error) {
	return m.profile, m.err
}

// mockOAuthManager implements OAuthManager for testing.
type mockOAuthManager struct {
	providers map[string]OAuthProvider
}

func (m *mockOAuthManager) GetProvider(name string) (OAuthProvider, error) {
	p, ok := m.providers[name]
	if !ok {
		return nil, errors.New("unknown provider: " + name)
	}
	return p, nil
}

// =============================================================================
// Test Helpers
// =============================================================================

func newTestHandler(authSvc *mockAuthService, sessSvc *mockSessionService) *AuthHandler {
	return NewAuthHandler(
		authSvc,
		sessSvc,
		nil, // oauthManager - nil unless explicitly set in test
		DefaultCookieConfig(),
		nil, // logger
		core.NewValidator(nil),
	)
}

func testUser() *types.User {
	return &types.User{
		ID:             "user_test123",
		OrganizationID: "org_test456",
		Email:          "test@example.com",
		Name:           "Test User",
		Role:           types.RoleOwner,
		Status:         types.UserStatusActive,
		CreatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func testSession() *types.Session {
	return &types.Session{
		ID:             "sess_test_session_abc",
		UserID:         "user_test123",
		OrganizationID: "org_test456",
		CSRFToken:      "csrf_token_xyz",
		ExpiresAt:      time.Date(2026, 2, 13, 12, 0, 0, 0, time.UTC),
		CreatedAt:      time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC),
	}
}

// findCookie searches the response recorder's Set-Cookie headers for the named cookie.
func findCookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

// =============================================================================
// HandleLogin Tests
// =============================================================================

func TestHandleLogin_Success(t *testing.T) {
	user := testUser()
	session := testSession()

	authSvc := &mockAuthService{
		loginFn: func(_ context.Context, email, password, ip string) (*types.User, *types.Session, error) {
			if email != "test@example.com" {
				t.Errorf("expected email 'test@example.com', got %q", email)
			}
			if password != "correct_password" {
				t.Errorf("expected password 'correct_password', got %q", password)
			}
			return user, session, nil
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"email":"Test@Example.com","password":"correct_password"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	// Verify HTTP status
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", w.Code, w.Body.String())
	}

	// Verify Content-Type
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	// Verify response body contains CSRF token and user
	var resp struct {
		Data AuthResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Data.CSRFToken != "csrf_token_xyz" {
		t.Errorf("expected CSRF token 'csrf_token_xyz', got %q", resp.Data.CSRFToken)
	}
	if resp.Data.User == nil {
		t.Fatal("expected user in response, got nil")
	}
	if resp.Data.User.ID != "user_test123" {
		t.Errorf("expected user ID 'user_test123', got %q", resp.Data.User.ID)
	}
	if resp.Data.ExpiresAt.IsZero() {
		t.Error("expected non-zero ExpiresAt")
	}

	// Verify Set-Cookie header
	cookie := findCookie(w, "session_id")
	if cookie == nil {
		t.Fatal("expected session_id cookie to be set")
	}
	if cookie.Value != "sess_test_session_abc" {
		t.Errorf("expected cookie value 'sess_test_session_abc', got %q", cookie.Value)
	}
	if !cookie.HttpOnly {
		t.Error("expected HttpOnly cookie")
	}
	if !cookie.Secure {
		t.Error("expected Secure cookie")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("expected SameSite=Lax, got %d", cookie.SameSite)
	}
	if cookie.MaxAge != 604800 {
		t.Errorf("expected MaxAge=604800, got %d", cookie.MaxAge)
	}
}

func TestHandleLogin_EmailCanonicalisation(t *testing.T) {
	// Verify that email is canonicalized (lowercased) before being passed to the service.
	// Note: The go-playground/validator email tag rejects whitespace-padded emails,
	// so we test only case normalization here. Whitespace trimming is handled by
	// CanonicalizeEmail for emails that arrive via other channels.
	var receivedEmail string
	authSvc := &mockAuthService{
		loginFn: func(_ context.Context, email, _, _ string) (*types.User, *types.Session, error) {
			receivedEmail = email
			return testUser(), testSession(), nil
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"email":"Test@EXAMPLE.COM","password":"password123"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}

	if receivedEmail != "test@example.com" {
		t.Errorf("expected canonicalized email 'test@example.com', got %q", receivedEmail)
	}
}

func TestHandleLogin_InvalidCreds(t *testing.T) {
	authSvc := &mockAuthService{
		loginFn: func(_ context.Context, _, _, _ string) (*types.User, *types.Session, error) {
			return nil, nil, types.NewAppError(types.ErrCodeAuthInvalidCreds, "invalid email or password", nil)
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"email":"test@example.com","password":"wrong_password"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d; body: %s", w.Code, w.Body.String())
	}

	var errResp core.APIErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if errResp.Error.Code != string(types.ErrCodeAuthInvalidCreds) {
		t.Errorf("expected error code %q, got %q", types.ErrCodeAuthInvalidCreds, errResp.Error.Code)
	}
	if errResp.Error.Message != "Invalid email or password." {
		t.Errorf("expected generic message 'Invalid email or password.', got %q", errResp.Error.Message)
	}

	// Verify no session cookie is set on failure
	cookie := findCookie(w, "session_id")
	if cookie != nil {
		t.Error("expected no session cookie on failed login")
	}
}

func TestHandleLogin_AccountLocked(t *testing.T) {
	authSvc := &mockAuthService{
		loginFn: func(_ context.Context, _, _, _ string) (*types.User, *types.Session, error) {
			return nil, nil, types.NewAppError(types.ErrCodeAuthLocked, "account locked", nil)
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"email":"test@example.com","password":"any"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}

	var errResp core.APIErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Message != "Too many failed attempts. Try again later." {
		t.Errorf("expected lockout message, got %q", errResp.Error.Message)
	}
}

func TestHandleLogin_AccountNotActive(t *testing.T) {
	authSvc := &mockAuthService{
		loginFn: func(_ context.Context, _, _, _ string) (*types.User, *types.Session, error) {
			return nil, nil, types.NewAppError(types.ErrCodeAuthAccountNotActive, "not active", nil)
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"email":"test@example.com","password":"any"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandleLogin_InvalidJSON(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleLogin_EmptyBody(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleLogin_MissingEmail(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	body := `{"password":"password123"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleLogin_MissingPassword(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	body := `{"email":"test@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleLogin_InvalidEmail(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	body := `{"email":"not-an-email","password":"password123"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleLogin_UserNotFoundMasked(t *testing.T) {
	// Verify that ErrAuthUserNotFound is masked as ErrAuthInvalidCreds
	authSvc := &mockAuthService{
		loginFn: func(_ context.Context, _, _, _ string) (*types.User, *types.Session, error) {
			return nil, nil, types.NewAppError(types.ErrCodeAuthUserNotFound, "user not found", nil)
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"email":"test@example.com","password":"password"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	var errResp core.APIErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	// Should be masked as invalid_credentials, NOT user_not_found
	if errResp.Error.Code != string(types.ErrCodeAuthInvalidCreds) {
		t.Errorf("expected masked error code %q, got %q", types.ErrCodeAuthInvalidCreds, errResp.Error.Code)
	}
}

func TestHandleLogin_ClientIPExtraction(t *testing.T) {
	var receivedIP string
	authSvc := &mockAuthService{
		loginFn: func(_ context.Context, _, _, ip string) (*types.User, *types.Session, error) {
			receivedIP = ip
			return testUser(), testSession(), nil
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"email":"test@example.com","password":"password"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.50, 10.0.0.1")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	if receivedIP != "203.0.113.50" {
		t.Errorf("expected IP '203.0.113.50', got %q", receivedIP)
	}
}

func TestHandleLogin_SessionIDNotInBody(t *testing.T) {
	// Per Section 7.2: Session ID must NOT appear in the JSON response body.
	authSvc := &mockAuthService{
		loginFn: func(_ context.Context, _, _, _ string) (*types.User, *types.Session, error) {
			return testUser(), testSession(), nil
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"email":"test@example.com","password":"password"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleLogin(w, req)

	responseBody := w.Body.String()
	if strings.Contains(responseBody, "sess_test_session_abc") {
		t.Error("session ID must not appear in the JSON response body")
	}
}

// =============================================================================
// HandleLogout Tests
// =============================================================================

func TestHandleLogout_Success(t *testing.T) {
	var invalidatedSession string
	sessSvc := &mockSessionService{
		invalidateSessionFn: func(_ context.Context, sessionID string) error {
			invalidatedSession = sessionID
			return nil
		},
	}
	handler := newTestHandler(&mockAuthService{}, sessSvc)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	// Set the session cookie that the handler will read
	req.AddCookie(&http.Cookie{
		Name:  "session_id",
		Value: "sess_active_session_123",
	})
	w := httptest.NewRecorder()

	handler.HandleLogout(w, req)

	// Verify HTTP status
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}

	// Verify session was invalidated
	if invalidatedSession != "sess_active_session_123" {
		t.Errorf("expected session 'sess_active_session_123' to be invalidated, got %q", invalidatedSession)
	}

	// Verify the session cookie is cleared
	cookie := findCookie(w, "session_id")
	if cookie == nil {
		t.Fatal("expected Set-Cookie header for session_id")
	}
	if cookie.Value != "" {
		t.Errorf("expected empty cookie value, got %q", cookie.Value)
	}
	if cookie.MaxAge != -1 {
		t.Errorf("expected MaxAge=-1 (deletion), got %d", cookie.MaxAge)
	}
	if !cookie.Expires.Before(time.Now()) {
		t.Error("expected Expires to be in the past (epoch)")
	}
	if !cookie.HttpOnly {
		t.Error("expected HttpOnly on cleared cookie")
	}
	if !cookie.Secure {
		t.Error("expected Secure on cleared cookie")
	}
}

func TestHandleLogout_NoCookie(t *testing.T) {
	sessSvc := &mockSessionService{
		invalidateSessionFn: func(_ context.Context, _ string) error {
			t.Error("InvalidateSession should not be called when no cookie is present")
			return nil
		},
	}
	handler := newTestHandler(&mockAuthService{}, sessSvc)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	// No cookie
	w := httptest.NewRecorder()

	handler.HandleLogout(w, req)

	// Should still return 200 and clear the cookie
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Cookie should still be cleared (defensive)
	cookie := findCookie(w, "session_id")
	if cookie == nil {
		t.Fatal("expected Set-Cookie header even without session")
	}
}

func TestHandleLogout_InvalidationError_StillClearsCookie(t *testing.T) {
	// Even if the DB invalidation fails, the cookie should still be cleared.
	sessSvc := &mockSessionService{
		invalidateSessionFn: func(_ context.Context, _ string) error {
			return errors.New("database error")
		},
	}
	handler := newTestHandler(&mockAuthService{}, sessSvc)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{
		Name:  "session_id",
		Value: "sess_failing_session",
	})
	w := httptest.NewRecorder()

	handler.HandleLogout(w, req)

	// Should still return 200
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 despite DB error, got %d", w.Code)
	}

	// Cookie should be cleared
	cookie := findCookie(w, "session_id")
	if cookie == nil {
		t.Fatal("expected cookie to be cleared despite DB error")
	}
	if cookie.MaxAge != -1 {
		t.Errorf("expected MaxAge=-1, got %d", cookie.MaxAge)
	}
}

// =============================================================================
// HandleAcceptInvite Tests
// =============================================================================

func TestHandleAcceptInvite_Success(t *testing.T) {
	user := testUser()
	session := testSession()

	authSvc := &mockAuthService{
		acceptInviteFn: func(_ context.Context, token, name, password, _ string) (*types.User, *types.Session, error) {
			if token != "raw_invite_token_xyz" {
				t.Errorf("expected token 'raw_invite_token_xyz', got %q", token)
			}
			if name != "John Doe" {
				t.Errorf("expected name 'John Doe', got %q", name)
			}
			if password != "SecurePassword1!" {
				t.Errorf("expected password 'SecurePassword1!', got %q", password)
			}
			return user, session, nil
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"token":"raw_invite_token_xyz","name":"John Doe","password":"SecurePassword1!"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/accept-invite", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAcceptInvite(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}

	// Verify cookie is set
	cookie := findCookie(w, "session_id")
	if cookie == nil {
		t.Fatal("expected session cookie to be set")
	}
	if cookie.Value != "sess_test_session_abc" {
		t.Errorf("expected session ID in cookie, got %q", cookie.Value)
	}
}

func TestHandleAcceptInvite_ExpiredToken(t *testing.T) {
	authSvc := &mockAuthService{
		acceptInviteFn: func(_ context.Context, _, _, _, _ string) (*types.User, *types.Session, error) {
			return nil, nil, types.NewAppError(types.ErrCodeAuthTokenExpired, "invite expired", nil)
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"token":"expired_token","name":"Jane","password":"Password1!"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/accept-invite", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAcceptInvite(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleAcceptInvite_ShortPassword(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	body := `{"token":"valid_token","name":"Jane","password":"short"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/accept-invite", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAcceptInvite(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for short password, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleAcceptInvite_MissingFields(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	// Missing name
	body := `{"token":"valid","password":"Password1!"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/accept-invite", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAcceptInvite(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// =============================================================================
// HandleForgotPassword Tests
// =============================================================================

func TestHandleForgotPassword_AlwaysReturnsSuccess(t *testing.T) {
	// Per Section 7.1: Always returns success to prevent email enumeration.
	handler := newTestHandler(&mockAuthService{
		requestPasswordResetFn: func(_ context.Context, _ string) error {
			return errors.New("user not found")
		},
	}, &mockSessionService{})

	body := `{"email":"nonexistent@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/forgot-password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleForgotPassword(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (enumeration protection), got %d", w.Code)
	}
}

func TestHandleForgotPassword_InvalidEmail(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	body := `{"email":"not-an-email"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/forgot-password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleForgotPassword(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// =============================================================================
// HandleResetPassword Tests
// =============================================================================

func TestHandleResetPassword_Success(t *testing.T) {
	authSvc := &mockAuthService{
		completePasswordResetFn: func(_ context.Context, token, newPassword string) error {
			if token != "valid_reset_token" {
				t.Errorf("expected token 'valid_reset_token', got %q", token)
			}
			return nil
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"token":"valid_reset_token","new_password":"NewPassword1!"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/reset-password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResetPassword(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleResetPassword_InvalidToken(t *testing.T) {
	authSvc := &mockAuthService{
		completePasswordResetFn: func(_ context.Context, _, _ string) error {
			return types.NewAppError(types.ErrCodeAuthTokenInvalid, "invalid reset token", nil)
		},
	}
	handler := newTestHandler(authSvc, &mockSessionService{})

	body := `{"token":"bad_token","new_password":"NewPassword1!"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/reset-password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResetPassword(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleResetPassword_ShortPassword(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	body := `{"token":"valid","new_password":"short"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/reset-password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResetPassword(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for short password, got %d", w.Code)
	}
}

// =============================================================================
// HandleOAuthCallback Tests
// =============================================================================

func TestHandleOAuthCallback_MissingProvider(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	req := httptest.NewRequest(http.MethodGet, "/auth/oauth//callback", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("provider", "")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.HandleOAuthCallback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleOAuthCallback_NilOAuthManager(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})

	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/google/callback", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("provider", "google")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.HandleOAuthCallback(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when OAuth not configured, got %d", w.Code)
	}
}

func TestHandleOAuthCallback_InvalidState(t *testing.T) {
	oauthMgr := &mockOAuthManager{
		providers: map[string]OAuthProvider{
			"google": &mockOAuthProvider{name: "google"},
		},
	}
	handler := NewAuthHandler(
		&mockAuthService{},
		&mockSessionService{},
		oauthMgr,
		DefaultCookieConfig(),
		nil,
		core.NewValidator(nil),
	)

	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/google/callback?state=bad_state&code=authcode123", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("provider", "google")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	// Set a cookie with a different state value
	req.AddCookie(&http.Cookie{Name: "oauth_state", Value: "correct_state"})
	w := httptest.NewRecorder()

	handler.HandleOAuthCallback(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for state mismatch, got %d; body: %s", w.Code, w.Body.String())
	}
}

// =============================================================================
// HandleOAuthLogin Tests
// =============================================================================

func TestHandleOAuthLogin_Redirect(t *testing.T) {
	oauthMgr := &mockOAuthManager{
		providers: map[string]OAuthProvider{
			"google": &mockOAuthProvider{
				name:     "google",
				loginURL: "https://accounts.google.com/o/oauth2/auth",
			},
		},
	}
	handler := NewAuthHandler(
		&mockAuthService{},
		&mockSessionService{},
		oauthMgr,
		DefaultCookieConfig(),
		nil,
		core.NewValidator(nil),
	)

	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/google/login", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("provider", "google")
	// Set a request ID to use as state
	ctx := types.WithRequestID(req.Context(), "test_state_12345")
	req = req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.HandleOAuthLogin(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307, got %d; body: %s", w.Code, w.Body.String())
	}

	location := w.Header().Get("Location")
	if !strings.Contains(location, "accounts.google.com") {
		t.Errorf("expected redirect to Google, got %q", location)
	}

	// Verify oauth_state cookie is set
	cookie := findCookie(w, "oauth_state")
	if cookie == nil {
		t.Fatal("expected oauth_state cookie")
	}
	if cookie.Value != "test_state_12345" {
		t.Errorf("expected state value 'test_state_12345', got %q", cookie.Value)
	}
}

func TestHandleOAuthLogin_UnknownProvider(t *testing.T) {
	oauthMgr := &mockOAuthManager{
		providers: map[string]OAuthProvider{},
	}
	handler := NewAuthHandler(
		&mockAuthService{},
		&mockSessionService{},
		oauthMgr,
		DefaultCookieConfig(),
		nil,
		core.NewValidator(nil),
	)

	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/facebook/login", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("provider", "facebook")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.HandleOAuthLogin(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown provider, got %d", w.Code)
	}
}

// =============================================================================
// Cookie Configuration Tests
// =============================================================================

func TestDefaultCookieConfig(t *testing.T) {
	cfg := DefaultCookieConfig()

	if cfg.Name != "session_id" {
		t.Errorf("expected Name 'session_id', got %q", cfg.Name)
	}
	if !cfg.Secure {
		t.Error("expected Secure=true")
	}
	if !cfg.HttpOnly {
		t.Error("expected HttpOnly=true")
	}
	if cfg.SameSite != http.SameSiteLaxMode {
		t.Errorf("expected SameSite=Lax, got %d", cfg.SameSite)
	}
	if cfg.MaxAge != 604800 {
		t.Errorf("expected MaxAge=604800, got %d", cfg.MaxAge)
	}
	if cfg.Path != "/" {
		t.Errorf("expected Path='/', got %q", cfg.Path)
	}
}

// =============================================================================
// extractClientIP Tests
// =============================================================================

func TestExtractClientIP_XForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.50, 10.0.0.1, 10.0.0.2")

	ip := extractClientIP(req)
	if ip != "203.0.113.50" {
		t.Errorf("expected '203.0.113.50', got %q", ip)
	}
}

func TestExtractClientIP_RemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.100:12345"

	ip := extractClientIP(req)
	if ip != "192.168.1.100" {
		t.Errorf("expected '192.168.1.100', got %q", ip)
	}
}

func TestExtractClientIP_RemoteAddrNoPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.100"

	ip := extractClientIP(req)
	if ip != "192.168.1.100" {
		t.Errorf("expected '192.168.1.100', got %q", ip)
	}
}

// =============================================================================
// Route Registration Tests
// =============================================================================

func TestRegisterRoutes(t *testing.T) {
	handler := newTestHandler(&mockAuthService{}, &mockSessionService{})
	r := chi.NewRouter()
	handler.RegisterRoutes(r)

	// Walk the routes and verify all expected routes are registered.
	var routes []string
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, method+" "+route)
		return nil
	})

	expectedRoutes := map[string]bool{
		"POST /login":                      false,
		"POST /forgot-password":            false,
		"POST /reset-password":             false,
		"POST /accept-invite":              false,
		"GET /oauth/{provider}/login":      false,
		"GET /oauth/{provider}/callback":   false,
		"POST /logout":                     false,
	}

	for _, route := range routes {
		if _, ok := expectedRoutes[route]; ok {
			expectedRoutes[route] = true
		}
	}

	for route, found := range expectedRoutes {
		if !found {
			t.Errorf("expected route %q to be registered, but it was not", route)
		}
	}
}
