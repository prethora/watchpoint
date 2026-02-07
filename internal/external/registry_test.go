package external

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"watchpoint/internal/config"
	"watchpoint/internal/types"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestNewClientRegistry_TestModeReturnsStubs verifies that when IsTestMode is
// true, the registry returns stub implementations for all service interfaces.
func TestNewClientRegistry_TestModeReturnsStubs(t *testing.T) {
	cfg := &config.Config{
		IsTestMode:  true,
		Environment: "dev",
	}

	reg, err := NewClientRegistry(cfg, aws.Config{}, testLogger())
	if err != nil {
		t.Fatalf("NewClientRegistry returned error: %v", err)
	}

	// Verify all fields are populated.
	if reg.Billing == nil {
		t.Fatal("Billing is nil")
	}
	if reg.Email == nil {
		t.Fatal("Email is nil")
	}
	if reg.OAuth == nil {
		t.Fatal("OAuth is nil")
	}
	if reg.StripeVerifier == nil {
		t.Fatal("StripeVerifier is nil")
	}

	// Verify they are stub implementations.
	if _, ok := reg.Billing.(*StubBillingService); !ok {
		t.Errorf("Billing is %T, want *StubBillingService", reg.Billing)
	}
	if _, ok := reg.Email.(*StubEmailProvider); !ok {
		t.Errorf("Email is %T, want *StubEmailProvider", reg.Email)
	}
	if _, ok := reg.OAuth.(*StubOAuthManager); !ok {
		t.Errorf("OAuth is %T, want *StubOAuthManager", reg.OAuth)
	}
	if _, ok := reg.StripeVerifier.(*StubWebhookVerifier); !ok {
		t.Errorf("StripeVerifier is %T, want *StubWebhookVerifier", reg.StripeVerifier)
	}
}

// TestNewClientRegistry_LocalEnvReturnsStubs verifies that when Environment is
// "local", the registry returns stub implementations even if IsTestMode is false.
func TestNewClientRegistry_LocalEnvReturnsStubs(t *testing.T) {
	cfg := &config.Config{
		IsTestMode:  false,
		Environment: "local",
	}

	reg, err := NewClientRegistry(cfg, aws.Config{}, testLogger())
	if err != nil {
		t.Fatalf("NewClientRegistry returned error: %v", err)
	}

	if _, ok := reg.Billing.(*StubBillingService); !ok {
		t.Errorf("Billing is %T, want *StubBillingService", reg.Billing)
	}
	if _, ok := reg.Email.(*StubEmailProvider); !ok {
		t.Errorf("Email is %T, want *StubEmailProvider", reg.Email)
	}
	if _, ok := reg.OAuth.(*StubOAuthManager); !ok {
		t.Errorf("OAuth is %T, want *StubOAuthManager", reg.OAuth)
	}
}

// TestNewClientRegistry_ProductionReturnsRealClients verifies that when neither
// IsTestMode nor local environment is set, real client implementations are used.
func TestNewClientRegistry_ProductionReturnsRealClients(t *testing.T) {
	cfg := &config.Config{
		IsTestMode:  false,
		Environment: "prod",
		Billing: config.BillingConfig{
			StripeSecretKey: types.SecretString("sk_test_fake"),
		},
		Auth: config.AuthConfig{
			GoogleClientID:     "google-client-id",
			GoogleClientSecret: types.SecretString("google-secret"),
			GithubClientID:     "github-client-id",
			GithubClientSecret: types.SecretString("github-secret"),
		},
		Server: config.ServerConfig{
			APIExternalURL: "https://api.watchpoint.io",
		},
	}

	reg, err := NewClientRegistry(cfg, aws.Config{}, testLogger())
	if err != nil {
		t.Fatalf("NewClientRegistry returned error: %v", err)
	}

	// Verify real implementations are used.
	if _, ok := reg.Billing.(*StripeClient); !ok {
		t.Errorf("Billing is %T, want *StripeClient", reg.Billing)
	}
	if _, ok := reg.Email.(*SESClient); !ok {
		t.Errorf("Email is %T, want *SESClient", reg.Email)
	}
	if _, ok := reg.OAuth.(*OAuthManagerImpl); !ok {
		t.Errorf("OAuth is %T, want *OAuthManagerImpl", reg.OAuth)
	}
	if _, ok := reg.StripeVerifier.(*StripeVerifier); !ok {
		t.Errorf("StripeVerifier is %T, want *StripeVerifier", reg.StripeVerifier)
	}
}

// TestNewClientRegistry_NilLoggerDefaultsToSlog verifies that passing a nil
// logger does not cause a panic.
func TestNewClientRegistry_NilLoggerDefaultsToSlog(t *testing.T) {
	cfg := &config.Config{
		IsTestMode:  true,
		Environment: "dev",
	}

	reg, err := NewClientRegistry(cfg, aws.Config{}, nil)
	if err != nil {
		t.Fatalf("NewClientRegistry returned error: %v", err)
	}
	if reg.Billing == nil {
		t.Fatal("Billing is nil with nil logger")
	}
}

// TestStubBillingService_EnsureCustomer verifies the stub returns a predictable
// customer ID.
func TestStubBillingService_EnsureCustomer(t *testing.T) {
	stub := NewStubBillingService(testLogger())
	customerID, err := stub.EnsureCustomer(context.Background(), "org_123", "test@example.com")
	if err != nil {
		t.Fatalf("EnsureCustomer returned error: %v", err)
	}
	if customerID != "cus_stub_org_123" {
		t.Errorf("EnsureCustomer = %q, want %q", customerID, "cus_stub_org_123")
	}
}

// TestStubBillingService_CreateCheckoutSession verifies the stub returns
// predictable checkout URL and session ID.
func TestStubBillingService_CreateCheckoutSession(t *testing.T) {
	stub := NewStubBillingService(testLogger())
	url, sessionID, err := stub.CreateCheckoutSession(
		context.Background(),
		"org_123",
		types.PlanPro,
		types.RedirectURLs{Success: "https://example.com/success", Cancel: "https://example.com/cancel"},
	)
	if err != nil {
		t.Fatalf("CreateCheckoutSession returned error: %v", err)
	}
	if url != "https://checkout.stub.local/session" {
		t.Errorf("URL = %q, want %q", url, "https://checkout.stub.local/session")
	}
	if sessionID != "cs_stub_org_123" {
		t.Errorf("SessionID = %q, want %q", sessionID, "cs_stub_org_123")
	}
}

// TestStubBillingService_GetSubscription verifies the stub returns a free-tier
// active subscription.
func TestStubBillingService_GetSubscription(t *testing.T) {
	stub := NewStubBillingService(testLogger())
	details, err := stub.GetSubscription(context.Background(), "org_123")
	if err != nil {
		t.Fatalf("GetSubscription returned error: %v", err)
	}
	if details.Plan != types.PlanFree {
		t.Errorf("Plan = %q, want %q", details.Plan, types.PlanFree)
	}
	if details.Status != types.SubStatusActive {
		t.Errorf("Status = %q, want %q", details.Status, types.SubStatusActive)
	}
}

// TestStubEmailProvider_Send verifies the stub returns a predictable message ID.
func TestStubEmailProvider_Send(t *testing.T) {
	stub := NewStubEmailProvider(testLogger())
	msgID, err := stub.Send(context.Background(), types.SendInput{
		To:          "recipient@example.com",
		From:        types.SenderIdentity{Address: "alerts@watchpoint.io", Name: "WatchPoint"},
		Subject:     "Test Alert",
		ReferenceID: "ref_abc",
	})
	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if msgID != "msg_stub_ref_abc" {
		t.Errorf("MessageID = %q, want %q", msgID, "msg_stub_ref_abc")
	}
}

// TestStubOAuthProvider_Exchange verifies the stub returns a fixed test profile.
func TestStubOAuthProvider_Exchange(t *testing.T) {
	stub := NewStubOAuthProvider("google", testLogger())
	profile, err := stub.Exchange(context.Background(), "test_code")
	if err != nil {
		t.Fatalf("Exchange returned error: %v", err)
	}
	if profile.Provider != "google" {
		t.Errorf("Provider = %q, want %q", profile.Provider, "google")
	}
	if profile.Email != "stub@example.com" {
		t.Errorf("Email = %q, want %q", profile.Email, "stub@example.com")
	}
	if !profile.EmailVerified {
		t.Error("EmailVerified = false, want true")
	}
}

// TestStubOAuthManager_GetProvider verifies that stub OAuth manager resolves
// known providers and returns an error for unknown ones.
func TestStubOAuthManager_GetProvider(t *testing.T) {
	mgr := NewStubOAuthManager(testLogger(), "google", "github")

	p, err := mgr.GetProvider("google")
	if err != nil {
		t.Fatalf("GetProvider(google) returned error: %v", err)
	}
	if p.Name() != "google" {
		t.Errorf("Name() = %q, want %q", p.Name(), "google")
	}

	_, err = mgr.GetProvider("facebook")
	if err == nil {
		t.Fatal("GetProvider(facebook) expected error, got nil")
	}
}

// TestStubWebhookVerifier_AlwaysSucceeds verifies the stub verifier never
// returns an error.
func TestStubWebhookVerifier_AlwaysSucceeds(t *testing.T) {
	stub := NewStubWebhookVerifier(testLogger())
	err := stub.Verify([]byte("payload"), "sig_header", "secret")
	if err != nil {
		t.Errorf("Verify returned error: %v", err)
	}
}

// TestNewClientRegistry_ProductionWithOrgBillingLookup verifies that the
// WithOrgBillingLookup option is properly passed to the StripeClient.
func TestNewClientRegistry_ProductionWithOrgBillingLookup(t *testing.T) {
	cfg := &config.Config{
		IsTestMode:  false,
		Environment: "prod",
		Billing: config.BillingConfig{
			StripeSecretKey: types.SecretString("sk_test_fake"),
		},
		Server: config.ServerConfig{
			APIExternalURL: "https://api.watchpoint.io",
		},
	}

	mockLookup := &registryMockOrgBillingLookup{}

	reg, err := NewClientRegistry(cfg, aws.Config{}, testLogger(), WithOrgBillingLookup(mockLookup))
	if err != nil {
		t.Fatalf("NewClientRegistry returned error: %v", err)
	}

	stripeClient, ok := reg.Billing.(*StripeClient)
	if !ok {
		t.Fatalf("Billing is %T, want *StripeClient", reg.Billing)
	}

	if stripeClient.orgLookup != mockLookup {
		t.Error("StripeClient.orgLookup does not match the injected mock")
	}
}

// registryMockOrgBillingLookup is a minimal test double for OrgBillingLookup.
type registryMockOrgBillingLookup struct{}

func (m *registryMockOrgBillingLookup) GetBillingInfo(ctx context.Context, orgID string) (string, string, error) {
	return "cus_test", "test@example.com", nil
}

func (m *registryMockOrgBillingLookup) UpdateStripeCustomerID(ctx context.Context, orgID string, customerID string) error {
	return nil
}
