package external

import (
	"log/slog"
	"net/http"
	"time"

	"watchpoint/internal/config"
)

// ---------------------------------------------------------------------------
// Client Registry — Section 7
//
// Central factory that instantiates all external service clients based on
// configuration. In test/local mode, returns stub implementations that log
// actions without requiring real credentials. In production mode, returns
// real client implementations with strict timeouts.
// ---------------------------------------------------------------------------

// ClientRegistry holds all external service client interfaces. It is the single
// point of access for the rest of the application to interact with third-party
// services (Stripe, SendGrid, OAuth providers).
type ClientRegistry struct {
	Billing BillingService
	Email   EmailProvider
	OAuth   OAuthManager

	// Verifiers
	StripeVerifier WebhookVerifier
	EmailVerifier  EmailVerifier
}

// RegistryOption is a functional option for configuring a ClientRegistry.
// Options allow callers to inject dependencies that are not available from
// config alone (e.g., database-backed lookup services needed by real clients).
type RegistryOption func(*registryConfig)

// registryConfig holds optional dependencies used when building real clients.
type registryConfig struct {
	orgBillingLookup OrgBillingLookup
}

// WithOrgBillingLookup provides the OrgBillingLookup implementation required
// by the real StripeClient. This is a no-op in test/local mode where stubs
// are used instead.
func WithOrgBillingLookup(lookup OrgBillingLookup) RegistryOption {
	return func(rc *registryConfig) {
		rc.orgBillingLookup = lookup
	}
}

// NewClientRegistry initializes all external service clients.
// If cfg.IsTestMode is true or cfg.Environment is "local", the registry is
// populated with Stub implementations that log actions without requiring real
// credentials. Otherwise, real client implementations are initialized with
// strict timeouts per provider.
//
// This function matches the architecture specification in Section 7 of
// 10-external-integrations.md.
func NewClientRegistry(cfg *config.Config, logger *slog.Logger, opts ...RegistryOption) (*ClientRegistry, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// Apply functional options.
	rc := &registryConfig{}
	for _, opt := range opts {
		opt(rc)
	}

	// Determine whether to use stub implementations.
	useStubs := cfg.IsTestMode || cfg.Environment == "local"

	if useStubs {
		logger.Info("initializing external clients in STUB mode",
			"is_test_mode", cfg.IsTestMode,
			"environment", cfg.Environment,
		)
		return newStubRegistry(logger), nil
	}

	logger.Info("initializing external clients in PRODUCTION mode",
		"environment", cfg.Environment,
	)
	return newProductionRegistry(cfg, logger, rc)
}

// newStubRegistry creates a ClientRegistry populated entirely with stub
// implementations. This allows the application to boot locally without
// any external service credentials.
func newStubRegistry(logger *slog.Logger) *ClientRegistry {
	stubLogger := logger.With("mode", "stub")

	return &ClientRegistry{
		Billing:        NewStubBillingService(stubLogger),
		Email:          NewStubEmailProvider(stubLogger),
		OAuth:          NewStubOAuthManager(stubLogger, "google", "github"),
		StripeVerifier: NewStubWebhookVerifier(stubLogger),
		EmailVerifier:  NewStubEmailVerifier(stubLogger),
	}
}

// newProductionRegistry creates a ClientRegistry with real client implementations
// configured with strict timeouts and resilience patterns.
func newProductionRegistry(cfg *config.Config, logger *slog.Logger, rc *registryConfig) (*ClientRegistry, error) {
	reg := &ClientRegistry{}

	// --- Billing (Stripe) ---
	// Timeout: 20 seconds per architecture spec (Section 4.4).
	stripeHTTPClient := &http.Client{Timeout: 20 * time.Second}
	reg.Billing = NewStripeClient(stripeHTTPClient, rc.orgBillingLookup, StripeClientConfig{
		SecretKey: cfg.Billing.StripeSecretKey.Unmask(),
		Logger:    logger.With("client", "stripe"),
	})

	// Stripe webhook verifier (real implementation).
	reg.StripeVerifier = &StripeVerifier{}

	// --- Email (SendGrid) ---
	// Timeout: 10 seconds per architecture spec (Section 5.4).
	sendgridHTTPClient := &http.Client{Timeout: 10 * time.Second}
	reg.Email = NewSendGridClient(sendgridHTTPClient, SendGridClientConfig{
		APIKey: cfg.Email.SendGridAPIKey.Unmask(),
		Logger: logger.With("client", "sendgrid"),
	})

	// SendGrid email verifier (real implementation).
	reg.EmailVerifier = &SendGridVerifier{}

	// --- OAuth ---
	oauthHTTPClient := &http.Client{Timeout: 10 * time.Second}
	var providers []OAuthProvider

	// Initialize Google provider if configured.
	if cfg.Auth.GoogleClientID != "" {
		providers = append(providers, NewGoogleProvider(oauthHTTPClient, GoogleProviderConfig{
			ClientID:     cfg.Auth.GoogleClientID,
			ClientSecret: cfg.Auth.GoogleClientSecret.Unmask(),
			RedirectURL:  cfg.Server.APIExternalURL + "/v1/auth/google/callback",
			Logger:       logger.With("client", "google-oauth"),
		}))
	}

	// Initialize GitHub provider if configured.
	if cfg.Auth.GithubClientID != "" {
		providers = append(providers, NewGithubProvider(oauthHTTPClient, GithubProviderConfig{
			ClientID:     cfg.Auth.GithubClientID,
			ClientSecret: cfg.Auth.GithubClientSecret.Unmask(),
			RedirectURL:  cfg.Server.APIExternalURL + "/v1/auth/github/callback",
			Logger:       logger.With("client", "github-oauth"),
		}))
	}

	reg.OAuth = NewOAuthManager(providers...)

	return reg, nil
}
