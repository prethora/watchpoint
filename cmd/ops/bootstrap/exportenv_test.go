package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newMockSSMWithValues creates a mock SSM client that returns the given
// values for GetParameter calls. Values are keyed by full SSM path.
func newMockSSMWithValues(values map[string]string) *mockSSMClient {
	return &mockSSMClient{
		getParameterFn: func(_ context.Context, input *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			path := aws.ToString(input.Name)
			val, ok := values[path]
			if !ok {
				return nil, &ssmtypes.ParameterNotFound{Message: aws.String("not found: " + path)}
			}
			return &ssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{
					Name:  aws.String(path),
					Value: aws.String(val),
				},
			}, nil
		},
	}
}

// newTestExportConfig creates an ExportEnvConfig for testing with a temp
// directory for the output file.
func newTestExportConfig(t *testing.T, mock *mockSSMClient, env string, includeDefaults bool) (ExportEnvConfig, string) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ssmMgr := NewSSMManagerWithClient(mock, env, logger)

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, ".env")
	stderr := &bytes.Buffer{}

	return ExportEnvConfig{
		OutputPath:           outputPath,
		Environment:          env,
		SSM:                  ssmMgr,
		Stderr:               stderr,
		IncludeLocalDefaults: includeDefaults,
	}, outputPath
}

// allSSMValues returns a complete set of SSM parameter values for the
// dev environment, one for each bootstrap inventory step.
func allSSMValues() map[string]string {
	return map[string]string{
		"/dev/watchpoint/database/url":                   "postgres://user:pass@host:6543/db",
		"/dev/watchpoint/billing/stripe_secret_key":      "sk_test_abc123def456ghi789jkl012",
		"/dev/watchpoint/billing/stripe_publishable_key": "pk_test_abc123def456ghi789jkl012",
		"/dev/watchpoint/email/sendgrid_api_key":         "SG.test_sendgrid_key_value_here",
		"/dev/watchpoint/forecast/runpod_api_key":        "rp_test_runpod_key_value_here_long",
		"/dev/watchpoint/forecast/runpod_endpoint_id":    "vllm-test-endpoint-123",
		"/dev/watchpoint/auth/google_client_id":          "1234567890.apps.googleusercontent.com",
		"/dev/watchpoint/auth/google_secret":             "GOCSPX-test-google-secret-value",
		"/dev/watchpoint/auth/github_client_id":          "gh-test-client-id-value",
		"/dev/watchpoint/auth/github_secret":             "ghp_test_github_secret_value_here",
		"/dev/watchpoint/auth/session_key":               "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"/dev/watchpoint/security/admin_api_key":         "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
	}
}

// ---------------------------------------------------------------------------
// ssmToEnvMapping tests
// ---------------------------------------------------------------------------

func TestSSMToEnvMapping_CoversAllInventorySteps(t *testing.T) {
	v := NewValidatorWithDeps(nil, nil)
	inventory := BuildInventory(v)

	for _, step := range inventory {
		if _, ok := ssmToEnvMapping[step.SSMCategoryKey]; !ok {
			t.Errorf("SSM key %q (label: %s) has no entry in ssmToEnvMapping",
				step.SSMCategoryKey, step.HumanLabel)
		}
	}
}

func TestSSMToEnvMapping_NoEmptyValues(t *testing.T) {
	for ssmKey, envVar := range ssmToEnvMapping {
		if envVar == "" {
			t.Errorf("ssmToEnvMapping[%q] has empty env var name", ssmKey)
		}
	}
}

func TestSSMToEnvMapping_NoDuplicateEnvVars(t *testing.T) {
	seen := make(map[string]string)
	for ssmKey, envVar := range ssmToEnvMapping {
		if prevKey, ok := seen[envVar]; ok {
			t.Errorf("env var %q is mapped by both %q and %q", envVar, prevKey, ssmKey)
		}
		seen[envVar] = ssmKey
	}
}

func TestSSMToEnvMapping_MatchesConfigEnvTags(t *testing.T) {
	// Verify the mapping matches the envconfig tags from 03-config.md.
	expectedMappings := map[string]string{
		"database/url":                   "DATABASE_URL",
		"billing/stripe_secret_key":      "STRIPE_SECRET_KEY",
		"billing/stripe_publishable_key": "STRIPE_PUBLISHABLE_KEY",
		"email/sendgrid_api_key":         "SENDGRID_API_KEY",
		"forecast/runpod_api_key":        "RUNPOD_API_KEY",
		"forecast/runpod_endpoint_id":    "RUNPOD_ENDPOINT_ID",
		"auth/google_client_id":          "GOOGLE_CLIENT_ID",
		"auth/google_secret":             "GOOGLE_CLIENT_SECRET",
		"auth/github_client_id":          "GITHUB_CLIENT_ID",
		"auth/github_secret":             "GITHUB_CLIENT_SECRET",
		"auth/session_key":               "SESSION_KEY",
		"security/admin_api_key":         "ADMIN_API_KEY",
	}

	for ssmKey, expectedVar := range expectedMappings {
		gotVar, ok := ssmToEnvMapping[ssmKey]
		if !ok {
			t.Errorf("ssmToEnvMapping missing key %q", ssmKey)
			continue
		}
		if gotVar != expectedVar {
			t.Errorf("ssmToEnvMapping[%q] = %q, want %q", ssmKey, gotVar, expectedVar)
		}
	}
}

// ---------------------------------------------------------------------------
// formatEnvLine tests
// ---------------------------------------------------------------------------

func TestFormatEnvLine_SimpleValue(t *testing.T) {
	got := formatEnvLine("KEY", "value")
	if got != "KEY=value" {
		t.Errorf("formatEnvLine = %q, want %q", got, "KEY=value")
	}
}

func TestFormatEnvLine_ValueWithSpaces(t *testing.T) {
	got := formatEnvLine("KEY", "hello world")
	if got != `KEY="hello world"` {
		t.Errorf("formatEnvLine = %q, want %q", got, `KEY="hello world"`)
	}
}

func TestFormatEnvLine_ValueWithDoubleQuotes(t *testing.T) {
	got := formatEnvLine("KEY", `say "hello"`)
	if got != `KEY="say \"hello\""` {
		t.Errorf("formatEnvLine = %q, want %q", got, `KEY="say \"hello\""`)
	}
}

func TestFormatEnvLine_ValueWithHash(t *testing.T) {
	got := formatEnvLine("KEY", "value#comment")
	if got != `KEY="value#comment"` {
		t.Errorf("formatEnvLine = %q, want %q", got, `KEY="value#comment"`)
	}
}

func TestFormatEnvLine_EmptyValue(t *testing.T) {
	got := formatEnvLine("KEY", "")
	if got != `KEY=""` {
		t.Errorf("formatEnvLine = %q, want %q", got, `KEY=""`)
	}
}

func TestFormatEnvLine_URLValue(t *testing.T) {
	got := formatEnvLine("DATABASE_URL", "postgres://user:pass@host:6543/db")
	// URLs don't contain characters that require quoting (no spaces, etc.)
	if got != "DATABASE_URL=postgres://user:pass@host:6543/db" {
		t.Errorf("formatEnvLine = %q, want simple assignment", got)
	}
}

func TestFormatEnvLine_JSONValue(t *testing.T) {
	json := `{"default":{"threshold_crossed":"d-123"}}`
	got := formatEnvLine("EMAIL_TEMPLATES_JSON", json)
	// JSON contains curly braces which require quoting.
	if !strings.HasPrefix(got, `EMAIL_TEMPLATES_JSON="`) {
		t.Errorf("formatEnvLine should quote JSON value, got %q", got)
	}
}

func TestFormatEnvLine_ValueWithNewline(t *testing.T) {
	got := formatEnvLine("KEY", "line1\nline2")
	expected := `KEY="line1\nline2"`
	if got != expected {
		t.Errorf("formatEnvLine = %q, want %q", got, expected)
	}
}

func TestFormatEnvLine_ValueWithBackslash(t *testing.T) {
	got := formatEnvLine("KEY", `path\to\file`)
	expected := `KEY="path\\to\\file"`
	if got != expected {
		t.Errorf("formatEnvLine = %q, want %q", got, expected)
	}
}

func TestFormatEnvLine_ValueWithDollarSign(t *testing.T) {
	got := formatEnvLine("KEY", "price$100")
	// Dollar sign requires quoting.
	if !strings.HasPrefix(got, `KEY="`) {
		t.Errorf("formatEnvLine should quote dollar sign value, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// ExportEnvFile tests
// ---------------------------------------------------------------------------

func TestExportEnvFile_AllParameters(t *testing.T) {
	values := allSSMValues()
	mock := newMockSSMWithValues(values)

	cfg, outputPath := newTestExportConfig(t, mock, "dev", false)

	err := ExportEnvFile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Read the generated file.
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	text := string(content)

	// Verify all expected env vars are present.
	for _, envVar := range ssmToEnvMapping {
		if !strings.Contains(text, envVar+"=") {
			t.Errorf("output missing env var %s", envVar)
		}
	}

	// Verify specific values.
	if !strings.Contains(text, "DATABASE_URL=postgres://user:pass@host:6543/db") {
		t.Error("output missing correct DATABASE_URL value")
	}
	if !strings.Contains(text, "STRIPE_SECRET_KEY=sk_test_abc123def456ghi789jkl012") {
		t.Error("output missing correct STRIPE_SECRET_KEY value")
	}
	if !strings.Contains(text, "SESSION_KEY=0123456789abcdef") {
		t.Error("output missing correct SESSION_KEY value")
	}
}

func TestExportEnvFile_ContainsHeader(t *testing.T) {
	values := allSSMValues()
	mock := newMockSSMWithValues(values)

	cfg, outputPath := newTestExportConfig(t, mock, "dev", false)

	err := ExportEnvFile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	text := string(content)

	if !strings.Contains(text, "Auto-generated by bootstrap --export-env") {
		t.Error("output missing header comment")
	}
	if !strings.Contains(text, "Environment: dev") {
		t.Error("output missing environment in header")
	}
	if !strings.Contains(text, "SECURITY WARNING") {
		t.Error("output missing security warning")
	}
}

func TestExportEnvFile_WithLocalDefaults(t *testing.T) {
	values := allSSMValues()
	mock := newMockSSMWithValues(values)

	cfg, outputPath := newTestExportConfig(t, mock, "dev", true)

	err := ExportEnvFile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	text := string(content)

	// Verify local defaults are included.
	if !strings.Contains(text, "APP_ENV=local") {
		t.Error("output missing APP_ENV=local")
	}
	if !strings.Contains(text, "LOG_LEVEL=debug") {
		t.Error("output missing LOG_LEVEL=debug")
	}
	if !strings.Contains(text, "AWS_ENDPOINT_URL=http://localhost:9000") {
		t.Error("output missing AWS_ENDPOINT_URL for MinIO")
	}
	if !strings.Contains(text, "SQS_EVAL_URGENT=http://localhost:4566/000000000000/eval-queue-urgent") {
		t.Error("output missing SQS_EVAL_URGENT for LocalStack")
	}

	// Verify that SSM-sourced vars are NOT duplicated in the defaults section.
	// Count occurrences of DATABASE_URL= (should appear exactly once).
	count := strings.Count(text, "DATABASE_URL=")
	if count != 1 {
		t.Errorf("DATABASE_URL= appears %d times, want exactly 1", count)
	}
}

func TestExportEnvFile_WithoutLocalDefaults(t *testing.T) {
	values := allSSMValues()
	mock := newMockSSMWithValues(values)

	cfg, outputPath := newTestExportConfig(t, mock, "dev", false)

	err := ExportEnvFile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	text := string(content)

	// Local defaults should NOT be present.
	if strings.Contains(text, "APP_ENV=") {
		t.Error("output should not contain APP_ENV when IncludeLocalDefaults=false")
	}
	if strings.Contains(text, "Local Development Defaults") {
		t.Error("output should not contain defaults section header")
	}
}

func TestExportEnvFile_FilePermissions(t *testing.T) {
	values := allSSMValues()
	mock := newMockSSMWithValues(values)

	cfg, outputPath := newTestExportConfig(t, mock, "dev", false)

	err := ExportEnvFile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("failed to stat output file: %v", err)
	}

	// File should have 0600 permissions (owner read/write only).
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("file permissions = %o, want 0600", perm)
	}
}

func TestExportEnvFile_PartialSSMFailure(t *testing.T) {
	// Only provide a subset of values -- some will fail.
	values := map[string]string{
		"/dev/watchpoint/database/url":                   "postgres://user:pass@host:6543/db",
		"/dev/watchpoint/billing/stripe_secret_key":      "sk_test_abc123def456ghi789jkl012",
		"/dev/watchpoint/billing/stripe_publishable_key": "pk_test_abc123def456ghi789jkl012",
		"/dev/watchpoint/auth/session_key":               "session-key-value-long-enough-xxxxx",
		"/dev/watchpoint/security/admin_api_key":         "admin-key-value-long-enough-xxxxx",
	}
	mock := newMockSSMWithValues(values)

	cfg, outputPath := newTestExportConfig(t, mock, "dev", false)

	err := ExportEnvFile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	text := string(content)

	// Present values should be in the file.
	if !strings.Contains(text, "DATABASE_URL=") {
		t.Error("output missing DATABASE_URL")
	}
	if !strings.Contains(text, "STRIPE_SECRET_KEY=") {
		t.Error("output missing STRIPE_SECRET_KEY")
	}

	// Missing values should not be in the file.
	if strings.Contains(text, "SENDGRID_API_KEY=") {
		t.Error("output should not contain SENDGRID_API_KEY (not in SSM)")
	}
	if strings.Contains(text, "RUNPOD_API_KEY=") {
		t.Error("output should not contain RUNPOD_API_KEY (not in SSM)")
	}
}

func TestExportEnvFile_AllParametersMissing(t *testing.T) {
	// Empty SSM -- no parameters found.
	mock := newMockSSMWithValues(map[string]string{})

	cfg, _ := newTestExportConfig(t, mock, "dev", false)

	err := ExportEnvFile(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when no parameters could be read")
	}
	if !strings.Contains(err.Error(), "no parameters could be read") {
		t.Errorf("error = %q, want to contain 'no parameters could be read'", err.Error())
	}
}

func TestExportEnvFile_StagingEnvironment(t *testing.T) {
	// Use staging paths.
	values := map[string]string{
		"/staging/watchpoint/database/url":                   "postgres://user:pass@staging-host:6543/db",
		"/staging/watchpoint/billing/stripe_secret_key":      "sk_test_staging_key",
		"/staging/watchpoint/billing/stripe_publishable_key": "pk_test_staging_key",
		"/staging/watchpoint/email/sendgrid_api_key":         "SG.staging_key",
		"/staging/watchpoint/forecast/runpod_api_key":        "rp_staging_key_long_enough",
		"/staging/watchpoint/forecast/runpod_endpoint_id":    "vllm-staging-endpoint",
		"/staging/watchpoint/auth/google_client_id":          "staging.apps.googleusercontent.com",
		"/staging/watchpoint/auth/google_secret":             "GOCSPX-staging-secret",
		"/staging/watchpoint/auth/github_client_id":          "gh-staging-client-id",
		"/staging/watchpoint/auth/github_secret":             "ghp_staging_secret",
		"/staging/watchpoint/auth/session_key":               "staging-session-key-aaaaaaaaaaaaa",
		"/staging/watchpoint/security/admin_api_key":         "staging-admin-key-bbbbbbbbbbbbb",
	}
	mock := newMockSSMWithValues(values)

	cfg, outputPath := newTestExportConfig(t, mock, "staging", false)

	err := ExportEnvFile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	text := string(content)

	if !strings.Contains(text, "Environment: staging") {
		t.Error("output header should reference staging environment")
	}
	if !strings.Contains(text, "DATABASE_URL=postgres://user:pass@staging-host:6543/db") {
		t.Error("output missing correct staging DATABASE_URL")
	}
}

func TestExportEnvFile_CustomOutputPath(t *testing.T) {
	values := allSSMValues()
	mock := newMockSSMWithValues(values)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ssmMgr := NewSSMManagerWithClient(mock, "dev", logger)

	tmpDir := t.TempDir()
	customPath := filepath.Join(tmpDir, "subdir", "custom.env")

	cfg := ExportEnvConfig{
		OutputPath:           customPath,
		Environment:          "dev",
		SSM:                  ssmMgr,
		Stderr:               &bytes.Buffer{},
		IncludeLocalDefaults: false,
	}

	err := ExportEnvFile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the file was created at the custom path.
	if _, err := os.Stat(customPath); os.IsNotExist(err) {
		t.Errorf("file was not created at custom path %s", customPath)
	}
}

func TestExportEnvFile_ContextCancelled(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(ctx context.Context, _ *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return nil, ctx.Err()
		},
	}

	cfg, _ := newTestExportConfig(t, mock, "dev", false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ExportEnvFile(ctx, cfg)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestExportEnvFile_StderrOutput(t *testing.T) {
	values := allSSMValues()
	mock := newMockSSMWithValues(values)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ssmMgr := NewSSMManagerWithClient(mock, "dev", logger)

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, ".env")
	stderr := &bytes.Buffer{}

	cfg := ExportEnvConfig{
		OutputPath:           outputPath,
		Environment:          "dev",
		SSM:                  ssmMgr,
		Stderr:               stderr,
		IncludeLocalDefaults: false,
	}

	err := ExportEnvFile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := stderr.String()
	if !strings.Contains(output, "Environment file exported") {
		t.Error("stderr missing export confirmation message")
	}
	if !strings.Contains(output, "Parameters written: 12") {
		t.Errorf("stderr missing parameter count, got:\n%s", output)
	}
	if !strings.Contains(output, "0600") {
		t.Error("stderr missing file permission info")
	}
}

// ---------------------------------------------------------------------------
// GetParameterValue tests
// ---------------------------------------------------------------------------

func TestGetParameterValue_Success(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(_ context.Context, input *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return &ssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{
					Name:  input.Name,
					Value: aws.String("my-secret-value"),
				},
			}, nil
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	value, err := mgr.GetParameterValue(context.Background(), "/dev/watchpoint/database/url", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if value != "my-secret-value" {
		t.Errorf("value = %q, want %q", value, "my-secret-value")
	}

	// Verify WithDecryption was set correctly.
	if len(mock.getCalls) != 1 {
		t.Fatalf("expected 1 GetParameter call, got %d", len(mock.getCalls))
	}
	if !aws.ToBool(mock.getCalls[0].WithDecryption) {
		t.Error("expected WithDecryption=true for secret parameter")
	}
}

func TestGetParameterValue_NoDecrypt(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(_ context.Context, input *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return &ssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{
					Name:  input.Name,
					Value: aws.String("plain-value"),
				},
			}, nil
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	value, err := mgr.GetParameterValue(context.Background(), "/dev/watchpoint/forecast/runpod_endpoint_id", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if value != "plain-value" {
		t.Errorf("value = %q, want %q", value, "plain-value")
	}

	if aws.ToBool(mock.getCalls[0].WithDecryption) {
		t.Error("expected WithDecryption=false for non-secret parameter")
	}
}

func TestGetParameterValue_NotFound(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(_ context.Context, _ *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return nil, &ssmtypes.ParameterNotFound{Message: aws.String("not found")}
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	_, err := mgr.GetParameterValue(context.Background(), "/dev/watchpoint/database/url", true)
	if err == nil {
		t.Fatal("expected error for missing parameter")
	}
	if !strings.Contains(err.Error(), "reading SSM parameter") {
		t.Errorf("error = %q, want to contain 'reading SSM parameter'", err.Error())
	}
}

func TestGetParameterValue_NilValue(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(_ context.Context, input *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return &ssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{
					Name:  input.Name,
					Value: nil,
				},
			}, nil
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	_, err := mgr.GetParameterValue(context.Background(), "/dev/watchpoint/database/url", true)
	if err == nil {
		t.Fatal("expected error for nil value")
	}
	if !strings.Contains(err.Error(), "has no value") {
		t.Errorf("error = %q, want to contain 'has no value'", err.Error())
	}
}

func TestGetParameterValue_APIError(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(_ context.Context, _ *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return nil, fmt.Errorf("access denied")
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	_, err := mgr.GetParameterValue(context.Background(), "/dev/watchpoint/database/url", true)
	if err == nil {
		t.Fatal("expected error for API failure")
	}
}

func TestGetParameterValue_ContextCancelled(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(ctx context.Context, _ *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return nil, ctx.Err()
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := mgr.GetParameterValue(ctx, "/dev/watchpoint/database/url", true)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// ---------------------------------------------------------------------------
// Local defaults tests
// ---------------------------------------------------------------------------

func TestLocalDevDefaults_CoverRequiredNonSSMVars(t *testing.T) {
	// These are the env vars required by config that are NOT sourced from SSM.
	requiredNonSSMVars := []string{
		"APP_ENV",
		"API_EXTERNAL_URL",
		"DASHBOARD_URL",
		"AWS_REGION",
		"FORECAST_BUCKET",
		"SQS_EVAL_URGENT",
		"SQS_EVAL_STANDARD",
		"SQS_NOTIFICATIONS",
		"SQS_DLQ",
	}

	for _, envVar := range requiredNonSSMVars {
		if _, ok := localDevDefaults[envVar]; !ok {
			t.Errorf("localDevDefaults missing required var %q", envVar)
		}
	}
}

func TestLocalDevDefaults_NoOverlapWithSSMMapping(t *testing.T) {
	// Local defaults should not include vars that come from SSM.
	for key := range localDevDefaults {
		for _, envVar := range ssmToEnvMapping {
			if key == envVar {
				t.Errorf("localDevDefaults contains %q which is also in ssmToEnvMapping (would be duplicated)", key)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Edge case: Stripe webhook secret
// ---------------------------------------------------------------------------

func TestExportEnvFile_StripeWebhookSecretNotInBootstrap(t *testing.T) {
	// STRIPE_WEBHOOK_SECRET is in the config but NOT in the bootstrap inventory.
	// This is expected -- it's generated by Stripe after webhook registration
	// in Phase 5 (Post-Deployment Wiring). The .env.example provides a dummy.
	// Verify the SSM mapping doesn't include it.
	for ssmKey := range ssmToEnvMapping {
		if strings.Contains(ssmKey, "webhook_secret") {
			t.Errorf("ssmToEnvMapping should not include webhook_secret (not in bootstrap): %s", ssmKey)
		}
	}

	// But verify it IS in localDevDefaults so the .env file is complete.
	if _, ok := localDevDefaults["STRIPE_WEBHOOK_SECRET"]; !ok {
		t.Error("localDevDefaults should include STRIPE_WEBHOOK_SECRET for local dev completeness")
	}
}

func TestExportEnvFile_WithLocalDefaults_IncludesStripeWebhookSecret(t *testing.T) {
	values := allSSMValues()
	mock := newMockSSMWithValues(values)

	cfg, outputPath := newTestExportConfig(t, mock, "dev", true)

	err := ExportEnvFile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}

	text := string(content)
	if !strings.Contains(text, "STRIPE_WEBHOOK_SECRET=") {
		t.Error("output missing STRIPE_WEBHOOK_SECRET (required by config but not in SSM bootstrap)")
	}
}
