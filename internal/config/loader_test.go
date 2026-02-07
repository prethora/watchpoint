package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testSecretProvider is a configurable mock for testing SSM resolution.
type testSecretProvider struct {
	values      map[string]string
	err         error
	calledWith  []string // records the keys passed to GetParametersBatch
	callCount   int
}

func (p *testSecretProvider) GetParametersBatch(_ context.Context, keys []string) (map[string]string, error) {
	p.callCount++
	p.calledWith = append(p.calledWith, keys...)
	if p.err != nil {
		return nil, p.err
	}
	result := make(map[string]string)
	for _, k := range keys {
		if v, ok := p.values[k]; ok {
			result[k] = v
		}
	}
	return result, nil
}

// setFullTestEnv sets all required environment variables for a valid Config.
// It uses t.Setenv so values are automatically cleaned up after the test.
func setFullTestEnv(t *testing.T) {
	t.Helper()

	// System metadata
	t.Setenv("APP_ENV", "local")
	t.Setenv("OTEL_SERVICE_NAME", "test-service")
	t.Setenv("LOG_LEVEL", "debug")

	// Server
	t.Setenv("API_EXTERNAL_URL", "https://api.test.local")
	t.Setenv("DASHBOARD_URL", "https://app.test.local")

	// Database
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/testdb")

	// AWS
	t.Setenv("FORECAST_BUCKET", "test-forecast-bucket")
	t.Setenv("SQS_EVAL_URGENT", "https://sqs.us-east-1.amazonaws.com/123/eval-urgent")
	t.Setenv("SQS_EVAL_STANDARD", "https://sqs.us-east-1.amazonaws.com/123/eval-standard")
	t.Setenv("SQS_NOTIFICATIONS", "https://sqs.us-east-1.amazonaws.com/123/notifications")
	t.Setenv("SQS_DLQ", "https://sqs.us-east-1.amazonaws.com/123/dlq")

	// Billing
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_abc123")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_test_456")
	t.Setenv("STRIPE_PUBLISHABLE_KEY", "pk_test_789")

	// Forecast
	t.Setenv("RUNPOD_API_KEY", "rp_test_key_456")
	t.Setenv("RUNPOD_ENDPOINT_ID", "test-endpoint-id")

	// Auth
	t.Setenv("SESSION_KEY", "a-very-long-session-key-that-is-at-least-32-chars-long")

	// Security
	t.Setenv("ADMIN_API_KEY", "admin-api-key-test-value")
}

// TestLoadConfigLocalSuccess verifies that LoadConfig successfully loads
// configuration in local mode with all required environment variables set.
func TestLoadConfigLocalSuccess(t *testing.T) {
	setFullTestEnv(t)

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	// Verify system metadata
	if cfg.Environment != "local" {
		t.Errorf("Environment = %q, want %q", cfg.Environment, "local")
	}
	if cfg.Service != "test-service" {
		t.Errorf("Service = %q, want %q", cfg.Service, "test-service")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}

	// Verify server config
	if cfg.Server.APIExternalURL != "https://api.test.local" {
		t.Errorf("Server.APIExternalURL = %q, want %q", cfg.Server.APIExternalURL, "https://api.test.local")
	}
	if cfg.Server.DashboardURL != "https://app.test.local" {
		t.Errorf("Server.DashboardURL = %q, want %q", cfg.Server.DashboardURL, "https://app.test.local")
	}

	// Verify defaults
	if cfg.Server.Port != "8080" {
		t.Errorf("Server.Port = %q, want default %q", cfg.Server.Port, "8080")
	}
	if cfg.Database.MaxConns != 10 {
		t.Errorf("Database.MaxConns = %d, want default 10", cfg.Database.MaxConns)
	}
	if cfg.Database.AcquireTimeout != 2*time.Second {
		t.Errorf("Database.AcquireTimeout = %v, want 2s", cfg.Database.AcquireTimeout)
	}

	// Verify secrets are wrapped in SecretString
	if cfg.Database.URL.Unmask() != "postgres://user:pass@localhost:5432/testdb" {
		t.Errorf("Database.URL.Unmask() = %q, want postgres URL", cfg.Database.URL.Unmask())
	}
	if cfg.Database.URL.String() != "***REDACTED***" {
		t.Errorf("Database.URL.String() should be redacted, got %q", cfg.Database.URL.String())
	}

	// Verify build info populated
	if cfg.Build.Version != "dev" {
		t.Errorf("Build.Version = %q, want %q", cfg.Build.Version, "dev")
	}

	// Verify feature flags defaults
	if !cfg.Feature.EnableNowcast {
		t.Error("Feature.EnableNowcast should default to true")
	}
	if !cfg.Feature.EnableEmail {
		t.Error("Feature.EnableEmail should default to true")
	}

	// Verify webhook config defaults
	if cfg.Webhook.UserAgent != "WatchPoint-Webhook/1.0" {
		t.Errorf("Webhook.UserAgent = %q, want default", cfg.Webhook.UserAgent)
	}
	if cfg.Webhook.DefaultTimeout != 10*time.Second {
		t.Errorf("Webhook.DefaultTimeout = %v, want 10s", cfg.Webhook.DefaultTimeout)
	}
	if cfg.Webhook.MaxRedirects != 3 {
		t.Errorf("Webhook.MaxRedirects = %d, want 3", cfg.Webhook.MaxRedirects)
	}
}

// TestLoadConfigSetsUTC verifies that LoadConfig sets time.Local to UTC.
func TestLoadConfigSetsUTC(t *testing.T) {
	setFullTestEnv(t)

	// Temporarily set to a non-UTC timezone to verify it gets reset.
	originalLocal := time.Local
	t.Cleanup(func() {
		time.Local = originalLocal
	})
	nyc, _ := time.LoadLocation("America/New_York")
	time.Local = nyc

	_, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if time.Local != time.UTC {
		t.Errorf("time.Local = %v, want UTC", time.Local)
	}
}

// TestLoadConfigValidationFailure verifies that LoadConfig returns a validation
// error when required fields are missing.
func TestLoadConfigValidationFailure(t *testing.T) {
	// Set only APP_ENV, leaving all required fields empty.
	t.Setenv("APP_ENV", "local")

	_, err := LoadConfig(nil)
	if err == nil {
		t.Fatal("expected error for missing required fields, got nil")
	}

	// The error could be a parsing error (envconfig fails on required fields)
	// or a validation error. Either way, it should be a ConfigError.
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *ConfigError, got %T: %v", err, err)
	}

	// The error type should indicate either parsing or validation failure.
	if cfgErr.Type != ErrParsing && cfgErr.Type != ErrValidation {
		t.Errorf("expected ErrParsing or ErrValidation, got %q", cfgErr.Type)
	}
}

// TestLoadConfigInvalidEnvironment verifies that LoadConfig returns a
// validation error when APP_ENV has an invalid value.
func TestLoadConfigInvalidEnvironment(t *testing.T) {
	setFullTestEnv(t)
	t.Setenv("APP_ENV", "invalid-env")

	_, err := LoadConfig(nil)
	if err == nil {
		t.Fatal("expected error for invalid APP_ENV, got nil")
	}

	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *ConfigError, got %T: %v", err, err)
	}
	if cfgErr.Type != ErrValidation {
		t.Errorf("expected ErrValidation, got %q", cfgErr.Type)
	}
}

// TestLoadConfigSSMResolution verifies that _SSM_PARAM variables are resolved
// via the SecretProvider when APP_ENV is not "local".
func TestLoadConfigSSMResolution(t *testing.T) {
	// Set up a non-local environment.
	t.Setenv("APP_ENV", "dev")
	t.Setenv("OTEL_SERVICE_NAME", "test-service")
	t.Setenv("LOG_LEVEL", "info")

	// Server
	t.Setenv("API_EXTERNAL_URL", "https://api.dev.test")
	t.Setenv("DASHBOARD_URL", "https://app.dev.test")

	// AWS
	t.Setenv("FORECAST_BUCKET", "dev-forecast-bucket")
	t.Setenv("SQS_EVAL_URGENT", "https://sqs.us-east-1.amazonaws.com/123/eval-urgent")
	t.Setenv("SQS_EVAL_STANDARD", "https://sqs.us-east-1.amazonaws.com/123/eval-standard")
	t.Setenv("SQS_NOTIFICATIONS", "https://sqs.us-east-1.amazonaws.com/123/notifications")
	t.Setenv("SQS_DLQ", "https://sqs.us-east-1.amazonaws.com/123/dlq")

	// Billing (non-secret)
	t.Setenv("STRIPE_PUBLISHABLE_KEY", "pk_test_789")

	// Forecast (non-secret)
	t.Setenv("RUNPOD_ENDPOINT_ID", "test-endpoint-id")

	// Set _SSM_PARAM pointers for all secrets
	t.Setenv("DATABASE_URL_SSM_PARAM", "/dev/watchpoint/database/url")
	t.Setenv("STRIPE_SECRET_KEY_SSM_PARAM", "/dev/watchpoint/billing/stripe_secret_key")
	t.Setenv("STRIPE_WEBHOOK_SECRET_SSM_PARAM", "/dev/watchpoint/billing/stripe_webhook_secret")
	t.Setenv("RUNPOD_API_KEY_SSM_PARAM", "/dev/watchpoint/forecast/runpod_api_key")
	t.Setenv("SESSION_KEY_SSM_PARAM", "/dev/watchpoint/auth/session_key")
	t.Setenv("ADMIN_API_KEY_SSM_PARAM", "/dev/watchpoint/security/admin_api_key")

	// Ensure target env vars (the ones SSM resolution will set) are NOT already
	// present in the OS environment. This prevents pre-existing env vars (e.g.,
	// from the shell profile) from causing SSM resolution to skip variables.
	// We save and restore any pre-existing values in cleanup.
	resolvedVars := []string{
		"DATABASE_URL", "STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET",
		"RUNPOD_API_KEY", "SESSION_KEY",
		"ADMIN_API_KEY",
	}
	savedVars := make(map[string]struct{ val string; ok bool })
	for _, v := range resolvedVars {
		val, ok := os.LookupEnv(v)
		savedVars[v] = struct{ val string; ok bool }{val, ok}
		os.Unsetenv(v)
	}
	t.Cleanup(func() {
		for _, v := range resolvedVars {
			saved := savedVars[v]
			if saved.ok {
				os.Setenv(v, saved.val)
			} else {
				os.Unsetenv(v)
			}
		}
	})

	provider := &testSecretProvider{
		values: map[string]string{
			"/dev/watchpoint/database/url":                "postgres://user:pass@rds.amazonaws.com/devdb",
			"/dev/watchpoint/billing/stripe_secret_key":   "sk_live_resolved",
			"/dev/watchpoint/billing/stripe_webhook_secret": "whsec_live_resolved",
			"/dev/watchpoint/forecast/runpod_api_key":     "rp_resolved_key",
			"/dev/watchpoint/auth/session_key":            "resolved-session-key-that-is-definitely-at-least-32-characters-long",
			"/dev/watchpoint/security/admin_api_key":      "resolved-admin-key",
		},
	}

	cfg, err := LoadConfig(provider)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	// Verify SSM-resolved values were injected correctly.
	if cfg.Database.URL.Unmask() != "postgres://user:pass@rds.amazonaws.com/devdb" {
		t.Errorf("Database.URL = %q, want resolved SSM value", cfg.Database.URL.Unmask())
	}
	if cfg.Billing.StripeSecretKey.Unmask() != "sk_live_resolved" {
		t.Errorf("Billing.StripeSecretKey = %q, want resolved SSM value", cfg.Billing.StripeSecretKey.Unmask())
	}
	if cfg.Billing.StripeWebhookSecret.Unmask() != "whsec_live_resolved" {
		t.Errorf("Billing.StripeWebhookSecret = %q, want resolved SSM value", cfg.Billing.StripeWebhookSecret.Unmask())
	}
	if cfg.Forecast.RunPodAPIKey.Unmask() != "rp_resolved_key" {
		t.Errorf("Forecast.RunPodAPIKey = %q, want resolved SSM value", cfg.Forecast.RunPodAPIKey.Unmask())
	}
	if cfg.Auth.SessionKey.Unmask() != "resolved-session-key-that-is-definitely-at-least-32-characters-long" {
		t.Errorf("Auth.SessionKey = %q, want resolved SSM value", cfg.Auth.SessionKey.Unmask())
	}
	if cfg.Security.AdminAPIKey.Unmask() != "resolved-admin-key" {
		t.Errorf("Security.AdminAPIKey = %q, want resolved SSM value", cfg.Security.AdminAPIKey.Unmask())
	}
	// Verify provider was called exactly once (single batch call).
	if provider.callCount != 1 {
		t.Errorf("provider.callCount = %d, want 1 (single batch call)", provider.callCount)
	}

	// Verify the correct number of SSM keys were requested.
	if len(provider.calledWith) != 6 {
		t.Errorf("provider was called with %d keys, want 6 (all SSM params)", len(provider.calledWith))
	}
}

// TestLoadConfigSSMSkippedForLocal verifies that SSM resolution is skipped
// when APP_ENV is "local", even if _SSM_PARAM variables are set.
func TestLoadConfigSSMSkippedForLocal(t *testing.T) {
	setFullTestEnv(t)

	// Also set some SSM params that should be ignored.
	t.Setenv("SOME_SECRET_SSM_PARAM", "/local/some/path")

	provider := &testSecretProvider{
		values: map[string]string{
			"/local/some/path": "should-not-be-used",
		},
	}

	cfg, err := LoadConfig(provider)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	// Verify the provider was NOT called.
	if provider.callCount != 0 {
		t.Errorf("provider.callCount = %d, want 0 (should not be called in local mode)", provider.callCount)
	}

	// Verify config was loaded from direct env vars.
	if cfg.Environment != "local" {
		t.Errorf("Environment = %q, want %q", cfg.Environment, "local")
	}
}

// TestLoadConfigSSMPriorityDirectEnvWins verifies that directly set environment
// variables take priority over SSM resolution (the priority chain:
// OS Environment > Dotenv > SSM).
func TestLoadConfigSSMPriorityDirectEnvWins(t *testing.T) {
	setFullTestEnv(t)
	t.Setenv("APP_ENV", "dev")

	// Set both a direct env var and its SSM param pointer.
	t.Setenv("DATABASE_URL", "postgres://direct-env-value/db")
	t.Setenv("DATABASE_URL_SSM_PARAM", "/dev/watchpoint/database/url")

	provider := &testSecretProvider{
		values: map[string]string{
			"/dev/watchpoint/database/url": "postgres://ssm-value/db",
		},
	}

	cfg, err := LoadConfig(provider)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	// The direct env var should win over SSM.
	if cfg.Database.URL.Unmask() != "postgres://direct-env-value/db" {
		t.Errorf("Database.URL = %q, want direct env value (not SSM)", cfg.Database.URL.Unmask())
	}
}

// TestLoadConfigSSMProviderError verifies that an error from the SecretProvider
// is properly propagated as a ConfigError with ErrSSMResolution type.
func TestLoadConfigSSMProviderError(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	t.Setenv("DATABASE_URL_SSM_PARAM", "/dev/watchpoint/database/url")

	provider := &testSecretProvider{
		err: fmt.Errorf("SSM throttled"),
	}

	_, err := LoadConfig(provider)
	if err == nil {
		t.Fatal("expected error when provider fails, got nil")
	}

	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *ConfigError, got %T: %v", err, err)
	}
	if cfgErr.Type != ErrSSMResolution {
		t.Errorf("expected ErrSSMResolution, got %q", cfgErr.Type)
	}
}

// TestLoadConfigSSMNilProviderNonLocal verifies that a nil provider in
// non-local mode returns an error when SSM params need to be resolved.
func TestLoadConfigSSMNilProviderNonLocal(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	t.Setenv("DATABASE_URL_SSM_PARAM", "/dev/watchpoint/database/url")

	_, err := LoadConfig(nil)
	if err == nil {
		t.Fatal("expected error for nil provider in non-local mode, got nil")
	}

	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *ConfigError, got %T: %v", err, err)
	}
	if cfgErr.Type != ErrSSMResolution {
		t.Errorf("expected ErrSSMResolution, got %q", cfgErr.Type)
	}
}

// TestLoadConfigSSMMissingParameter verifies that an error is returned when
// the provider returns a result that doesn't include all requested parameters.
func TestLoadConfigSSMMissingParameter(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	t.Setenv("DATABASE_URL_SSM_PARAM", "/dev/watchpoint/database/url")

	// Provider returns empty map (parameter not found).
	provider := &testSecretProvider{
		values: map[string]string{},
	}

	_, err := LoadConfig(provider)
	if err == nil {
		t.Fatal("expected error for missing SSM parameter, got nil")
	}

	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *ConfigError, got %T: %v", err, err)
	}
	if cfgErr.Type != ErrSSMResolution {
		t.Errorf("expected ErrSSMResolution, got %q", cfgErr.Type)
	}
	if !strings.Contains(cfgErr.Message, "DATABASE_URL") {
		t.Errorf("error message should mention DATABASE_URL, got: %s", cfgErr.Message)
	}
}

// TestLoadConfigDotenvFile verifies that .env file loading works correctly.
func TestLoadConfigDotenvFile(t *testing.T) {
	// Create a temporary directory with a .env file.
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")

	// Write a .env file with some values.
	envContent := `APP_ENV=local
API_EXTERNAL_URL=https://api.dotenv.local
DASHBOARD_URL=https://app.dotenv.local
DATABASE_URL=postgres://dotenv:pass@localhost/dotenvdb
FORECAST_BUCKET=dotenv-bucket
SQS_EVAL_URGENT=https://sqs.us-east-1.amazonaws.com/123/urgent
SQS_EVAL_STANDARD=https://sqs.us-east-1.amazonaws.com/123/standard
SQS_NOTIFICATIONS=https://sqs.us-east-1.amazonaws.com/123/notif
SQS_DLQ=https://sqs.us-east-1.amazonaws.com/123/dlq
STRIPE_SECRET_KEY=sk_test_dotenv
STRIPE_WEBHOOK_SECRET=whsec_dotenv
STRIPE_PUBLISHABLE_KEY=pk_test_dotenv
RUNPOD_API_KEY=rp_dotenv
RUNPOD_ENDPOINT_ID=dotenv-endpoint
SESSION_KEY=dotenv-session-key-that-is-at-least-32-characters-long
ADMIN_API_KEY=dotenv-admin-key
`
	if err := os.WriteFile(envFile, []byte(envContent), 0644); err != nil {
		t.Fatalf("failed to write .env file: %v", err)
	}

	// Change to the temp directory so godotenv.Load() finds the .env file.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to change directory: %v", err)
	}
	t.Cleanup(func() {
		os.Chdir(origDir)
	})

	// Clear env vars that might interfere (godotenv does NOT override existing vars).
	// We need to ensure these are NOT set so the .env file values are used.
	envVarsToClear := []string{
		"APP_ENV", "API_EXTERNAL_URL", "DASHBOARD_URL", "DATABASE_URL",
		"FORECAST_BUCKET", "SQS_EVAL_URGENT", "SQS_EVAL_STANDARD",
		"SQS_NOTIFICATIONS", "SQS_DLQ", "STRIPE_SECRET_KEY",
		"STRIPE_WEBHOOK_SECRET", "STRIPE_PUBLISHABLE_KEY",
		"RUNPOD_API_KEY",
		"RUNPOD_ENDPOINT_ID", "SESSION_KEY", "ADMIN_API_KEY",
	}
	for _, v := range envVarsToClear {
		os.Unsetenv(v)
		t.Cleanup(func() {
			os.Unsetenv(v)
		})
	}

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig with .env file returned error: %v", err)
	}

	// Verify values came from the .env file.
	if cfg.Server.APIExternalURL != "https://api.dotenv.local" {
		t.Errorf("APIExternalURL = %q, want value from .env file", cfg.Server.APIExternalURL)
	}
	if cfg.Database.URL.Unmask() != "postgres://dotenv:pass@localhost/dotenvdb" {
		t.Errorf("Database.URL = %q, want value from .env file", cfg.Database.URL.Unmask())
	}
}

// TestLoadConfigEnvOverridesDotenv verifies that OS environment variables
// take priority over .env file values.
func TestLoadConfigEnvOverridesDotenv(t *testing.T) {
	// Create a temporary .env file.
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")

	envContent := `APP_ENV=local
API_EXTERNAL_URL=https://api.from-dotenv.local
DASHBOARD_URL=https://app.from-dotenv.local
DATABASE_URL=postgres://dotenv:pass@localhost/db
FORECAST_BUCKET=dotenv-bucket
SQS_EVAL_URGENT=https://sqs.us-east-1.amazonaws.com/123/urgent
SQS_EVAL_STANDARD=https://sqs.us-east-1.amazonaws.com/123/standard
SQS_NOTIFICATIONS=https://sqs.us-east-1.amazonaws.com/123/notif
SQS_DLQ=https://sqs.us-east-1.amazonaws.com/123/dlq
STRIPE_SECRET_KEY=sk_dotenv
STRIPE_WEBHOOK_SECRET=whsec_dotenv
STRIPE_PUBLISHABLE_KEY=pk_dotenv
RUNPOD_API_KEY=rp_dotenv
RUNPOD_ENDPOINT_ID=dotenv-endpoint
SESSION_KEY=dotenv-session-key-that-is-at-least-32-characters-long
ADMIN_API_KEY=dotenv-admin-key
`
	if err := os.WriteFile(envFile, []byte(envContent), 0644); err != nil {
		t.Fatalf("failed to write .env file: %v", err)
	}

	// Change to temp directory.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to change directory: %v", err)
	}
	t.Cleanup(func() {
		os.Chdir(origDir)
	})

	// Clear potentially interfering vars and set the ones we want to override.
	envVarsToClear := []string{
		"API_EXTERNAL_URL", "DASHBOARD_URL", "DATABASE_URL",
		"FORECAST_BUCKET", "SQS_EVAL_URGENT", "SQS_EVAL_STANDARD",
		"SQS_NOTIFICATIONS", "SQS_DLQ", "STRIPE_SECRET_KEY",
		"STRIPE_WEBHOOK_SECRET", "STRIPE_PUBLISHABLE_KEY",
		"RUNPOD_API_KEY",
		"RUNPOD_ENDPOINT_ID", "SESSION_KEY", "ADMIN_API_KEY",
	}
	for _, v := range envVarsToClear {
		os.Unsetenv(v)
		t.Cleanup(func() {
			os.Unsetenv(v)
		})
	}

	// Set one env var that should override the .env value.
	t.Setenv("APP_ENV", "local")
	t.Setenv("API_EXTERNAL_URL", "https://api.from-os-env.local")

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	// The OS env var should win over .env file.
	if cfg.Server.APIExternalURL != "https://api.from-os-env.local" {
		t.Errorf("APIExternalURL = %q, want OS env value, not dotenv value", cfg.Server.APIExternalURL)
	}
}

// TestLoadConfigNilProviderLocalModeOK verifies that passing a nil provider
// is acceptable in local mode (SSM resolution is skipped entirely).
func TestLoadConfigNilProviderLocalModeOK(t *testing.T) {
	setFullTestEnv(t)

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig with nil provider in local mode should succeed, got: %v", err)
	}
	if cfg.Environment != "local" {
		t.Errorf("Environment = %q, want %q", cfg.Environment, "local")
	}
}

// TestLoadConfigNilProviderNonLocalNoSSMParams verifies that a nil provider
// is acceptable in non-local mode if there are no _SSM_PARAM variables set.
func TestLoadConfigNilProviderNonLocalNoSSMParams(t *testing.T) {
	setFullTestEnv(t)
	t.Setenv("APP_ENV", "dev")

	// No _SSM_PARAM variables are set, and all required values are directly
	// set in the environment, so SSM resolution is a no-op.
	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig should succeed when no SSM params need resolution: %v", err)
	}
	if cfg.Environment != "dev" {
		t.Errorf("Environment = %q, want %q", cfg.Environment, "dev")
	}
}

// TestConfigErrorError verifies the ConfigError.Error() method formatting.
func TestConfigErrorError(t *testing.T) {
	tests := []struct {
		name     string
		err      *ConfigError
		wantStr  string
	}{
		{
			name: "with underlying error",
			err: &ConfigError{
				Type:    ErrSSMResolution,
				Message: "failed to fetch",
				Err:     fmt.Errorf("connection timeout"),
			},
			wantStr: "[SSM_FAILURE] failed to fetch: connection timeout",
		},
		{
			name: "without underlying error",
			err: &ConfigError{
				Type:    ErrMissingEnv,
				Message: "DATABASE_URL not set",
			},
			wantStr: "[MISSING_ENV] DATABASE_URL not set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.wantStr {
				t.Errorf("ConfigError.Error() = %q, want %q", got, tt.wantStr)
			}
		})
	}
}

// TestConfigErrorUnwrap verifies that ConfigError.Unwrap() returns the
// underlying error for use with errors.Is/errors.As.
func TestConfigErrorUnwrap(t *testing.T) {
	underlying := fmt.Errorf("root cause")
	cfgErr := &ConfigError{
		Type:    ErrSSMResolution,
		Message: "test",
		Err:     underlying,
	}

	if unwrapped := cfgErr.Unwrap(); unwrapped != underlying {
		t.Errorf("Unwrap() = %v, want %v", unwrapped, underlying)
	}

	// Verify errors.Is works through the chain.
	if !errors.Is(cfgErr, underlying) {
		t.Error("errors.Is should find the underlying error through Unwrap")
	}
}

// TestResolveSSMParamsInternalLogic tests the SSM resolution logic with
// injectable dependencies to avoid global state mutation.
func TestResolveSSMParamsInternalLogic(t *testing.T) {
	// Set up a mock environment.
	envMap := map[string]string{
		"APP_ENV":                      "staging",
		"DATABASE_URL_SSM_PARAM":       "/staging/db/url",
		"SESSION_KEY_SSM_PARAM":        "/staging/auth/session_key",
		"ADMIN_API_KEY":                "already-set-directly", // Direct env var should prevent SSM resolution
		"ADMIN_API_KEY_SSM_PARAM":      "/staging/security/admin_api_key",
	}

	deps := loaderDeps{
		lookupEnv: func(key string) (string, bool) {
			v, ok := envMap[key]
			return v, ok
		},
		setEnv: func(key, value string) error {
			envMap[key] = value
			return nil
		},
		environ: func() []string {
			result := make([]string, 0, len(envMap))
			for k, v := range envMap {
				result = append(result, k+"="+v)
			}
			return result
		},
	}

	provider := &testSecretProvider{
		values: map[string]string{
			"/staging/db/url":                  "postgres://resolved",
			"/staging/auth/session_key":        "resolved-session-key",
			"/staging/security/admin_api_key":  "should-not-be-used",
		},
	}

	err := resolveSSMParams(provider, deps)
	if err != nil {
		t.Fatalf("resolveSSMParams returned error: %v", err)
	}

	// DATABASE_URL should be resolved from SSM.
	if v, ok := envMap["DATABASE_URL"]; !ok || v != "postgres://resolved" {
		t.Errorf("DATABASE_URL = %q, want %q", v, "postgres://resolved")
	}

	// SESSION_KEY should be resolved from SSM.
	if v, ok := envMap["SESSION_KEY"]; !ok || v != "resolved-session-key" {
		t.Errorf("SESSION_KEY = %q, want %q", v, "resolved-session-key")
	}

	// ADMIN_API_KEY should remain unchanged (direct env var takes priority).
	if v := envMap["ADMIN_API_KEY"]; v != "already-set-directly" {
		t.Errorf("ADMIN_API_KEY = %q, want %q (direct env should win)", v, "already-set-directly")
	}

	// Provider should have been called with only the two paths that need resolution.
	// (ADMIN_API_KEY was skipped because it's already set directly.)
	if provider.callCount != 1 {
		t.Errorf("provider.callCount = %d, want 1", provider.callCount)
	}
	if len(provider.calledWith) != 2 {
		t.Errorf("provider was called with %d keys, want 2", len(provider.calledWith))
	}
}

// TestResolveSSMParamsEmptySSMPath verifies that empty SSM paths are skipped.
func TestResolveSSMParamsEmptySSMPath(t *testing.T) {
	envMap := map[string]string{
		"APP_ENV":                    "dev",
		"EMPTY_SECRET_SSM_PARAM":    "", // Empty SSM path
	}

	deps := loaderDeps{
		lookupEnv: func(key string) (string, bool) {
			v, ok := envMap[key]
			return v, ok
		},
		setEnv: func(key, value string) error {
			envMap[key] = value
			return nil
		},
		environ: func() []string {
			result := make([]string, 0, len(envMap))
			for k, v := range envMap {
				result = append(result, k+"="+v)
			}
			return result
		},
	}

	provider := &testSecretProvider{values: map[string]string{}}

	err := resolveSSMParams(provider, deps)
	if err != nil {
		t.Fatalf("resolveSSMParams returned error: %v", err)
	}

	// Provider should not have been called (no valid SSM paths).
	if provider.callCount != 0 {
		t.Errorf("provider.callCount = %d, want 0", provider.callCount)
	}
}

// TestLoadConfigReturnsPointer verifies that LoadConfig returns a pointer to
// Config, not a value type.
func TestLoadConfigReturnsPointer(t *testing.T) {
	setFullTestEnv(t)

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig returned nil config without error")
	}
}

// TestLoadConfigSliceFields verifies that comma-separated envconfig values
// are properly parsed into slices.
func TestLoadConfigSliceFields(t *testing.T) {
	setFullTestEnv(t)
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://app.watchpoint.io,https://admin.watchpoint.io")
	t.Setenv("UPSTREAM_MIRRORS", "noaa-gfs-bdp-pds,aws-noaa-gfs,backup-mirror")

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if len(cfg.Security.CorsAllowedOrigins) != 2 {
		t.Errorf("CorsAllowedOrigins length = %d, want 2", len(cfg.Security.CorsAllowedOrigins))
	}
	if len(cfg.Forecast.UpstreamMirrors) != 3 {
		t.Errorf("UpstreamMirrors length = %d, want 3", len(cfg.Forecast.UpstreamMirrors))
	}
}

// --- Supplementary tests for Section 6.2 coverage ---

// TestLoadConfigIsTestModeFlag verifies that IS_TEST_MODE=true is correctly
// parsed into Config.IsTestMode boolean.
func TestLoadConfigIsTestModeFlag(t *testing.T) {
	setFullTestEnv(t)
	t.Setenv("IS_TEST_MODE", "true")

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if !cfg.IsTestMode {
		t.Error("IsTestMode should be true when IS_TEST_MODE=true")
	}
}

// TestLoadConfigDurationOverrides verifies that custom (non-default) duration
// values are correctly parsed by envconfig into time.Duration fields.
func TestLoadConfigDurationOverrides(t *testing.T) {
	setFullTestEnv(t)
	t.Setenv("DB_MAX_CONN_LIFETIME", "1h")
	t.Setenv("DB_ACQUIRE_TIMEOUT", "5s")
	t.Setenv("DB_HEALTH_CHECK_PERIOD", "30s")
	t.Setenv("WEBHOOK_TIMEOUT", "15s")
	t.Setenv("TIMEOUT_MEDIUM_RANGE", "6h")
	t.Setenv("TIMEOUT_NOWCAST", "45m")

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.Database.MaxConnLifetime != 1*time.Hour {
		t.Errorf("Database.MaxConnLifetime = %v, want 1h", cfg.Database.MaxConnLifetime)
	}
	if cfg.Database.AcquireTimeout != 5*time.Second {
		t.Errorf("Database.AcquireTimeout = %v, want 5s", cfg.Database.AcquireTimeout)
	}
	if cfg.Database.HealthCheckPeriod != 30*time.Second {
		t.Errorf("Database.HealthCheckPeriod = %v, want 30s", cfg.Database.HealthCheckPeriod)
	}
	if cfg.Webhook.DefaultTimeout != 15*time.Second {
		t.Errorf("Webhook.DefaultTimeout = %v, want 15s", cfg.Webhook.DefaultTimeout)
	}
	if cfg.Forecast.TimeoutMediumRange != 6*time.Hour {
		t.Errorf("Forecast.TimeoutMediumRange = %v, want 6h", cfg.Forecast.TimeoutMediumRange)
	}
	if cfg.Forecast.TimeoutNowcast != 45*time.Minute {
		t.Errorf("Forecast.TimeoutNowcast = %v, want 45m", cfg.Forecast.TimeoutNowcast)
	}
}

// TestLoadConfigDatabasePoolDefaults verifies that all database pool tuning
// parameters receive their correct default values.
func TestLoadConfigDatabasePoolDefaults(t *testing.T) {
	setFullTestEnv(t)

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.Database.MaxConns != 10 {
		t.Errorf("Database.MaxConns = %d, want 10", cfg.Database.MaxConns)
	}
	if cfg.Database.MinConns != 2 {
		t.Errorf("Database.MinConns = %d, want 2", cfg.Database.MinConns)
	}
	if cfg.Database.MaxConnLifetime != 30*time.Minute {
		t.Errorf("Database.MaxConnLifetime = %v, want 30m", cfg.Database.MaxConnLifetime)
	}
	if cfg.Database.AcquireTimeout != 2*time.Second {
		t.Errorf("Database.AcquireTimeout = %v, want 2s", cfg.Database.AcquireTimeout)
	}
	if cfg.Database.HealthCheckPeriod != 1*time.Minute {
		t.Errorf("Database.HealthCheckPeriod = %v, want 1m", cfg.Database.HealthCheckPeriod)
	}
}

// TestLoadConfigEmailDefaults verifies that email configuration fields
// receive their correct default values.
func TestLoadConfigEmailDefaults(t *testing.T) {
	setFullTestEnv(t)

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.Email.FromAddress != "alerts@watchpoint.io" {
		t.Errorf("Email.FromAddress = %q, want %q", cfg.Email.FromAddress, "alerts@watchpoint.io")
	}
	if cfg.Email.FromName != "WatchPoint Alerts" {
		t.Errorf("Email.FromName = %q, want %q", cfg.Email.FromName, "WatchPoint Alerts")
	}
	if cfg.Email.SESRegion != "us-east-1" {
		t.Errorf("Email.SESRegion = %q, want %q", cfg.Email.SESRegion, "us-east-1")
	}
	if cfg.Email.Provider != "ses" {
		t.Errorf("Email.Provider = %q, want %q", cfg.Email.Provider, "ses")
	}
}

// TestLoadConfigObservabilityDefaults verifies that observability settings
// receive their correct default values.
func TestLoadConfigObservabilityDefaults(t *testing.T) {
	setFullTestEnv(t)

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.Observability.MetricNamespace != "WatchPoint" {
		t.Errorf("Observability.MetricNamespace = %q, want %q", cfg.Observability.MetricNamespace, "WatchPoint")
	}
	if !cfg.Observability.EnableTracing {
		t.Error("Observability.EnableTracing should default to true")
	}
}

// TestLoadConfigAWSDefaults verifies that AWS config fields receive correct
// default values, including optional fields.
func TestLoadConfigAWSDefaults(t *testing.T) {
	setFullTestEnv(t)

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.AWS.Region != "us-east-1" {
		t.Errorf("AWS.Region = %q, want %q", cfg.AWS.Region, "us-east-1")
	}
	// ArchiveBucket and EndpointURL are optional with no default.
	if cfg.AWS.ArchiveBucket != "" {
		t.Errorf("AWS.ArchiveBucket = %q, want empty (optional field)", cfg.AWS.ArchiveBucket)
	}
	if cfg.AWS.EndpointURL != "" {
		t.Errorf("AWS.EndpointURL = %q, want empty (optional field)", cfg.AWS.EndpointURL)
	}
}

// TestLoadConfigForecastDefaults verifies default values for forecast-related
// configuration including timeouts and upstream mirrors.
func TestLoadConfigForecastDefaults(t *testing.T) {
	setFullTestEnv(t)

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.Forecast.TimeoutMediumRange != 3*time.Hour {
		t.Errorf("Forecast.TimeoutMediumRange = %v, want 3h", cfg.Forecast.TimeoutMediumRange)
	}
	if cfg.Forecast.TimeoutNowcast != 20*time.Minute {
		t.Errorf("Forecast.TimeoutNowcast = %v, want 20m", cfg.Forecast.TimeoutNowcast)
	}
	if len(cfg.Forecast.UpstreamMirrors) != 2 {
		t.Fatalf("Forecast.UpstreamMirrors length = %d, want 2", len(cfg.Forecast.UpstreamMirrors))
	}
	if cfg.Forecast.UpstreamMirrors[0] != "noaa-gfs-bdp-pds" {
		t.Errorf("Forecast.UpstreamMirrors[0] = %q, want %q", cfg.Forecast.UpstreamMirrors[0], "noaa-gfs-bdp-pds")
	}
	if cfg.Forecast.UpstreamMirrors[1] != "aws-noaa-gfs" {
		t.Errorf("Forecast.UpstreamMirrors[1] = %q, want %q", cfg.Forecast.UpstreamMirrors[1], "aws-noaa-gfs")
	}
}

// TestLoadConfigAllEnvironments verifies that LoadConfig succeeds with each
// valid APP_ENV value (local, dev, staging, prod).
func TestLoadConfigAllEnvironments(t *testing.T) {
	validEnvs := []string{"local", "dev", "staging", "prod"}
	for _, env := range validEnvs {
		t.Run("APP_ENV="+env, func(t *testing.T) {
			setFullTestEnv(t)
			t.Setenv("APP_ENV", env)

			cfg, err := LoadConfig(nil)
			if err != nil {
				t.Fatalf("LoadConfig(APP_ENV=%s) returned error: %v", env, err)
			}
			if cfg.Environment != env {
				t.Errorf("Environment = %q, want %q", cfg.Environment, env)
			}
		})
	}
}

// TestLoadConfigWithDepsIsolated verifies the internal loadConfigWithDeps
// function using fully injected dependencies (Section 6.2: EnvProvider
// Interface and Mock Implementation to avoid global state mutation).
func TestLoadConfigWithDepsIsolated(t *testing.T) {
	envMap := map[string]string{
		"APP_ENV":              "local",
		"OTEL_SERVICE_NAME":   "deps-test-service",
		"LOG_LEVEL":           "warn",
		"API_EXTERNAL_URL":    "https://api.deps.local",
		"DASHBOARD_URL":       "https://app.deps.local",
		"DATABASE_URL":        "postgres://deps:pass@localhost:5432/depsdb",
		"FORECAST_BUCKET":     "deps-bucket",
		"SQS_EVAL_URGENT":     "https://sqs.us-east-1.amazonaws.com/123/urgent",
		"SQS_EVAL_STANDARD":   "https://sqs.us-east-1.amazonaws.com/123/standard",
		"SQS_NOTIFICATIONS":   "https://sqs.us-east-1.amazonaws.com/123/notif",
		"SQS_DLQ":             "https://sqs.us-east-1.amazonaws.com/123/dlq",
		"STRIPE_SECRET_KEY":   "sk_test_deps",
		"STRIPE_WEBHOOK_SECRET": "whsec_deps",
		"STRIPE_PUBLISHABLE_KEY": "pk_test_deps",
		"RUNPOD_API_KEY":      "rp_deps_key",
		"RUNPOD_ENDPOINT_ID":  "deps-endpoint",
		"SESSION_KEY":         "deps-session-key-that-is-at-least-32-characters-long-enough",
		"ADMIN_API_KEY":       "deps-admin-key",
	}

	deps := loaderDeps{
		lookupEnv: func(key string) (string, bool) {
			v, ok := envMap[key]
			return v, ok
		},
		setEnv: func(key, value string) error {
			envMap[key] = value
			return nil
		},
		environ: func() []string {
			result := make([]string, 0, len(envMap))
			for k, v := range envMap {
				result = append(result, k+"="+v)
			}
			return result
		},
	}

	// Note: loadConfigWithDeps still calls envconfig.Process which reads OS env,
	// so we also need real env vars set for envconfig. This test validates the
	// SSM resolution path with deps injection; for envconfig we set the env.
	for k, v := range envMap {
		t.Setenv(k, v)
	}

	cfg, err := loadConfigWithDeps(nil, deps)
	if err != nil {
		t.Fatalf("loadConfigWithDeps returned error: %v", err)
	}

	if cfg.Environment != "local" {
		t.Errorf("Environment = %q, want %q", cfg.Environment, "local")
	}
	if cfg.Service != "deps-test-service" {
		t.Errorf("Service = %q, want %q", cfg.Service, "deps-test-service")
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "warn")
	}
	if cfg.Database.URL.Unmask() != "postgres://deps:pass@localhost:5432/depsdb" {
		t.Errorf("Database.URL = %q, want deps value", cfg.Database.URL.Unmask())
	}
}

// TestLoadConfigWithDepsSSMResolution verifies that loadConfigWithDeps
// correctly resolves SSM parameters using injected dependencies (the map-based
// provider pattern from Section 6.2). The injected deps control how SSM
// resolution scans and sets environment variables, while envconfig.Process
// reads from the real OS environment. This test therefore uses deps.setEnv
// that writes to BOTH the map and the real environment.
func TestLoadConfigWithDepsSSMResolution(t *testing.T) {
	envMap := map[string]string{
		"APP_ENV":                              "staging",
		"OTEL_SERVICE_NAME":                   "staging-service",
		"LOG_LEVEL":                           "info",
		"API_EXTERNAL_URL":                    "https://api.staging.test",
		"DASHBOARD_URL":                       "https://app.staging.test",
		"FORECAST_BUCKET":                     "staging-bucket",
		"SQS_EVAL_URGENT":                     "https://sqs.us-east-1.amazonaws.com/123/urgent",
		"SQS_EVAL_STANDARD":                   "https://sqs.us-east-1.amazonaws.com/123/standard",
		"SQS_NOTIFICATIONS":                   "https://sqs.us-east-1.amazonaws.com/123/notif",
		"SQS_DLQ":                             "https://sqs.us-east-1.amazonaws.com/123/dlq",
		"STRIPE_PUBLISHABLE_KEY":              "pk_staging_123",
		"RUNPOD_ENDPOINT_ID":                  "staging-endpoint",
		"DATABASE_URL_SSM_PARAM":              "/staging/db/url",
		"STRIPE_SECRET_KEY_SSM_PARAM":         "/staging/billing/stripe_key",
		"STRIPE_WEBHOOK_SECRET_SSM_PARAM":     "/staging/billing/webhook_secret",
		"RUNPOD_API_KEY_SSM_PARAM":            "/staging/forecast/runpod",
		"SESSION_KEY_SSM_PARAM":               "/staging/auth/session",
		"ADMIN_API_KEY_SSM_PARAM":             "/staging/security/admin",
	}

	provider := &testSecretProvider{
		values: map[string]string{
			"/staging/db/url":                "postgres://staging:pass@rds/stagingdb",
			"/staging/billing/stripe_key":    "sk_staging_resolved",
			"/staging/billing/webhook_secret": "whsec_staging_resolved",
			"/staging/forecast/runpod":       "rp_staging_resolved",
			"/staging/auth/session":          "staging-session-key-at-least-32-characters-for-validation-ok",
			"/staging/security/admin":        "staging-admin-resolved",
		},
	}

	// Set real env vars for envconfig processing and SSM param pointers.
	for k, v := range envMap {
		t.Setenv(k, v)
	}

	// Save and restore any pre-existing target env vars that SSM resolution
	// will overwrite. This prevents leaking OS env state between tests.
	resolvedVars := []string{
		"DATABASE_URL", "STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET",
		"RUNPOD_API_KEY", "SESSION_KEY",
		"ADMIN_API_KEY",
	}
	savedDepsSSM := make(map[string]struct{ val string; ok bool })
	for _, v := range resolvedVars {
		val, ok := os.LookupEnv(v)
		savedDepsSSM[v] = struct{ val string; ok bool }{val, ok}
	}
	t.Cleanup(func() {
		for _, v := range resolvedVars {
			saved := savedDepsSSM[v]
			if saved.ok {
				os.Setenv(v, saved.val)
			} else {
				os.Unsetenv(v)
			}
		}
	})

	// The deps.setEnv writes to both the map (for injection tracking) and the
	// real environment (so envconfig.Process can read the resolved values).
	deps := loaderDeps{
		lookupEnv: func(key string) (string, bool) {
			v, ok := envMap[key]
			return v, ok
		},
		setEnv: func(key, value string) error {
			envMap[key] = value
			return os.Setenv(key, value)
		},
		environ: func() []string {
			result := make([]string, 0, len(envMap))
			for k, v := range envMap {
				result = append(result, k+"="+v)
			}
			return result
		},
	}

	cfg, err := loadConfigWithDeps(provider, deps)
	if err != nil {
		t.Fatalf("loadConfigWithDeps returned error: %v", err)
	}

	// Verify SSM resolution happened via the provider.
	if provider.callCount != 1 {
		t.Errorf("provider.callCount = %d, want 1", provider.callCount)
	}

	// Verify resolved values propagated to the config.
	if cfg.Database.URL.Unmask() != "postgres://staging:pass@rds/stagingdb" {
		t.Errorf("Database.URL = %q, want resolved SSM value", cfg.Database.URL.Unmask())
	}
	if cfg.Billing.StripeSecretKey.Unmask() != "sk_staging_resolved" {
		t.Errorf("Billing.StripeSecretKey = %q, want resolved SSM value", cfg.Billing.StripeSecretKey.Unmask())
	}
	if cfg.Environment != "staging" {
		t.Errorf("Environment = %q, want %q", cfg.Environment, "staging")
	}

	// Verify the injected envMap was updated with resolved values.
	if v, ok := envMap["DATABASE_URL"]; !ok || v != "postgres://staging:pass@rds/stagingdb" {
		t.Errorf("envMap[DATABASE_URL] = %q, want resolved value to be tracked in map", v)
	}
}

// TestLoadConfigLocalStackEndpoint verifies that the optional AWS_ENDPOINT_URL
// field is correctly populated for LocalStack support.
func TestLoadConfigLocalStackEndpoint(t *testing.T) {
	setFullTestEnv(t)
	t.Setenv("AWS_ENDPOINT_URL", "http://localhost:4566")

	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.AWS.EndpointURL != "http://localhost:4566" {
		t.Errorf("AWS.EndpointURL = %q, want %q", cfg.AWS.EndpointURL, "http://localhost:4566")
	}
}

// TestLoadConfigSSMResolutionAllSecrets verifies that ALL secret fields
// defined in the SSM parameter inventory (Section 3.2) are correctly resolved
// when using SSM pointers.
func TestLoadConfigSSMResolutionAllSecrets(t *testing.T) {
	// Set non-secret env vars directly.
	t.Setenv("APP_ENV", "prod")
	t.Setenv("OTEL_SERVICE_NAME", "prod-service")
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("API_EXTERNAL_URL", "https://api.watchpoint.io")
	t.Setenv("DASHBOARD_URL", "https://app.watchpoint.io")
	t.Setenv("FORECAST_BUCKET", "prod-forecast")
	t.Setenv("SQS_EVAL_URGENT", "https://sqs.us-east-1.amazonaws.com/999/urgent")
	t.Setenv("SQS_EVAL_STANDARD", "https://sqs.us-east-1.amazonaws.com/999/standard")
	t.Setenv("SQS_NOTIFICATIONS", "https://sqs.us-east-1.amazonaws.com/999/notif")
	t.Setenv("SQS_DLQ", "https://sqs.us-east-1.amazonaws.com/999/dlq")
	t.Setenv("STRIPE_PUBLISHABLE_KEY", "pk_live_abc")
	t.Setenv("RUNPOD_ENDPOINT_ID", "prod-endpoint")
	t.Setenv("GOOGLE_CLIENT_ID", "google-client-id-prod")
	t.Setenv("GITHUB_CLIENT_ID", "github-client-id-prod")

	// Set ALL SSM param pointers (Section 3.2 inventory).
	t.Setenv("DATABASE_URL_SSM_PARAM", "/prod/watchpoint/database/url")
	t.Setenv("STRIPE_SECRET_KEY_SSM_PARAM", "/prod/watchpoint/billing/stripe_secret_key")
	t.Setenv("STRIPE_WEBHOOK_SECRET_SSM_PARAM", "/prod/watchpoint/billing/stripe_webhook_secret")
	t.Setenv("RUNPOD_API_KEY_SSM_PARAM", "/prod/watchpoint/forecast/runpod_api_key")
	t.Setenv("SESSION_KEY_SSM_PARAM", "/prod/watchpoint/auth/session_key")
	t.Setenv("ADMIN_API_KEY_SSM_PARAM", "/prod/watchpoint/security/admin_api_key")
	t.Setenv("GOOGLE_CLIENT_SECRET_SSM_PARAM", "/prod/watchpoint/auth/google_secret")
	t.Setenv("GITHUB_CLIENT_SECRET_SSM_PARAM", "/prod/watchpoint/auth/github_secret")

	// Ensure target env vars are NOT already present in the OS environment
	// (e.g., from the shell profile). Save and restore pre-existing values.
	resolvedVars := []string{
		"DATABASE_URL", "STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET",
		"RUNPOD_API_KEY", "SESSION_KEY",
		"ADMIN_API_KEY", "GOOGLE_CLIENT_SECRET", "GITHUB_CLIENT_SECRET",
	}
	savedAllSecrets := make(map[string]struct{ val string; ok bool })
	for _, v := range resolvedVars {
		val, ok := os.LookupEnv(v)
		savedAllSecrets[v] = struct{ val string; ok bool }{val, ok}
		os.Unsetenv(v)
	}
	t.Cleanup(func() {
		for _, v := range resolvedVars {
			saved := savedAllSecrets[v]
			if saved.ok {
				os.Setenv(v, saved.val)
			} else {
				os.Unsetenv(v)
			}
		}
	})

	provider := &testSecretProvider{
		values: map[string]string{
			"/prod/watchpoint/database/url":                "postgres://prod:secret@prod-rds/proddb",
			"/prod/watchpoint/billing/stripe_secret_key":   "sk_live_prod_key",
			"/prod/watchpoint/billing/stripe_webhook_secret": "whsec_live_prod_secret",
			"/prod/watchpoint/forecast/runpod_api_key":     "rp_prod_api_key",
			"/prod/watchpoint/auth/session_key":            "prod-session-key-that-is-definitely-at-least-32-characters-for-validation",
			"/prod/watchpoint/security/admin_api_key":      "prod-admin-api-key-value",
			"/prod/watchpoint/auth/google_secret":          "prod-google-client-secret",
			"/prod/watchpoint/auth/github_secret":          "prod-github-client-secret",
		},
	}

	cfg, err := LoadConfig(provider)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	// Verify every SSM-resolved secret field.
	secrets := map[string]struct {
		got  string
		want string
	}{
		"Database.URL":               {cfg.Database.URL.Unmask(), "postgres://prod:secret@prod-rds/proddb"},
		"Billing.StripeSecretKey":    {cfg.Billing.StripeSecretKey.Unmask(), "sk_live_prod_key"},
		"Billing.StripeWebhookSecret": {cfg.Billing.StripeWebhookSecret.Unmask(), "whsec_live_prod_secret"},
		"Forecast.RunPodAPIKey":      {cfg.Forecast.RunPodAPIKey.Unmask(), "rp_prod_api_key"},
		"Auth.SessionKey":            {cfg.Auth.SessionKey.Unmask(), "prod-session-key-that-is-definitely-at-least-32-characters-for-validation"},
		"Security.AdminAPIKey":       {cfg.Security.AdminAPIKey.Unmask(), "prod-admin-api-key-value"},
		"Auth.GoogleClientSecret":    {cfg.Auth.GoogleClientSecret.Unmask(), "prod-google-client-secret"},
		"Auth.GithubClientSecret":    {cfg.Auth.GithubClientSecret.Unmask(), "prod-github-client-secret"},
	}

	for name, check := range secrets {
		if check.got != check.want {
			t.Errorf("%s = %q, want %q", name, check.got, check.want)
		}
	}

	// Provider should be called exactly once for batched retrieval.
	if provider.callCount != 1 {
		t.Errorf("provider.callCount = %d, want 1", provider.callCount)
	}
	// All 8 SSM params should have been requested.
	if len(provider.calledWith) != 8 {
		t.Errorf("provider called with %d keys, want 8", len(provider.calledWith))
	}
}

// TestLoadConfigMissingAppEnv verifies that an empty/missing APP_ENV returns
// a validation error (required,oneof constraint).
func TestLoadConfigMissingAppEnv(t *testing.T) {
	// Do not set APP_ENV at all, set everything else.
	setFullTestEnv(t)
	// Override APP_ENV to empty string to simulate missing.
	t.Setenv("APP_ENV", "")

	_, err := LoadConfig(nil)
	if err == nil {
		t.Fatal("expected error for empty APP_ENV, got nil")
	}

	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *ConfigError, got %T: %v", err, err)
	}
}

// TestLoadConfigSessionKeyTooShort verifies that a session key shorter than
// 32 characters fails validation (validate:"required,min=32").
func TestLoadConfigSessionKeyTooShort(t *testing.T) {
	setFullTestEnv(t)
	t.Setenv("SESSION_KEY", "short-key")

	_, err := LoadConfig(nil)
	if err == nil {
		t.Fatal("expected error for short session key, got nil")
	}

	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *ConfigError, got %T: %v", err, err)
	}
	if cfgErr.Type != ErrValidation {
		t.Errorf("expected ErrValidation, got %q", cfgErr.Type)
	}
}

// TestLoadConfigInvalidURL verifies that an invalid URL in a url-validated
// field fails validation.
func TestLoadConfigInvalidURL(t *testing.T) {
	setFullTestEnv(t)
	t.Setenv("API_EXTERNAL_URL", "not-a-valid-url")

	_, err := LoadConfig(nil)
	if err == nil {
		t.Fatal("expected error for invalid URL, got nil")
	}

	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *ConfigError, got %T: %v", err, err)
	}
	if cfgErr.Type != ErrValidation {
		t.Errorf("expected ErrValidation, got %q", cfgErr.Type)
	}
}

