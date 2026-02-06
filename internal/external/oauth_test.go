package external

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// ---------------------------------------------------------------------------
// Helper: Create test OAuth providers pointed at httptest servers
// ---------------------------------------------------------------------------

func newTestGoogleProvider(t *testing.T, tokenURL, userInfoURL string) *GoogleProvider {
	t.Helper()
	base := NewBaseClient(
		&http.Client{Timeout: 5 * time.Second},
		"test-google-oauth",
		RetryPolicy{
			MaxRetries: 0,
			MinWait:    1 * time.Millisecond,
			MaxWait:    10 * time.Millisecond,
		},
		"WatchPoint-Test/1.0",
		WithSleepFunc(noopSleep),
	)

	return NewGoogleProviderWithBase(base, GoogleProviderConfig{
		ClientID:     "google-client-id",
		ClientSecret: "google-client-secret",
		RedirectURL:  "https://app.watchpoint.io/auth/oauth/google/callback",
		TokenURL:     tokenURL,
		UserInfoURL:  userInfoURL,
	})
}

func newTestGithubProvider(t *testing.T, tokenURL, userURL, emailsURL string) *GithubProvider {
	t.Helper()
	base := NewBaseClient(
		&http.Client{Timeout: 5 * time.Second},
		"test-github-oauth",
		RetryPolicy{
			MaxRetries: 0,
			MinWait:    1 * time.Millisecond,
			MaxWait:    10 * time.Millisecond,
		},
		"WatchPoint-Test/1.0",
		WithSleepFunc(noopSleep),
	)

	return NewGithubProviderWithBase(base, GithubProviderConfig{
		ClientID:     "github-client-id",
		ClientSecret: "github-client-secret",
		RedirectURL:  "https://app.watchpoint.io/auth/oauth/github/callback",
		TokenURL:     tokenURL,
		UserURL:      userURL,
		EmailsURL:    emailsURL,
	})
}

// ---------------------------------------------------------------------------
// Google Provider Tests
// ---------------------------------------------------------------------------

func TestGoogleProvider_Name(t *testing.T) {
	p := &GoogleProvider{}
	if p.Name() != "google" {
		t.Errorf("expected name 'google', got %q", p.Name())
	}
}

func TestGoogleProvider_GetLoginURL(t *testing.T) {
	p := &GoogleProvider{
		clientID:    "test-client-id",
		redirectURL: "https://app.example.com/callback",
		authBaseURL: "https://accounts.google.com/o/oauth2/v2/auth",
	}

	loginURL := p.GetLoginURL("random-state-token")

	parsed, err := url.Parse(loginURL)
	if err != nil {
		t.Fatalf("failed to parse login URL: %v", err)
	}

	if parsed.Scheme != "https" {
		t.Errorf("expected https scheme, got %s", parsed.Scheme)
	}
	if parsed.Host != "accounts.google.com" {
		t.Errorf("expected host accounts.google.com, got %s", parsed.Host)
	}

	query := parsed.Query()
	if query.Get("client_id") != "test-client-id" {
		t.Errorf("expected client_id 'test-client-id', got %q", query.Get("client_id"))
	}
	if query.Get("redirect_uri") != "https://app.example.com/callback" {
		t.Errorf("expected redirect_uri, got %q", query.Get("redirect_uri"))
	}
	if query.Get("response_type") != "code" {
		t.Errorf("expected response_type 'code', got %q", query.Get("response_type"))
	}
	if query.Get("state") != "random-state-token" {
		t.Errorf("expected state 'random-state-token', got %q", query.Get("state"))
	}

	scope := query.Get("scope")
	if !strings.Contains(scope, "userinfo.email") || !strings.Contains(scope, "userinfo.profile") {
		t.Errorf("expected scope to contain userinfo.email and userinfo.profile, got %q", scope)
	}
}

func TestGoogleProvider_Exchange_Success(t *testing.T) {
	// Set up a server that handles both token exchange and userinfo.
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST for token, got %s", r.Method)
		}

		// Verify Content-Type
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("expected Content-Type application/x-www-form-urlencoded, got %s", ct)
		}

		r.ParseForm()
		if code := r.FormValue("code"); code != "test-auth-code" {
			t.Errorf("expected code 'test-auth-code', got %q", code)
		}
		if grantType := r.FormValue("grant_type"); grantType != "authorization_code" {
			t.Errorf("expected grant_type 'authorization_code', got %q", grantType)
		}
		if clientID := r.FormValue("client_id"); clientID != "google-client-id" {
			t.Errorf("expected client_id 'google-client-id', got %q", clientID)
		}
		if clientSecret := r.FormValue("client_secret"); clientSecret != "google-client-secret" {
			t.Errorf("expected client_secret 'google-client-secret', got %q", clientSecret)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "google-access-token-xyz",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})

	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET for userinfo, got %s", r.Method)
		}

		// Verify Bearer token
		auth := r.Header.Get("Authorization")
		if auth != "Bearer google-access-token-xyz" {
			t.Errorf("expected Bearer google-access-token-xyz, got %q", auth)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":             "google-uid-12345",
			"email":          "user@gmail.com",
			"verified_email": true,
			"name":           "Test User",
			"picture":        "https://lh3.googleusercontent.com/photo.jpg",
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGoogleProvider(t, server.URL+"/token", server.URL+"/userinfo")

	profile, err := provider.Exchange(context.Background(), "test-auth-code")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if profile.Provider != "google" {
		t.Errorf("expected provider 'google', got %q", profile.Provider)
	}
	if profile.ProviderID != "google-uid-12345" {
		t.Errorf("expected provider ID 'google-uid-12345', got %q", profile.ProviderID)
	}
	if profile.Email != "user@gmail.com" {
		t.Errorf("expected email 'user@gmail.com', got %q", profile.Email)
	}
	if profile.Name != "Test User" {
		t.Errorf("expected name 'Test User', got %q", profile.Name)
	}
	if profile.AvatarURL != "https://lh3.googleusercontent.com/photo.jpg" {
		t.Errorf("expected avatar URL, got %q", profile.AvatarURL)
	}
	if !profile.EmailVerified {
		t.Error("expected EmailVerified to be true")
	}
}

func TestGoogleProvider_Exchange_UnverifiedEmail(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "token-123",
			"token_type":   "Bearer",
		})
	})

	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":             "uid-unverified",
			"email":          "unverified@gmail.com",
			"verified_email": false,
			"name":           "Unverified User",
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGoogleProvider(t, server.URL+"/token", server.URL+"/userinfo")

	profile, err := provider.Exchange(context.Background(), "code-123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// The provider returns the profile with EmailVerified=false.
	// It's the AuthService's responsibility to reject unverified emails.
	if profile.EmailVerified {
		t.Error("expected EmailVerified to be false")
	}
	if profile.Email != "unverified@gmail.com" {
		t.Errorf("expected email 'unverified@gmail.com', got %q", profile.Email)
	}
}

func TestGoogleProvider_Exchange_TokenExchangeFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":             "invalid_grant",
			"error_description": "Code has expired or been used",
		})
	}))
	defer server.Close()

	provider := newTestGoogleProvider(t, server.URL+"/token", server.URL+"/userinfo")

	_, err := provider.Exchange(context.Background(), "expired-code")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeAuthTokenInvalid {
		t.Errorf("expected error code %s, got %s", types.ErrCodeAuthTokenInvalid, appErr.Code)
	}
}

func TestGoogleProvider_Exchange_EmptyAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "",
			"token_type":   "Bearer",
		})
	}))
	defer server.Close()

	provider := newTestGoogleProvider(t, server.URL+"/token", server.URL+"/userinfo")

	_, err := provider.Exchange(context.Background(), "code-123")
	if err == nil {
		t.Fatal("expected error for empty access token, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeAuthTokenInvalid {
		t.Errorf("expected error code %s, got %s", types.ErrCodeAuthTokenInvalid, appErr.Code)
	}
}

func TestGoogleProvider_Exchange_UserInfoFails(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "valid-token",
			"token_type":   "Bearer",
		})
	})

	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_token"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGoogleProvider(t, server.URL+"/token", server.URL+"/userinfo")

	_, err := provider.Exchange(context.Background(), "code-123")
	if err == nil {
		t.Fatal("expected error when userinfo fails, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeAuthTokenInvalid {
		t.Errorf("expected error code %s, got %s", types.ErrCodeAuthTokenInvalid, appErr.Code)
	}
}

func TestGoogleProvider_Exchange_UserInfoServerError(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "valid-token",
			"token_type":   "Bearer",
		})
	})

	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"server error"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGoogleProvider(t, server.URL+"/token", server.URL+"/userinfo")

	_, err := provider.Exchange(context.Background(), "code-123")
	if err == nil {
		t.Fatal("expected error when userinfo returns 500, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}

	// 500 from the userinfo endpoint gets caught by BaseClient retry logic.
	// With 0 retries, it maps to ErrCodeUpstreamUnavailable.
	if appErr.Code != types.ErrCodeUpstreamUnavailable {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamUnavailable, appErr.Code)
	}
}

// ---------------------------------------------------------------------------
// GitHub Provider Tests
// ---------------------------------------------------------------------------

func TestGithubProvider_Name(t *testing.T) {
	p := &GithubProvider{}
	if p.Name() != "github" {
		t.Errorf("expected name 'github', got %q", p.Name())
	}
}

func TestGithubProvider_GetLoginURL(t *testing.T) {
	p := &GithubProvider{
		clientID:    "gh-client-id",
		redirectURL: "https://app.example.com/callback",
		authBaseURL: "https://github.com/login/oauth/authorize",
	}

	loginURL := p.GetLoginURL("state-xyz")

	parsed, err := url.Parse(loginURL)
	if err != nil {
		t.Fatalf("failed to parse login URL: %v", err)
	}

	if parsed.Host != "github.com" {
		t.Errorf("expected host github.com, got %s", parsed.Host)
	}

	query := parsed.Query()
	if query.Get("client_id") != "gh-client-id" {
		t.Errorf("expected client_id 'gh-client-id', got %q", query.Get("client_id"))
	}
	if query.Get("state") != "state-xyz" {
		t.Errorf("expected state 'state-xyz', got %q", query.Get("state"))
	}

	scope := query.Get("scope")
	if !strings.Contains(scope, "read:user") || !strings.Contains(scope, "user:email") {
		t.Errorf("expected scope to contain 'read:user' and 'user:email', got %q", scope)
	}
}

func TestGithubProvider_Exchange_Success(t *testing.T) {
	mux := http.NewServeMux()
	callSequence := make([]string, 0, 3)

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		callSequence = append(callSequence, "token")

		if r.Method != http.MethodPost {
			t.Errorf("expected POST for token, got %s", r.Method)
		}

		// Verify Accept header (GitHub requires this for JSON response)
		if accept := r.Header.Get("Accept"); accept != "application/json" {
			t.Errorf("expected Accept application/json, got %s", accept)
		}

		r.ParseForm()
		if code := r.FormValue("code"); code != "gh-auth-code" {
			t.Errorf("expected code 'gh-auth-code', got %q", code)
		}
		if clientID := r.FormValue("client_id"); clientID != "github-client-id" {
			t.Errorf("expected client_id 'github-client-id', got %q", clientID)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "gho_github_token_abc",
			"token_type":   "bearer",
			"scope":        "read:user,user:email",
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		callSequence = append(callSequence, "user")

		if r.Method != http.MethodGet {
			t.Errorf("expected GET for user, got %s", r.Method)
		}

		auth := r.Header.Get("Authorization")
		if auth != "Bearer gho_github_token_abc" {
			t.Errorf("expected Bearer gho_github_token_abc, got %q", auth)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":         42,
			"login":      "testuser",
			"name":       "Test GitHub User",
			"avatar_url": "https://avatars.githubusercontent.com/u/42",
			"email":      "", // Private email
		})
	})

	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		callSequence = append(callSequence, "emails")

		if r.Method != http.MethodGet {
			t.Errorf("expected GET for emails, got %s", r.Method)
		}

		auth := r.Header.Get("Authorization")
		if auth != "Bearer gho_github_token_abc" {
			t.Errorf("expected Bearer gho_github_token_abc, got %q", auth)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"email":    "secondary@example.com",
				"primary":  false,
				"verified": true,
			},
			{
				"email":    "primary@example.com",
				"primary":  true,
				"verified": true,
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGithubProvider(t, server.URL+"/token", server.URL+"/user", server.URL+"/user/emails")

	profile, err := provider.Exchange(context.Background(), "gh-auth-code")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify call sequence
	if len(callSequence) != 3 {
		t.Fatalf("expected 3 API calls, got %d: %v", len(callSequence), callSequence)
	}
	if callSequence[0] != "token" || callSequence[1] != "user" || callSequence[2] != "emails" {
		t.Errorf("expected call sequence [token, user, emails], got %v", callSequence)
	}

	// Verify profile
	if profile.Provider != "github" {
		t.Errorf("expected provider 'github', got %q", profile.Provider)
	}
	if profile.ProviderID != "42" {
		t.Errorf("expected provider ID '42', got %q", profile.ProviderID)
	}
	if profile.Email != "primary@example.com" {
		t.Errorf("expected email 'primary@example.com', got %q", profile.Email)
	}
	if profile.Name != "Test GitHub User" {
		t.Errorf("expected name 'Test GitHub User', got %q", profile.Name)
	}
	if profile.AvatarURL != "https://avatars.githubusercontent.com/u/42" {
		t.Errorf("expected avatar URL, got %q", profile.AvatarURL)
	}
	if !profile.EmailVerified {
		t.Error("expected EmailVerified to be true for primary+verified email")
	}
}

func TestGithubProvider_Exchange_FallbackToVerifiedEmail(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "token-123",
			"token_type":   "bearer",
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   99,
			"login": "fallbackuser",
			"name": "Fallback User",
		})
	})

	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No email is both primary AND verified
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"email":    "primary-unverified@example.com",
				"primary":  true,
				"verified": false,
			},
			{
				"email":    "verified-nonprimary@example.com",
				"primary":  false,
				"verified": true,
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGithubProvider(t, server.URL+"/token", server.URL+"/user", server.URL+"/user/emails")

	profile, err := provider.Exchange(context.Background(), "code-123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Should fall back to the first verified email
	if profile.Email != "verified-nonprimary@example.com" {
		t.Errorf("expected email 'verified-nonprimary@example.com', got %q", profile.Email)
	}
	if !profile.EmailVerified {
		t.Error("expected EmailVerified to be true")
	}
}

func TestGithubProvider_Exchange_FallbackToPrimaryEmail(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "token-123",
			"token_type":   "bearer",
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   101,
			"login": "noverified",
			"name": "No Verified",
		})
	})

	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No verified emails at all
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"email":    "primary-unverified@example.com",
				"primary":  true,
				"verified": false,
			},
			{
				"email":    "other-unverified@example.com",
				"primary":  false,
				"verified": false,
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGithubProvider(t, server.URL+"/token", server.URL+"/user", server.URL+"/user/emails")

	profile, err := provider.Exchange(context.Background(), "code-123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Should fall back to the primary email even though it's unverified
	if profile.Email != "primary-unverified@example.com" {
		t.Errorf("expected email 'primary-unverified@example.com', got %q", profile.Email)
	}
	if profile.EmailVerified {
		t.Error("expected EmailVerified to be false")
	}
}

func TestGithubProvider_Exchange_NoEmails(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "token-123",
			"token_type":   "bearer",
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   102,
			"login": "noemails",
			"name": "No Emails",
		})
	})

	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGithubProvider(t, server.URL+"/token", server.URL+"/user", server.URL+"/user/emails")

	_, err := provider.Exchange(context.Background(), "code-123")
	if err == nil {
		t.Fatal("expected error for no emails, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeAuthTokenInvalid {
		t.Errorf("expected error code %s, got %s", types.ErrCodeAuthTokenInvalid, appErr.Code)
	}
}

func TestGithubProvider_Exchange_TokenExchangeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK) // GitHub returns 200 even on error
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":             "bad_verification_code",
			"error_description": "The code passed is incorrect or expired.",
		})
	}))
	defer server.Close()

	provider := newTestGithubProvider(t, server.URL+"/token", server.URL+"/user", server.URL+"/user/emails")

	_, err := provider.Exchange(context.Background(), "bad-code")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeAuthTokenInvalid {
		t.Errorf("expected error code %s, got %s", types.ErrCodeAuthTokenInvalid, appErr.Code)
	}
	if !strings.Contains(appErr.Message, "bad_verification_code") {
		t.Errorf("expected error message to contain 'bad_verification_code', got %q", appErr.Message)
	}
}

func TestGithubProvider_Exchange_TokenExchangeHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_request"}`))
	}))
	defer server.Close()

	provider := newTestGithubProvider(t, server.URL+"/token", server.URL+"/user", server.URL+"/user/emails")

	_, err := provider.Exchange(context.Background(), "bad-code")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
}

func TestGithubProvider_Exchange_UserEndpointFails(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "valid-token",
			"token_type":   "bearer",
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGithubProvider(t, server.URL+"/token", server.URL+"/user", server.URL+"/user/emails")

	_, err := provider.Exchange(context.Background(), "code-123")
	if err == nil {
		t.Fatal("expected error when user endpoint fails, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeAuthTokenInvalid {
		t.Errorf("expected error code %s, got %s", types.ErrCodeAuthTokenInvalid, appErr.Code)
	}
}

func TestGithubProvider_Exchange_EmailsEndpointFails(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "valid-token",
			"token_type":   "bearer",
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   42,
			"login": "testuser",
			"name": "Test User",
		})
	})

	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Resource not accessible"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGithubProvider(t, server.URL+"/token", server.URL+"/user", server.URL+"/user/emails")

	_, err := provider.Exchange(context.Background(), "code-123")
	if err == nil {
		t.Fatal("expected error when emails endpoint fails, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
}

func TestGithubProvider_Exchange_EmptyAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "",
			"token_type":   "bearer",
		})
	}))
	defer server.Close()

	provider := newTestGithubProvider(t, server.URL+"/token", server.URL+"/user", server.URL+"/user/emails")

	_, err := provider.Exchange(context.Background(), "code-123")
	if err == nil {
		t.Fatal("expected error for empty access token, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeAuthTokenInvalid {
		t.Errorf("expected error code %s, got %s", types.ErrCodeAuthTokenInvalid, appErr.Code)
	}
}

func TestGithubProvider_Exchange_ProviderIDIsStringified(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "token-123",
			"token_type":   "bearer",
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":         987654321,
			"login":      "bigid",
			"name":       "Big ID User",
			"avatar_url": "https://avatars.githubusercontent.com/u/987654321",
		})
	})

	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"email":    "bigid@example.com",
				"primary":  true,
				"verified": true,
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	provider := newTestGithubProvider(t, server.URL+"/token", server.URL+"/user", server.URL+"/user/emails")

	profile, err := provider.Exchange(context.Background(), "code-123")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// GitHub user ID is an integer but ProviderID is a string
	if profile.ProviderID != "987654321" {
		t.Errorf("expected provider ID '987654321', got %q", profile.ProviderID)
	}
}

// ---------------------------------------------------------------------------
// OAuthManager Tests
// ---------------------------------------------------------------------------

func TestOAuthManager_GetProvider_Found(t *testing.T) {
	google := &GoogleProvider{}
	github := &GithubProvider{}

	manager := NewOAuthManager(google, github)

	p, err := manager.GetProvider("google")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if p.Name() != "google" {
		t.Errorf("expected google provider, got %q", p.Name())
	}

	p, err = manager.GetProvider("github")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if p.Name() != "github" {
		t.Errorf("expected github provider, got %q", p.Name())
	}
}

func TestOAuthManager_GetProvider_NotFound(t *testing.T) {
	manager := NewOAuthManager(&GoogleProvider{}, &GithubProvider{})

	_, err := manager.GetProvider("facebook")
	if err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeValidationMissingField {
		t.Errorf("expected error code %s, got %s", types.ErrCodeValidationMissingField, appErr.Code)
	}
	if !strings.Contains(appErr.Message, "facebook") {
		t.Errorf("expected error message to contain 'facebook', got %q", appErr.Message)
	}
}

func TestOAuthManager_GetProvider_EmptyName(t *testing.T) {
	manager := NewOAuthManager(&GoogleProvider{}, &GithubProvider{})

	_, err := manager.GetProvider("")
	if err == nil {
		t.Fatal("expected error for empty provider name, got nil")
	}
}

func TestOAuthManager_NoProviders(t *testing.T) {
	manager := NewOAuthManager()

	_, err := manager.GetProvider("google")
	if err == nil {
		t.Fatal("expected error when no providers registered, got nil")
	}
}

// ---------------------------------------------------------------------------
// Error Helper Tests
// ---------------------------------------------------------------------------

func TestTruncateBody(t *testing.T) {
	short := truncateBody([]byte("short body"))
	if short != "short body" {
		t.Errorf("expected 'short body', got %q", short)
	}

	long := truncateBody([]byte(strings.Repeat("x", 300)))
	if len(long) > 210 { // 200 + "..."
		t.Errorf("expected truncated body, got length %d", len(long))
	}
	if !strings.HasSuffix(long, "...") {
		t.Errorf("expected truncated body to end with '...', got %q", long[len(long)-10:])
	}
}

func TestWrapOAuthError_AlreadyAppError(t *testing.T) {
	original := types.NewAppError(types.ErrCodeUpstreamRateLimited, "rate limited", nil)
	wrapped := wrapOAuthError("google", "token", original)

	// Should return the original error unchanged
	var appErr *types.AppError
	if !errors.As(wrapped, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", wrapped)
	}
	if appErr.Code != types.ErrCodeUpstreamRateLimited {
		t.Errorf("expected original error code to be preserved, got %s", appErr.Code)
	}
}

func TestWrapOAuthError_GenericError(t *testing.T) {
	original := fmt.Errorf("connection refused")
	wrapped := wrapOAuthError("github", "user", original)

	var appErr *types.AppError
	if !errors.As(wrapped, &appErr) {
		t.Fatalf("expected *types.AppError, got %T", wrapped)
	}
	if appErr.Code != types.ErrCodeUpstreamUnavailable {
		t.Errorf("expected error code %s, got %s", types.ErrCodeUpstreamUnavailable, appErr.Code)
	}
	if !strings.Contains(appErr.Message, "github") {
		t.Errorf("expected message to contain 'github', got %q", appErr.Message)
	}
	if !strings.Contains(appErr.Message, "user") {
		t.Errorf("expected message to contain 'user', got %q", appErr.Message)
	}
}

// ---------------------------------------------------------------------------
// Interface Compliance
// ---------------------------------------------------------------------------

// Compile-time assertions.
var _ OAuthProvider = (*GoogleProvider)(nil)
var _ OAuthProvider = (*GithubProvider)(nil)
var _ OAuthManager = (*OAuthManagerImpl)(nil)
