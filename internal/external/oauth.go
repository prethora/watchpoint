package external

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"watchpoint/internal/types"
)

// ---------------------------------------------------------------------------
// OAuth API Base URLs (overridable for testing)
// ---------------------------------------------------------------------------

const (
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"

	githubTokenURL    = "https://github.com/login/oauth/access_token"
	githubUserURL     = "https://api.github.com/user"
	githubEmailsURL   = "https://api.github.com/user/emails"

	googleAuthBaseURL = "https://accounts.google.com/o/oauth2/v2/auth"
	githubAuthBaseURL = "https://github.com/login/oauth/authorize"
)

// ---------------------------------------------------------------------------
// Google Provider
// ---------------------------------------------------------------------------

// GoogleProviderConfig holds the configuration for the Google OAuth provider.
type GoogleProviderConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Logger       *slog.Logger

	// Override URLs for testing
	TokenURL    string
	UserInfoURL string
	AuthBaseURL string
}

// GoogleProvider implements OAuthProvider for Google OAuth 2.0.
// It performs two sequential HTTP calls during Exchange:
//  1. Token exchange (authorization code -> access token)
//  2. UserInfo retrieval (access token -> user profile)
//
// The profile is normalized into types.OAuthProfile with Google-specific
// field mapping (e.g., verified_email -> EmailVerified).
type GoogleProvider struct {
	base         *BaseClient
	clientID     string
	clientSecret string
	redirectURL  string
	tokenURL     string
	userInfoURL  string
	authBaseURL  string
	logger       *slog.Logger
}

// NewGoogleProvider creates a new GoogleProvider with the given HTTP client and config.
func NewGoogleProvider(httpClient *http.Client, cfg GoogleProviderConfig) *GoogleProvider {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = googleTokenURL
	}

	userInfoURL := cfg.UserInfoURL
	if userInfoURL == "" {
		userInfoURL = googleUserInfoURL
	}

	authBaseURL := cfg.AuthBaseURL
	if authBaseURL == "" {
		authBaseURL = googleAuthBaseURL
	}

	base := NewBaseClient(
		httpClient,
		"google-oauth",
		RetryPolicy{
			MaxRetries: 1,
			MinWait:    500 * time.Millisecond,
			MaxWait:    3 * time.Second,
		},
		"WatchPoint/1.0",
	)

	return &GoogleProvider{
		base:         base,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  cfg.RedirectURL,
		tokenURL:     tokenURL,
		userInfoURL:  userInfoURL,
		authBaseURL:  authBaseURL,
		logger:       logger,
	}
}

// NewGoogleProviderWithBase creates a GoogleProvider with a pre-configured BaseClient.
// This is useful for testing when you want to control the BaseClient configuration.
func NewGoogleProviderWithBase(base *BaseClient, cfg GoogleProviderConfig) *GoogleProvider {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = googleTokenURL
	}

	userInfoURL := cfg.UserInfoURL
	if userInfoURL == "" {
		userInfoURL = googleUserInfoURL
	}

	authBaseURL := cfg.AuthBaseURL
	if authBaseURL == "" {
		authBaseURL = googleAuthBaseURL
	}

	return &GoogleProvider{
		base:         base,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  cfg.RedirectURL,
		tokenURL:     tokenURL,
		userInfoURL:  userInfoURL,
		authBaseURL:  authBaseURL,
		logger:       logger,
	}
}

// Name returns "google".
func (p *GoogleProvider) Name() string {
	return "google"
}

// GetLoginURL generates the Google OAuth authorization URL with the given state parameter.
// Scopes: userinfo.email, userinfo.profile
func (p *GoogleProvider) GetLoginURL(state string) string {
	params := url.Values{}
	params.Set("client_id", p.clientID)
	params.Set("redirect_uri", p.redirectURL)
	params.Set("response_type", "code")
	params.Set("scope", "https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile")
	params.Set("state", state)
	params.Set("access_type", "online")

	return p.authBaseURL + "?" + params.Encode()
}

// Exchange trades an authorization code for a normalized OAuthProfile.
// Performs two sequential HTTP calls:
//  1. POST to token endpoint to exchange code for access token
//  2. GET to userinfo endpoint to retrieve user profile
//
// Does NOT return access/refresh tokens -- scope is authentication only.
func (p *GoogleProvider) Exchange(ctx context.Context, code string) (*types.OAuthProfile, error) {
	// Step 1: Exchange authorization code for access token.
	accessToken, err := p.exchangeCodeForToken(ctx, code)
	if err != nil {
		return nil, err
	}

	// Step 2: Fetch user info using the access token.
	profile, err := p.fetchUserInfo(ctx, accessToken)
	if err != nil {
		return nil, err
	}

	return profile, nil
}

// exchangeCodeForToken performs the OAuth token exchange.
func (p *GoogleProvider) exchangeCodeForToken(ctx context.Context, code string) (string, error) {
	params := url.Values{}
	params.Set("client_id", p.clientID)
	params.Set("client_secret", p.clientSecret)
	params.Set("code", code)
	params.Set("grant_type", "authorization_code")
	params.Set("redirect_uri", p.redirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(params.Encode()))
	if err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to create Google token exchange request",
			err,
		)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.base.Do(req)
	if err != nil {
		return "", wrapOAuthError("google", "token exchange", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", handleOAuthTokenError("google", resp)
	}

	var tokenResp googleTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode Google token response",
			err,
		)
	}

	if tokenResp.AccessToken == "" {
		return "", types.NewAppError(
			types.ErrCodeAuthTokenInvalid,
			"Google returned empty access token",
			nil,
		)
	}

	return tokenResp.AccessToken, nil
}

// fetchUserInfo retrieves the user profile from the Google UserInfo endpoint.
func (p *GoogleProvider) fetchUserInfo(ctx context.Context, accessToken string) (*types.OAuthProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userInfoURL, nil)
	if err != nil {
		return nil, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to create Google userinfo request",
			err,
		)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.base.Do(req)
	if err != nil {
		return nil, wrapOAuthError("google", "userinfo", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, handleOAuthAPIError("google", "userinfo", resp)
	}

	var userInfo googleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return nil, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode Google userinfo response",
			err,
		)
	}

	return &types.OAuthProfile{
		Provider:      "google",
		ProviderID:    userInfo.ID,
		Email:         userInfo.Email,
		Name:          userInfo.Name,
		AvatarURL:     userInfo.Picture,
		EmailVerified: userInfo.VerifiedEmail,
	}, nil
}

// Google-specific response types for JSON deserialization.

type googleTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

type googleUserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// ---------------------------------------------------------------------------
// GitHub Provider
// ---------------------------------------------------------------------------

// GithubProviderConfig holds the configuration for the GitHub OAuth provider.
type GithubProviderConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Logger       *slog.Logger

	// Override URLs for testing
	TokenURL    string
	UserURL     string
	EmailsURL   string
	AuthBaseURL string
}

// GithubProvider implements OAuthProvider for GitHub OAuth.
// It performs three sequential HTTP calls during Exchange:
//  1. Token exchange (authorization code -> access token)
//  2. User retrieval (access token -> user profile)
//  3. Emails retrieval (access token -> primary verified email)
//
// The primary verified email is selected by iterating the email list
// to find {primary: true, verified: true}.
type GithubProvider struct {
	base         *BaseClient
	clientID     string
	clientSecret string
	redirectURL  string
	tokenURL     string
	userURL      string
	emailsURL    string
	authBaseURL  string
	logger       *slog.Logger
}

// NewGithubProvider creates a new GithubProvider with the given HTTP client and config.
func NewGithubProvider(httpClient *http.Client, cfg GithubProviderConfig) *GithubProvider {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = githubTokenURL
	}

	userURL := cfg.UserURL
	if userURL == "" {
		userURL = githubUserURL
	}

	emailsURL := cfg.EmailsURL
	if emailsURL == "" {
		emailsURL = githubEmailsURL
	}

	authBaseURL := cfg.AuthBaseURL
	if authBaseURL == "" {
		authBaseURL = githubAuthBaseURL
	}

	base := NewBaseClient(
		httpClient,
		"github-oauth",
		RetryPolicy{
			MaxRetries: 1,
			MinWait:    500 * time.Millisecond,
			MaxWait:    3 * time.Second,
		},
		"WatchPoint/1.0",
	)

	return &GithubProvider{
		base:         base,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  cfg.RedirectURL,
		tokenURL:     tokenURL,
		userURL:      userURL,
		emailsURL:    emailsURL,
		authBaseURL:  authBaseURL,
		logger:       logger,
	}
}

// NewGithubProviderWithBase creates a GithubProvider with a pre-configured BaseClient.
// This is useful for testing when you want to control the BaseClient configuration.
func NewGithubProviderWithBase(base *BaseClient, cfg GithubProviderConfig) *GithubProvider {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = githubTokenURL
	}

	userURL := cfg.UserURL
	if userURL == "" {
		userURL = githubUserURL
	}

	emailsURL := cfg.EmailsURL
	if emailsURL == "" {
		emailsURL = githubEmailsURL
	}

	authBaseURL := cfg.AuthBaseURL
	if authBaseURL == "" {
		authBaseURL = githubAuthBaseURL
	}

	return &GithubProvider{
		base:         base,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  cfg.RedirectURL,
		tokenURL:     tokenURL,
		userURL:      userURL,
		emailsURL:    emailsURL,
		authBaseURL:  authBaseURL,
		logger:       logger,
	}
}

// Name returns "github".
func (p *GithubProvider) Name() string {
	return "github"
}

// GetLoginURL generates the GitHub OAuth authorization URL with the given state parameter.
// Scopes: read:user, user:email
func (p *GithubProvider) GetLoginURL(state string) string {
	params := url.Values{}
	params.Set("client_id", p.clientID)
	params.Set("redirect_uri", p.redirectURL)
	params.Set("scope", "read:user user:email")
	params.Set("state", state)

	return p.authBaseURL + "?" + params.Encode()
}

// Exchange trades an authorization code for a normalized OAuthProfile.
// Performs three sequential HTTP calls:
//  1. POST to token endpoint to exchange code for access token
//  2. GET /user to retrieve basic profile
//  3. GET /user/emails to find primary verified email
//
// Does NOT return access/refresh tokens -- scope is authentication only.
func (p *GithubProvider) Exchange(ctx context.Context, code string) (*types.OAuthProfile, error) {
	// Step 1: Exchange authorization code for access token.
	accessToken, err := p.exchangeCodeForToken(ctx, code)
	if err != nil {
		return nil, err
	}

	// Step 2: Fetch user profile.
	user, err := p.fetchUser(ctx, accessToken)
	if err != nil {
		return nil, err
	}

	// Step 3: Fetch user emails to find the primary, verified email.
	email, emailVerified, err := p.fetchPrimaryEmail(ctx, accessToken)
	if err != nil {
		return nil, err
	}

	return &types.OAuthProfile{
		Provider:      "github",
		ProviderID:    fmt.Sprintf("%d", user.ID),
		Email:         email,
		Name:          user.Name,
		AvatarURL:     user.AvatarURL,
		EmailVerified: emailVerified,
	}, nil
}

// exchangeCodeForToken performs the OAuth token exchange with GitHub.
func (p *GithubProvider) exchangeCodeForToken(ctx context.Context, code string) (string, error) {
	params := url.Values{}
	params.Set("client_id", p.clientID)
	params.Set("client_secret", p.clientSecret)
	params.Set("code", code)
	params.Set("redirect_uri", p.redirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(params.Encode()))
	if err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to create GitHub token exchange request",
			err,
		)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.base.Do(req)
	if err != nil {
		return "", wrapOAuthError("github", "token exchange", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", handleOAuthTokenError("github", resp)
	}

	var tokenResp githubTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode GitHub token response",
			err,
		)
	}

	// GitHub returns errors in the token response body even with 200 status.
	if tokenResp.Error != "" {
		return "", types.NewAppError(
			types.ErrCodeAuthTokenInvalid,
			fmt.Sprintf("GitHub token exchange failed: %s - %s", tokenResp.Error, tokenResp.ErrorDescription),
			nil,
		)
	}

	if tokenResp.AccessToken == "" {
		return "", types.NewAppError(
			types.ErrCodeAuthTokenInvalid,
			"GitHub returned empty access token",
			nil,
		)
	}

	return tokenResp.AccessToken, nil
}

// fetchUser retrieves the user profile from the GitHub /user endpoint.
func (p *GithubProvider) fetchUser(ctx context.Context, accessToken string) (*githubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userURL, nil)
	if err != nil {
		return nil, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to create GitHub user request",
			err,
		)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := p.base.Do(req)
	if err != nil {
		return nil, wrapOAuthError("github", "user", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, handleOAuthAPIError("github", "user", resp)
	}

	var user githubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode GitHub user response",
			err,
		)
	}

	return &user, nil
}

// fetchPrimaryEmail retrieves the user's emails from GitHub and selects the
// primary, verified email. Returns (email, emailVerified, error).
// If no primary+verified email is found, falls back to the first verified email,
// then the first primary email.
func (p *GithubProvider) fetchPrimaryEmail(ctx context.Context, accessToken string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.emailsURL, nil)
	if err != nil {
		return "", false, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to create GitHub emails request",
			err,
		)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := p.base.Do(req)
	if err != nil {
		return "", false, wrapOAuthError("github", "emails", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", false, handleOAuthAPIError("github", "emails", resp)
	}

	var emails []githubEmail
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", false, types.NewAppError(
			types.ErrCodeInternalUnexpected,
			"failed to decode GitHub emails response",
			err,
		)
	}

	if len(emails) == 0 {
		return "", false, types.NewAppError(
			types.ErrCodeAuthTokenInvalid,
			"GitHub account has no email addresses",
			nil,
		)
	}

	// Per architecture spec: iterate to find {primary: true, verified: true}.
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email, true, nil
		}
	}

	// Fallback: first verified email
	for _, e := range emails {
		if e.Verified {
			return e.Email, true, nil
		}
	}

	// Last resort: first primary email (unverified)
	for _, e := range emails {
		if e.Primary {
			return e.Email, false, nil
		}
	}

	// Absolute fallback: first email in list
	return emails[0].Email, emails[0].Verified, nil
}

// GitHub-specific response types for JSON deserialization.

type githubTokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type githubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	Email     string `json:"email"` // May be empty if email is private
}

type githubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// ---------------------------------------------------------------------------
// OAuthManager Implementation
// ---------------------------------------------------------------------------

// OAuthManagerImpl implements OAuthManager by maintaining a map of registered providers.
type OAuthManagerImpl struct {
	providers map[string]OAuthProvider
}

// NewOAuthManager creates an OAuthManager with the given providers.
// Providers are registered by their Name() return value.
func NewOAuthManager(providers ...OAuthProvider) *OAuthManagerImpl {
	m := &OAuthManagerImpl{
		providers: make(map[string]OAuthProvider, len(providers)),
	}
	for _, p := range providers {
		m.providers[p.Name()] = p
	}
	return m
}

// GetProvider returns the OAuthProvider registered under the given name.
// Returns an error if no provider is registered with that name.
func (m *OAuthManagerImpl) GetProvider(name string) (OAuthProvider, error) {
	p, ok := m.providers[name]
	if !ok {
		return nil, types.NewAppError(
			types.ErrCodeValidationMissingField,
			fmt.Sprintf("unknown OAuth provider: %s", name),
			nil,
		)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Shared Error Helpers
// ---------------------------------------------------------------------------

// wrapOAuthError wraps a BaseClient transport error with OAuth context.
func wrapOAuthError(provider, operation string, err error) error {
	if _, ok := err.(*types.AppError); ok {
		return err
	}
	return types.NewAppError(
		types.ErrCodeUpstreamUnavailable,
		fmt.Sprintf("OAuth %s %s request failed: %v", provider, operation, err),
		err,
	)
}

// handleOAuthTokenError handles non-200 responses from the token endpoint.
func handleOAuthTokenError(provider string, resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return types.NewAppError(
		types.ErrCodeAuthTokenInvalid,
		fmt.Sprintf("OAuth %s token exchange failed (%d): %s", provider, resp.StatusCode, truncateBody(body)),
		nil,
	)
}

// handleOAuthAPIError handles non-200 responses from API endpoints (userinfo, user, emails).
func handleOAuthAPIError(provider, endpoint string, resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return types.NewAppError(
			types.ErrCodeAuthTokenInvalid,
			fmt.Sprintf("OAuth %s %s: access token rejected (%d): %s", provider, endpoint, resp.StatusCode, truncateBody(body)),
			nil,
		)
	case resp.StatusCode == http.StatusForbidden:
		return types.NewAppError(
			types.ErrCodeAuthTokenInvalid,
			fmt.Sprintf("OAuth %s %s: insufficient permissions (%d): %s", provider, endpoint, resp.StatusCode, truncateBody(body)),
			nil,
		)
	case resp.StatusCode >= 500:
		return types.NewAppError(
			types.ErrCodeUpstreamUnavailable,
			fmt.Sprintf("OAuth %s %s: server error (%d): %s", provider, endpoint, resp.StatusCode, truncateBody(body)),
			nil,
		)
	default:
		return types.NewAppError(
			types.ErrCodeUpstreamUnavailable,
			fmt.Sprintf("OAuth %s %s: unexpected response (%d): %s", provider, endpoint, resp.StatusCode, truncateBody(body)),
			nil,
		)
	}
}

// truncateBody returns a string representation of the body, truncated to a reasonable length.
func truncateBody(body []byte) string {
	const maxLen = 200
	s := string(body)
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// Interface Compliance
// ---------------------------------------------------------------------------

// Compile-time assertions that concrete types satisfy their interfaces.
var _ OAuthProvider = (*GoogleProvider)(nil)
var _ OAuthProvider = (*GithubProvider)(nil)
var _ OAuthManager = (*OAuthManagerImpl)(nil)
