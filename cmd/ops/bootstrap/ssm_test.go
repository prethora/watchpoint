package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// mockSSMClient implements SSMClient for testing. It records calls and
// returns configurable responses/errors.
type mockSSMClient struct {
	// getParameterFn, if set, is called for GetParameter requests.
	getParameterFn func(ctx context.Context, input *ssm.GetParameterInput) (*ssm.GetParameterOutput, error)

	// putParameterFn, if set, is called for PutParameter requests.
	putParameterFn func(ctx context.Context, input *ssm.PutParameterInput) (*ssm.PutParameterOutput, error)

	// getCalls records all GetParameter invocations for assertion.
	getCalls []*ssm.GetParameterInput

	// putCalls records all PutParameter invocations for assertion.
	putCalls []*ssm.PutParameterInput
}

func (m *mockSSMClient) GetParameter(ctx context.Context, params *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	m.getCalls = append(m.getCalls, params)
	if m.getParameterFn != nil {
		return m.getParameterFn(ctx, params)
	}
	return &ssm.GetParameterOutput{}, nil
}

func (m *mockSSMClient) PutParameter(ctx context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	m.putCalls = append(m.putCalls, params)
	if m.putParameterFn != nil {
		return m.putParameterFn(ctx, params)
	}
	return &ssm.PutParameterOutput{
		Version: 1,
	}, nil
}

// newTestSSMManager creates an SSMManager with a mock client for testing.
func newTestSSMManager(mock *mockSSMClient, env string) *SSMManager {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	return NewSSMManagerWithClient(mock, env, logger)
}

// ---------------------------------------------------------------------------
// SSMPath tests
// ---------------------------------------------------------------------------

func TestSSMPath(t *testing.T) {
	tests := []struct {
		name           string
		env            string
		categoryAndKey string
		expected       string
	}{
		{
			name:           "dev database URL",
			env:            "dev",
			categoryAndKey: "database/url",
			expected:       "/dev/watchpoint/database/url",
		},
		{
			name:           "prod billing stripe secret key",
			env:            "prod",
			categoryAndKey: "billing/stripe_secret_key",
			expected:       "/prod/watchpoint/billing/stripe_secret_key",
		},
		{
			name:           "staging auth session key",
			env:            "staging",
			categoryAndKey: "auth/session_key",
			expected:       "/staging/watchpoint/auth/session_key",
		},
		{
			name:           "dev security admin api key",
			env:            "dev",
			categoryAndKey: "security/admin_api_key",
			expected:       "/dev/watchpoint/security/admin_api_key",
		},
		{
			name:           "prod forecast runpod api key",
			env:            "prod",
			categoryAndKey: "forecast/runpod_api_key",
			expected:       "/prod/watchpoint/forecast/runpod_api_key",
		},
		{
			name:           "dev forecast runpod endpoint id",
			env:            "dev",
			categoryAndKey: "forecast/runpod_endpoint_id",
			expected:       "/dev/watchpoint/forecast/runpod_endpoint_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockSSMClient{}
			mgr := newTestSSMManager(mock, tt.env)

			got := mgr.SSMPath(tt.categoryAndKey)
			if got != tt.expected {
				t.Errorf("SSMPath(%q) = %q, want %q", tt.categoryAndKey, got, tt.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ParameterExists tests
// ---------------------------------------------------------------------------

func TestParameterExists_Found(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(_ context.Context, input *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return &ssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{
					Name:  input.Name,
					Value: aws.String("some-value"),
				},
			}, nil
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	exists, err := mgr.ParameterExists(context.Background(), "/dev/watchpoint/database/url")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected parameter to exist, got false")
	}

	// Verify the call was made with WithDecryption=false
	if len(mock.getCalls) != 1 {
		t.Fatalf("expected 1 GetParameter call, got %d", len(mock.getCalls))
	}
	if aws.ToBool(mock.getCalls[0].WithDecryption) {
		t.Error("expected WithDecryption=false for existence check")
	}
}

func TestParameterExists_NotFound(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(_ context.Context, _ *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return nil, &ssmtypes.ParameterNotFound{}
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	exists, err := mgr.ParameterExists(context.Background(), "/dev/watchpoint/database/url")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected parameter to not exist, got true")
	}
}

func TestParameterExists_Error(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(_ context.Context, _ *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return nil, fmt.Errorf("access denied")
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	_, err := mgr.ParameterExists(context.Background(), "/dev/watchpoint/database/url")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Verify the error message includes the path.
	expected := `checking SSM parameter "/dev/watchpoint/database/url"`
	if got := err.Error(); got[:len(expected)] != expected {
		t.Errorf("error message = %q, want prefix %q", got, expected)
	}
}

// ---------------------------------------------------------------------------
// PutSecret tests
// ---------------------------------------------------------------------------

func TestPutSecret_Success(t *testing.T) {
	mock := &mockSSMClient{}
	mgr := newTestSSMManager(mock, "dev")

	err := mgr.PutSecret(context.Background(), "/dev/watchpoint/database/url", "postgres://user:pass@host:6543/db", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the PutParameter call.
	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 PutParameter call, got %d", len(mock.putCalls))
	}

	call := mock.putCalls[0]
	if aws.ToString(call.Name) != "/dev/watchpoint/database/url" {
		t.Errorf("Name = %q, want %q", aws.ToString(call.Name), "/dev/watchpoint/database/url")
	}
	if aws.ToString(call.Value) != "postgres://user:pass@host:6543/db" {
		t.Errorf("Value = %q, want the database URL", aws.ToString(call.Value))
	}
	if call.Type != ssmtypes.ParameterTypeSecureString {
		t.Errorf("Type = %v, want SecureString", call.Type)
	}
	if aws.ToBool(call.Overwrite) {
		t.Error("Overwrite should be false")
	}
}

func TestPutSecret_WithOverwrite(t *testing.T) {
	mock := &mockSSMClient{}
	mgr := newTestSSMManager(mock, "prod")

	err := mgr.PutSecret(context.Background(), "/prod/watchpoint/billing/stripe_secret_key", "sk_live_abc123", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 PutParameter call, got %d", len(mock.putCalls))
	}

	call := mock.putCalls[0]
	if !aws.ToBool(call.Overwrite) {
		t.Error("Overwrite should be true")
	}
	if call.Type != ssmtypes.ParameterTypeSecureString {
		t.Errorf("Type = %v, want SecureString", call.Type)
	}
}

func TestPutSecret_AlreadyExists(t *testing.T) {
	mock := &mockSSMClient{
		putParameterFn: func(_ context.Context, _ *ssm.PutParameterInput) (*ssm.PutParameterOutput, error) {
			return nil, &ssmtypes.ParameterAlreadyExists{}
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	err := mgr.PutSecret(context.Background(), "/dev/watchpoint/database/url", "postgres://...", false)
	if err == nil {
		t.Fatal("expected error for already existing parameter, got nil")
	}

	expected := `SSM parameter "/dev/watchpoint/database/url" already exists`
	if got := err.Error(); got[:len(expected)] != expected {
		t.Errorf("error message = %q, want prefix %q", got, expected)
	}
}

func TestPutSecret_APIError(t *testing.T) {
	mock := &mockSSMClient{
		putParameterFn: func(_ context.Context, _ *ssm.PutParameterInput) (*ssm.PutParameterOutput, error) {
			return nil, fmt.Errorf("throttling exception")
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	err := mgr.PutSecret(context.Background(), "/dev/watchpoint/auth/session_key", "random-32-byte-key-here-xxxxx", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	expected := `writing SSM parameter "/dev/watchpoint/auth/session_key"`
	if got := err.Error(); got[:len(expected)] != expected {
		t.Errorf("error message = %q, want prefix %q", got, expected)
	}
}

func TestPutSecret_EmptyPath(t *testing.T) {
	mock := &mockSSMClient{}
	mgr := newTestSSMManager(mock, "dev")

	err := mgr.PutSecret(context.Background(), "", "some-value", false)
	if err == nil {
		t.Fatal("expected error for empty path, got nil")
	}
	if len(mock.putCalls) != 0 {
		t.Error("expected no SSM calls for empty path")
	}
}

func TestPutSecret_EmptyValue(t *testing.T) {
	mock := &mockSSMClient{}
	mgr := newTestSSMManager(mock, "dev")

	err := mgr.PutSecret(context.Background(), "/dev/watchpoint/database/url", "", false)
	if err == nil {
		t.Fatal("expected error for empty value, got nil")
	}
	if len(mock.putCalls) != 0 {
		t.Error("expected no SSM calls for empty value")
	}
}

// ---------------------------------------------------------------------------
// PutString tests
// ---------------------------------------------------------------------------

func TestPutString_Success(t *testing.T) {
	mock := &mockSSMClient{}
	mgr := newTestSSMManager(mock, "dev")

	err := mgr.PutString(context.Background(), "/dev/watchpoint/forecast/runpod_endpoint_id", "vllm-abc123xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 PutParameter call, got %d", len(mock.putCalls))
	}

	call := mock.putCalls[0]
	if aws.ToString(call.Name) != "/dev/watchpoint/forecast/runpod_endpoint_id" {
		t.Errorf("Name = %q, want %q", aws.ToString(call.Name), "/dev/watchpoint/forecast/runpod_endpoint_id")
	}
	if aws.ToString(call.Value) != "vllm-abc123xyz" {
		t.Errorf("Value = %q, want %q", aws.ToString(call.Value), "vllm-abc123xyz")
	}
	if call.Type != ssmtypes.ParameterTypeString {
		t.Errorf("Type = %v, want String", call.Type)
	}
	// PutString always uses overwrite=true
	if !aws.ToBool(call.Overwrite) {
		t.Error("Overwrite should be true for PutString")
	}
}

func TestPutString_StripePubKey(t *testing.T) {
	mock := &mockSSMClient{}
	mgr := newTestSSMManager(mock, "staging")

	err := mgr.PutString(context.Background(), "/staging/watchpoint/billing/stripe_publishable_key", "pk_test_abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call := mock.putCalls[0]
	if call.Type != ssmtypes.ParameterTypeString {
		t.Errorf("Type = %v, want String (Stripe publishable key is non-sensitive)", call.Type)
	}
}

func TestPutString_EmptyPath(t *testing.T) {
	mock := &mockSSMClient{}
	mgr := newTestSSMManager(mock, "dev")

	err := mgr.PutString(context.Background(), "", "some-value")
	if err == nil {
		t.Fatal("expected error for empty path, got nil")
	}
	if len(mock.putCalls) != 0 {
		t.Error("expected no SSM calls for empty path")
	}
}

func TestPutString_EmptyValue(t *testing.T) {
	mock := &mockSSMClient{}
	mgr := newTestSSMManager(mock, "dev")

	err := mgr.PutString(context.Background(), "/dev/watchpoint/forecast/runpod_endpoint_id", "")
	if err == nil {
		t.Fatal("expected error for empty value, got nil")
	}
	if len(mock.putCalls) != 0 {
		t.Error("expected no SSM calls for empty value")
	}
}

func TestPutString_APIError(t *testing.T) {
	mock := &mockSSMClient{
		putParameterFn: func(_ context.Context, _ *ssm.PutParameterInput) (*ssm.PutParameterOutput, error) {
			return nil, fmt.Errorf("internal server error")
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	err := mgr.PutString(context.Background(), "/dev/watchpoint/forecast/runpod_endpoint_id", "vllm-abc123xyz")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// NewSSMManager integration test (constructor)
// ---------------------------------------------------------------------------

func TestNewSSMManager(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	bctx := &BootstrapContext{
		Environment: "dev",
		AWSRegion:   "us-east-1",
		AWSConfig:   aws.Config{Region: "us-east-1"},
		Logger:      logger,
	}

	mgr := NewSSMManager(bctx)
	if mgr == nil {
		t.Fatal("NewSSMManager returned nil")
	}
	if mgr.env != "dev" {
		t.Errorf("env = %q, want %q", mgr.env, "dev")
	}
	if mgr.logger != logger {
		t.Error("logger not set correctly")
	}
	if mgr.client == nil {
		t.Error("client should not be nil")
	}
}

// ---------------------------------------------------------------------------
// Secret Inventory coverage
// ---------------------------------------------------------------------------

// TestSecretInventoryPaths verifies that SSMPath produces the correct paths
// for every entry in the Secret Inventory Table from 13-human-setup.md Section 4.
func TestSecretInventoryPaths(t *testing.T) {
	mock := &mockSSMClient{}
	mgr := newTestSSMManager(mock, "dev")

	inventory := []struct {
		label          string
		categoryAndKey string
		expectedPath   string
	}{
		{"Database URL", "database/url", "/dev/watchpoint/database/url"},
		{"Stripe Secret", "billing/stripe_secret_key", "/dev/watchpoint/billing/stripe_secret_key"},
		{"Stripe Public", "billing/stripe_publishable_key", "/dev/watchpoint/billing/stripe_publishable_key"},
		{"RunPod Key", "forecast/runpod_api_key", "/dev/watchpoint/forecast/runpod_api_key"},
		{"Google Secret", "auth/google_secret", "/dev/watchpoint/auth/google_secret"},
		{"GitHub Secret", "auth/github_secret", "/dev/watchpoint/auth/github_secret"},
		{"Session Key", "auth/session_key", "/dev/watchpoint/auth/session_key"},
		{"Admin API Key", "security/admin_api_key", "/dev/watchpoint/security/admin_api_key"},
	}

	for _, item := range inventory {
		t.Run(item.label, func(t *testing.T) {
			path := mgr.SSMPath(item.categoryAndKey)
			if path != item.expectedPath {
				t.Errorf("SSMPath(%q) = %q, want %q", item.categoryAndKey, path, item.expectedPath)
			}
		})
	}
}

// TestParameterTypeAssignment verifies that the correct SSM parameter type
// is used for each entry in the Parameter Type Matrix from 03-config.md
// Section 4.2 and the Secret Inventory Table from 13-human-setup.md.
func TestParameterTypeAssignment(t *testing.T) {
	// Track types used in put calls.
	var recordedType ssmtypes.ParameterType
	mock := &mockSSMClient{
		putParameterFn: func(_ context.Context, input *ssm.PutParameterInput) (*ssm.PutParameterOutput, error) {
			recordedType = input.Type
			return &ssm.PutParameterOutput{Version: 1}, nil
		},
	}
	mgr := newTestSSMManager(mock, "dev")
	ctx := context.Background()

	// SecureString entries
	secureEntries := []string{
		"/dev/watchpoint/database/url",
		"/dev/watchpoint/billing/stripe_secret_key",
		"/dev/watchpoint/forecast/runpod_api_key",
		"/dev/watchpoint/auth/google_secret",
		"/dev/watchpoint/auth/github_secret",
		"/dev/watchpoint/auth/session_key",
		"/dev/watchpoint/security/admin_api_key",
	}
	for _, path := range secureEntries {
		t.Run("SecureString:"+path, func(t *testing.T) {
			err := mgr.PutSecret(ctx, path, "test-value-xxxxx", false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if recordedType != ssmtypes.ParameterTypeSecureString {
				t.Errorf("PutSecret used Type=%v, want SecureString", recordedType)
			}
		})
	}

	// String entries
	stringEntries := []string{
		"/dev/watchpoint/billing/stripe_publishable_key",
		"/dev/watchpoint/forecast/runpod_endpoint_id",
	}
	for _, path := range stringEntries {
		t.Run("String:"+path, func(t *testing.T) {
			err := mgr.PutString(ctx, path, "test-value")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if recordedType != ssmtypes.ParameterTypeString {
				t.Errorf("PutString used Type=%v, want String", recordedType)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Context cancellation test
// ---------------------------------------------------------------------------

func TestParameterExists_ContextCancelled(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(ctx context.Context, _ *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return nil, ctx.Err()
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := mgr.ParameterExists(ctx, "/dev/watchpoint/database/url")
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestPutSecret_ContextCancelled(t *testing.T) {
	mock := &mockSSMClient{
		putParameterFn: func(ctx context.Context, _ *ssm.PutParameterInput) (*ssm.PutParameterOutput, error) {
			return nil, ctx.Err()
		},
	}
	mgr := newTestSSMManager(mock, "dev")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := mgr.PutSecret(ctx, "/dev/watchpoint/database/url", "some-value", false)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}
