package external

import (
	"context"
	"fmt"
	"log/slog"

	"watchpoint/internal/types"
)

// ---------------------------------------------------------------------------
// Stub Implementations — Section 7 / TEST-004
//
// Stub implementations allow the application to boot in local/test mode
// without requiring real external service credentials. They log all
// actions and return predictable, safe default values.
// ---------------------------------------------------------------------------

// StubBillingService implements BillingService by logging calls and returning
// test-safe defaults. Used when config.IsTestMode is true or APP_ENV=local.
type StubBillingService struct {
	logger *slog.Logger
}

// NewStubBillingService creates a new StubBillingService.
func NewStubBillingService(logger *slog.Logger) *StubBillingService {
	return &StubBillingService{logger: logger}
}

func (s *StubBillingService) EnsureCustomer(ctx context.Context, orgID string, email string) (string, error) {
	s.logger.InfoContext(ctx, "stub: EnsureCustomer called",
		"org_id", orgID,
		"email", email,
	)
	return fmt.Sprintf("cus_stub_%s", orgID), nil
}

func (s *StubBillingService) CreateCheckoutSession(ctx context.Context, orgID string, plan types.PlanTier, urls types.RedirectURLs) (string, string, error) {
	s.logger.InfoContext(ctx, "stub: CreateCheckoutSession called",
		"org_id", orgID,
		"plan", plan,
	)
	return "https://checkout.stub.local/session", fmt.Sprintf("cs_stub_%s", orgID), nil
}

func (s *StubBillingService) CreatePortalSession(ctx context.Context, orgID string, returnURL string) (string, error) {
	s.logger.InfoContext(ctx, "stub: CreatePortalSession called",
		"org_id", orgID,
		"return_url", returnURL,
	)
	return "https://portal.stub.local/session", nil
}

func (s *StubBillingService) GetInvoices(ctx context.Context, orgID string, params types.ListInvoicesParams) ([]*types.Invoice, types.PageInfo, error) {
	s.logger.InfoContext(ctx, "stub: GetInvoices called",
		"org_id", orgID,
	)
	return []*types.Invoice{}, types.PageInfo{}, nil
}

func (s *StubBillingService) GetSubscription(ctx context.Context, orgID string) (*types.SubscriptionDetails, error) {
	s.logger.InfoContext(ctx, "stub: GetSubscription called",
		"org_id", orgID,
	)
	return &types.SubscriptionDetails{
		Plan:   types.PlanFree,
		Status: types.SubStatusActive,
	}, nil
}

// StubEmailProvider implements EmailProvider by logging calls and returning
// a fake message ID. Used when config.IsTestMode is true or APP_ENV=local.
type StubEmailProvider struct {
	logger *slog.Logger
}

// NewStubEmailProvider creates a new StubEmailProvider.
func NewStubEmailProvider(logger *slog.Logger) *StubEmailProvider {
	return &StubEmailProvider{logger: logger}
}

func (s *StubEmailProvider) Send(ctx context.Context, input types.SendInput) (string, error) {
	s.logger.InfoContext(ctx, "stub: Send email called",
		"to", input.To,
		"template_id", input.TemplateID,
		"from", input.From.Address,
	)
	return fmt.Sprintf("msg_stub_%s", input.ReferenceID), nil
}

// StubOAuthProvider implements OAuthProvider by returning a fixed test profile.
// Used when config.IsTestMode is true or APP_ENV=local.
type StubOAuthProvider struct {
	name   string
	logger *slog.Logger
}

// NewStubOAuthProvider creates a new StubOAuthProvider with the given provider name.
func NewStubOAuthProvider(name string, logger *slog.Logger) *StubOAuthProvider {
	return &StubOAuthProvider{name: name, logger: logger}
}

func (s *StubOAuthProvider) Name() string {
	return s.name
}

func (s *StubOAuthProvider) GetLoginURL(state string) string {
	s.logger.Info("stub: GetLoginURL called",
		"provider", s.name,
		"state", state,
	)
	return fmt.Sprintf("https://stub.local/oauth/%s?state=%s", s.name, state)
}

func (s *StubOAuthProvider) Exchange(ctx context.Context, code string) (*types.OAuthProfile, error) {
	s.logger.InfoContext(ctx, "stub: Exchange called",
		"provider", s.name,
		"code", code,
	)
	return &types.OAuthProfile{
		Provider:      s.name,
		ProviderID:    "stub_user_12345",
		Email:         "stub@example.com",
		Name:          "Stub User",
		AvatarURL:     "https://stub.local/avatar.png",
		EmailVerified: true,
	}, nil
}

// StubOAuthManager implements OAuthManager using stub providers.
// Used when config.IsTestMode is true or APP_ENV=local.
type StubOAuthManager struct {
	providers map[string]OAuthProvider
}

// NewStubOAuthManager creates a new StubOAuthManager with stub providers
// for the given provider names.
func NewStubOAuthManager(logger *slog.Logger, providerNames ...string) *StubOAuthManager {
	m := &StubOAuthManager{
		providers: make(map[string]OAuthProvider, len(providerNames)),
	}
	for _, name := range providerNames {
		m.providers[name] = NewStubOAuthProvider(name, logger)
	}
	return m
}

func (m *StubOAuthManager) GetProvider(name string) (OAuthProvider, error) {
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

// StubWebhookVerifier implements WebhookVerifier by always succeeding.
// Used when config.IsTestMode is true or APP_ENV=local.
type StubWebhookVerifier struct {
	logger *slog.Logger
}

// NewStubWebhookVerifier creates a new StubWebhookVerifier.
func NewStubWebhookVerifier(logger *slog.Logger) *StubWebhookVerifier {
	return &StubWebhookVerifier{logger: logger}
}

func (s *StubWebhookVerifier) Verify(payload []byte, header string, secret string) error {
	s.logger.Info("stub: Stripe webhook Verify called",
		"payload_len", len(payload),
	)
	return nil
}

// StubEmailVerifier implements EmailVerifier by always returning valid.
// Used when config.IsTestMode is true or APP_ENV=local.
type StubEmailVerifier struct {
	logger *slog.Logger
}

// NewStubEmailVerifier creates a new StubEmailVerifier.
func NewStubEmailVerifier(logger *slog.Logger) *StubEmailVerifier {
	return &StubEmailVerifier{logger: logger}
}

func (s *StubEmailVerifier) Verify(payload []byte, signature string, timestamp string, publicKey string) (bool, error) {
	s.logger.Info("stub: SendGrid email Verify called",
		"payload_len", len(payload),
	)
	return true, nil
}

// ---------------------------------------------------------------------------
// Interface Compliance
// ---------------------------------------------------------------------------

var _ BillingService = (*StubBillingService)(nil)
var _ EmailProvider = (*StubEmailProvider)(nil)
var _ OAuthProvider = (*StubOAuthProvider)(nil)
var _ OAuthManager = (*StubOAuthManager)(nil)
var _ WebhookVerifier = (*StubWebhookVerifier)(nil)
var _ EmailVerifier = (*StubEmailVerifier)(nil)
